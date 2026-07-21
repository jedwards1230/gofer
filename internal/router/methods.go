package router

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/wirestream"
)

// Send dispatches prompt as sessionID's next turn, forwarding to the owning
// worker's reconstruction core (which fires the session/prompt Call on its own
// goroutine — fire-and-forget). Send's ctx is ignored by the core by design; use
// Interrupt to stop an in-flight turn.
func (s *Supervisor) Send(ctx context.Context, sessionID, prompt string) error {
	h, ok := s.get(sessionID)
	if !ok {
		return fmt.Errorf("router: send %s: %w", sessionID, ErrNotLive)
	}
	if err := h.refuseNewWork("send"); err != nil {
		return fmt.Errorf("router: send %s: %w", sessionID, err)
	}
	return h.rec.Send(ctx, sessionID, prompt)
}

// refuseNewWork reports whether op — a request that gives the worker NEW work —
// must be refused because of h's version skew, returning [ErrWorkerSkewed] (with
// the observed versions) when it must.
//
// Only a WIRE mismatch refuses (see [skewClass.refusesNewWork]): the protocol
// itself is in doubt, so the router restricts the connection to the additive
// observe / permission-reply / finish subset design §6 guarantees across a
// version gap, and lets the in-flight turn end normally. A BINARY mismatch is
// NOT refused — see the package doc.
//
// Reading h.skew/h.wireVersion needs no lock: both are set before the handle is
// registered and never mutated (see [workerHandle]).
func (h *workerHandle) refuseNewWork(op string) error {
	if !h.skew.refusesNewWork() {
		return nil
	}
	return fmt.Errorf("%w: cannot %s a worker on wire v%d (router speaks v%d); the session may finish but takes no new work",
		ErrWorkerSkewed, op, h.wireVersion, daemon.WireVersion)
}

// SubscribeLive returns sessionID's reconstructed event stream WITHOUT the
// retained must-deliver backlog — the daemon's session/prompt handler drives a
// fresh turn off it. The session is always already referenced (Create called
// RegisterFresh), so this never first-references it and so never triggers a
// spurious history replay onto the live stream.
func (s *Supervisor) SubscribeLive(ctx context.Context, sessionID string) (*event.Subscription, error) {
	h, ok := s.get(sessionID)
	if !ok {
		return nil, fmt.Errorf("router: subscribe %s: %w", sessionID, ErrNotLive)
	}
	return h.rec.SubscribeLive(ctx, sessionID)
}

// Interrupt cancels sessionID's in-flight turn by forwarding session/cancel to
// its worker — a notification, per ACP.
//
// ctx is read exactly once, by the admission check below; the write's lifetime
// is owned by [daemon.Client.Notify], which takes no context and derives its own
// bound (see clientWriteTimeout) so a wedged worker socket cannot block the
// handler AND a caller cancellation cannot tear the shared worker link down.
// Interrupt is the likeliest trigger for that hazard in practice — Ctrl-C then
// quit cancels the peer request that carried the session/cancel — and borrowing
// its ctx for the write would have let the quit destroy the router's link to a
// still-healthy worker.
func (s *Supervisor) Interrupt(ctx context.Context, sessionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h, ok := s.get(sessionID)
	if !ok {
		return fmt.Errorf("router: interrupt %s: %w", sessionID, ErrNotLive)
	}
	if err := h.client.Notify(acp.MethodSessionCancel, acp.CancelNotification{SessionID: sessionID}); err != nil {
		return fmt.Errorf("router: interrupt %s: %w", sessionID, err)
	}
	return nil
}

// SetModel changes sessionID's model for its next turn by forwarding
// gofer/set_model to its worker. The worker validates the model (unknown /
// cross-provider rejections surface as the Call's application error) and, on an
// actual change, emits its own config_option_update — which the router
// reconstructs and re-fans, so clients track the new model without the router
// itself emitting anything (see EmitConfigOptions).
//
// ctx is read exactly once, by the admission check below — the handle lookup
// and the skew refusal that follow take no context — while the write runs under
// an owned bound (see [wireCallCtx]). A borrow here is the most DAMAGING of the
// four: the peer whose ctx it would be is by definition mid-model-change on a
// session that may be running, so its cancellation would kill the worker link
// under a live turn.
func (s *Supervisor) SetModel(ctx context.Context, sessionID, model string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h, ok := s.get(sessionID)
	if !ok {
		return fmt.Errorf("router: set model %s: %w", sessionID, ErrNotLive)
	}
	// A model change is NEW WORK — it configures the worker's next turn — so it
	// is refused on wire skew exactly like a prompt.
	if err := h.refuseNewWork("set the model on"); err != nil {
		return fmt.Errorf("router: set model %s: %w", sessionID, err)
	}
	cctx, cancel := wireCallCtx()
	defer cancel()
	params := map[string]string{"sessionId": sessionID, "model": model}
	if _, err := h.client.Call(cctx, methodGoferSetModel, params); err != nil {
		return fmt.Errorf("router: set model %s: %w", sessionID, err)
	}
	return nil
}

// Reply answers a pending permission request by forwarding permission.reply to
// the owning worker as a bare notification. The write's lifetime is owned by
// [daemon.Client.Notify], which takes no context and derives its own bound (see
// clientWriteTimeout), so a wedged worker socket cannot block the reply forever.
// The op carries no session id (the worker resolves the request by call id at
// its own gate), but the router still looks the handle up by sessionID to reach
// the right worker's connection.
func (s *Supervisor) Reply(sessionID string, op event.PermissionReply) error {
	h, ok := s.get(sessionID)
	if !ok {
		return fmt.Errorf("router: reply %s: %w", sessionID, ErrNotLive)
	}
	params := struct {
		ID       string        `json:"id"`
		Verdict  event.Verdict `json:"verdict"`
		Remember bool          `json:"remember,omitempty"`
	}{ID: op.ID, Verdict: op.Verdict, Remember: op.Remember}
	if err := h.client.Notify(methodPermissionReply, params); err != nil {
		return fmt.Errorf("router: reply %s: %w", sessionID, err)
	}
	return nil
}

// EmitConfigOptions is unsupported in worker mode: no wire method lets a client
// make a worker emit an arbitrary config-options snapshot, and it is off the
// crash-isolation critical path (see [ErrEmitConfigUnsupported]). The daemon's
// advertiseModelChange treats this error as non-fatal, and the live
// config_option_update a model swap actually produces still reaches clients — the
// WORKER emits it and the router reconstructs it (see [Supervisor.SetModel]).
func (s *Supervisor) EmitConfigOptions(string, []event.ConfigOption) error {
	return ErrEmitConfigUnsupported
}

// Resume brings a session back under this router's live supervision. It has two
// paths:
//
//   - LIVE ATTACH — the router already hosts the session (adopted or created).
//     Resume returns a minimal live snapshot so the daemon's session/load handler
//     succeeds and registers the calling peer in the session's fan-out set. That
//     attach path is what lets a client of a restarted router SEE and answer an
//     adopted session's re-surfaced permission (design §7): handleSessionLoad only
//     needs Resume to succeed (it reads History and replays pending permissions
//     separately), so a snapshot carrying just the id + Live is sufficient.
//   - OFFLINE RESUME — the session has an on-disk journal but no live worker (it
//     crashed, was killed, or the router restarted and could not adopt it). Resume
//     SPAWNS a fresh worker for the id and rebuilds it from the journal by issuing
//     session/load on the worker (see [Supervisor.resumeOffline]).
//
// A genuinely unknown id — no live handle AND no journal — returns an error
// wrapping [session.ErrSessionNotFound], which the daemon surfaces as a clean
// application error rather than spawning a worker over nothing.
func (s *Supervisor) Resume(ctx context.Context, id string, opts supervisor.ResumeOptions) (supervisor.SessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return supervisor.SessionInfo{}, err
	}
	if _, ok := s.get(id); ok {
		return supervisor.SessionInfo{ID: id, Live: true}, nil
	}
	// Offline: confirm a journal exists before forking anything, so an unknown id
	// is a clean not-found rather than a spawned-then-empty worker. The read uses
	// a throwaway store closed immediately — the same disk-read path
	// [Supervisor.History]/[Supervisor.List] use — and never conflicts with the
	// worker's own open, which happens after this returns.
	if err := s.confirmJournal(ctx, id); err != nil {
		return supervisor.SessionInfo{}, err
	}
	return s.resumeOffline(ctx, id, opts)
}

// confirmJournal reports whether id has an on-disk journal, returning an error
// wrapping [session.ErrSessionNotFound] when it does not (a genuinely unknown id)
// and a wrapped open error for any other read failure.
func (s *Supervisor) confirmJournal(ctx context.Context, id string) error {
	store, err := session.NewFileStore(session.WithRoot(s.root))
	if err != nil {
		return fmt.Errorf("router: resume %s: open store: %w", id, err)
	}
	defer func() { _ = store.Close() }()
	if _, err := store.Open(ctx, id); err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			return fmt.Errorf("router: resume %s: %w", id, session.ErrSessionNotFound)
		}
		return fmt.Errorf("router: resume %s: open journal: %w", id, err)
	}
	return nil
}

// resumeOffline spawns a fresh worker for an OFFLINE session id and rebuilds it
// from the on-disk journal, then registers the live handle — the offline half of
// [Supervisor.Resume]. It reuses [Supervisor.spawnWorker] (the exact bring-up
// [Supervisor.Create] uses) and diverges only in seeding the worker: instead of
// session/new it issues session/load, which drives the worker's own
// handleSessionLoad → in-process supervisor.Resume to reopen the persisted
// session and replay its history.
//
// # Why the replay must not reach clients (the crux)
//
// The worker replays the journal to this router as gofer/event frames, and the
// handle's reconstruction core fires its [wirestream.EventSink] for every one —
// which normally re-broadcasts to attached clients ([Supervisor.SetEventRelay]).
// But Resume runs WHILE the daemon serves, with the relay installed and a client
// driving this very session/load: those replayed history frames are NOT new
// events, and the resuming client already receives the transcript once from the
// daemon's own handleSessionLoad History replay. Re-broadcasting them through the
// sink would DOUBLE the transcript for every already-attached peer.
//
// So resumeOffline SUPPRESSES the sink for the whole replay: it sets a per-resume
// guard the sink consults ([Supervisor.eventSink]'s replaySuppressed) BEFORE
// triggering the load, and clears it only AFTER the load has fully settled.
//
// # Why clearing the guard has no lost-event race
//
// [wirestream.Reconstructor.Load] blocks until the demuxer goroutine has DRAINED
// and applied every notification the session/load replayed and closed the
// session's loadDone (see wirestream's loadHistory ordering proof). The sink
// fires on that same demuxer goroutine. So by the time Load returns, every replay
// frame has already passed through the (suppressed) sink and been dropped — no
// replay frame can escape after the guard clears.
//
// In the other direction, no LIVE frame can be dropped by clearing late: the
// worker was just spawned and loaded to an IDLE session, and it emits no event of
// its own until it is prompted. That first prompt is the resuming client's, which
// it cannot send until this session/load completes — i.e. until Resume returns,
// strictly after the guard is cleared. drainNotifications also empties the
// notification buffer before loadDone closes, so nothing live is left in flight.
// The window between "load settled" and "guard cleared" therefore carries no
// frame at all.
func (s *Supervisor) resumeOffline(ctx context.Context, id string, opts supervisor.ResumeOptions) (supervisor.SessionInfo, error) {
	model := opts.Model
	if model == "" {
		model = s.model
	}

	// Admission: reserve a spawn slot and claim the id against a concurrent
	// same-id resume, all under one lock. A router at capacity, closed, or already
	// resuming this id is refused here before anything is forked.
	proceed, snap, err := s.admitResume(id)
	if err != nil {
		return supervisor.SessionInfo{}, err
	}
	if !proceed {
		// The session raced live (another resume won, or an adopt landed) between
		// the top-of-Resume check and the admission lock — return its snapshot.
		return snap, nil
	}
	defer s.finishResume(id)
	reserved := true
	defer func() {
		if reserved {
			s.releaseSlot()
		}
	}()

	// GC any stale endpoint/socket a crashed or killed prior worker left behind
	// for this id (the reaper drops the handle but leaves the residue, so
	// adoption/List can still see it). Without this, the fresh worker's
	// endpoint-file discovery would find the STALE file first and dial a dead
	// socket. If a live process still owns the id — an un-adopted worker, rare —
	// refuse rather than fork a duplicate that would only collide on the
	// <uuid>.lock; the ProcessAlive probe mirrors adoption's step-1 liveness check
	// (and inherits its documented, bounded pid-reuse caveat — see killHandleProcess).
	if ep, epErr := daemon.ReadWorkerEndpoint(id); epErr == nil {
		if daemon.ProcessAlive(ep.PID) {
			return supervisor.SessionInfo{}, fmt.Errorf("router: resume %s: a worker process (pid %d) still owns this session; not spawning a duplicate", id, ep.PID)
		}
		removeWorkerArtifacts(id)
	}

	sw, err := s.spawnWorker(ctx, id, model, opts.Cwd)
	if err != nil {
		return supervisor.SessionInfo{}, fmt.Errorf("router: resume %s: %w", id, err)
	}

	// Build the reconstruction core with a REPLAY GUARD wired into its sink, so
	// the journal replay the load triggers is suppressed for attached clients (see
	// this method's doc for why, and why clearing it after the load has no race).
	var replaySuppressed atomic.Bool
	rec := wirestream.New(sw.client, wirestream.WithEventSink(s.eventSink(id, &replaySuppressed)))

	// Suppress the sink, then drive the worker's session/load. Load blocks until
	// the whole replay has settled onto the reconstructed broker; only then is the
	// guard cleared. The bound is OWNED, not the resuming peer's ctx (see
	// [wireCallCtx]) — a peer that hangs up mid-load must not destroy the worker
	// link. A load that does not settle is non-fatal: the session is live, its
	// history is on disk, and the daemon's handleSessionLoad replays it to the
	// client regardless (matching adoption's live-only fallback).
	replaySuppressed.Store(true)
	loadCtx, loadCancel := wireCallCtx()
	lerr := rec.Load(loadCtx, id)
	loadCancel()
	replaySuppressed.Store(false)
	if lerr != nil {
		s.log.Warn("resume: history load did not settle; resuming live-only", "session", id, "err", lerr)
	}

	h := newWorkerHandle(id, sw.cmd, sw.client, rec, sw.pid, sw.wait, sw.hello, sw.skew)
	if registered, closed := s.registerWorker(h); !registered {
		_ = rec.Close()
		cleanupSpawnedWorker(id, sw.cmd, sw.wait)
		if closed {
			return supervisor.SessionInfo{}, fmt.Errorf("router: resume %s: %w", id, ErrNotLive)
		}
		// Unreachable in practice — the resuming guard makes this id's spawn
		// exclusive, and Create draws fresh uuids — but if a handle for the id
		// somehow appeared, prefer the live one over clobbering it.
		return supervisor.SessionInfo{ID: id, Live: true}, nil
	}
	reserved = false

	s.log.Info("worker resumed", "session", id, "addr", sw.ep.Addr, "pid", sw.pid)
	now := time.Now()
	return supervisor.SessionInfo{
		ID:            id,
		Model:         model,
		Cwd:           opts.Cwd,
		Status:        supervisor.StatusNeedsInput,
		Created:       now,
		Updated:       now,
		Live:          true,
		BinaryVersion: sw.hello.BinaryVersion,
	}, nil
}

// admitResume is [Supervisor.resumeOffline]'s spawn-admission gate, run before
// any process is forked. Under one lock hold it refuses a closed router
// ([ErrNotLive]), returns the live snapshot if the session raced live
// (proceed=false, a benign no-op for the caller), refuses a concurrent resume of
// the same id ([ErrResumeInProgress]), and enforces the [Config.MaxWorkers] cap
// ([ErrAtCapacity]) — occupancy counting live handles plus in-flight spawns, the
// same resource the cap protects for Create. On success it RESERVES both a worker
// slot (pending++) and the id (resuming), which [Supervisor.finishResume] and the
// caller's releaseSlot/registration release.
func (s *Supervisor) admitResume(id string) (proceed bool, snapshot supervisor.SessionInfo, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false, supervisor.SessionInfo{}, fmt.Errorf("router: resume %s: %w", id, ErrNotLive)
	}
	if _, ok := s.workers[id]; ok {
		// Raced live between the top-of-Resume get and this lock.
		return false, supervisor.SessionInfo{ID: id, Live: true}, nil
	}
	if _, ok := s.resuming[id]; ok {
		return false, supervisor.SessionInfo{}, fmt.Errorf("router: resume %s: %w", id, ErrResumeInProgress)
	}
	if live := len(s.workers) + s.pending; s.maxWorkers > 0 && live >= s.maxWorkers {
		return false, supervisor.SessionInfo{}, fmt.Errorf("router: resume %s: %w (%d live, max %d)", id, ErrAtCapacity, live, s.maxWorkers)
	}
	s.resuming[id] = struct{}{}
	s.pending++
	return true, supervisor.SessionInfo{}, nil
}

// finishResume drops id's in-flight-resume claim taken by [Supervisor.admitResume].
// The pending reservation is released separately (registration consumes it, or the
// caller's releaseSlot returns it on failure), so this only clears the id guard.
func (s *Supervisor) finishResume(id string) {
	s.mu.Lock()
	delete(s.resuming, id)
	s.mu.Unlock()
}

// Roster aggregates every live worker's roster row into the daemon's expected
// snapshot type, each row marked Live.
//
// It serves from the PUSHED CACHE (rostercache.go): each handle's row was seeded
// once from its worker and is maintained from the event stream this router
// already subscribes to, so the steady-state cost of a roster read is ZERO worker
// RPCs — a lock-free [atomic.Pointer] load per handle.
//
// The point is AVAILABILITY, not throughput. The old path issued one RPC per
// live worker SERIALLY, each bounded by [wireCallTimeout] (15s), so a single
// wedged worker stalled every Roster call — and so every `gofer ps`, every
// [Supervisor.List] and the TUI's ~1Hz roster poll — for up to fifteen seconds.
// An atomic load cannot be held hostage that way.
//
// A handle with NO cached row falls back to a live RPC for that handle alone.
// That is the degraded path, not the normal one — it means the seed failed or has
// not landed yet — and it keeps a struggling worker visible in the roster instead
// of vanishing from it. A worker whose fallback call also fails is skipped rather
// than failing the whole roster: crash isolation extends to observation, and the
// dead session reappears offline via List.
func (s *Supervisor) Roster(ctx context.Context) ([]supervisor.SessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []supervisor.SessionInfo
	for _, h := range s.snapshotHandles() {
		// The happy path: an immutable snapshot published by this handle's
		// watchSession goroutine. No lock, no RPC, no copy beyond the value.
		if info := h.info.Load(); info != nil {
			out = append(out, *info)
			continue
		}
		// The degraded path issues a REAL gofer/roster Call on this handle's
		// shared worker link, so — like every other router→worker write — it runs
		// under an owned bound rather than the reading peer's ctx (see
		// [wireCallCtx]), the same helper the seed path uses. Otherwise
		// a client that hangs up mid-`gofer ps` could destroy the link to a worker
		// whose only sin was not having published its first roster row yet.
		rctx, cancel := wireCallCtx()
		rows, err := h.rec.Roster(rctx)
		cancel()
		if err != nil {
			s.log.Debug("roster: uncached worker unreachable, skipping", "session", h.id, "err", err)
			continue
		}
		for _, r := range rows {
			out = append(out, toSupervisorInfo(r, h.binaryVersion))
		}
	}
	return out, nil
}

// List returns the union of live workers ∪ on-disk journals: live sessions from
// the aggregated roster, every other on-disk session as an offline (Live=false)
// entry read from its journal. This union is what makes a crashed session — whose
// worker is gone but whose journal remains — show up as offline. It mirrors
// [supervisor.Supervisor.List]'s disk-enumeration approach (os.ReadDir over the
// project dirs + store.List per slug + a read-only journal fold for metadata),
// linking the SDK session package for reads only, never the runner/loop.
func (s *Supervisor) List(ctx context.Context) ([]supervisor.SessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	live := s.liveInfoByID(ctx)

	sessionsDir := filepath.Join(s.root, "sessions")
	des, err := os.ReadDir(sessionsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No journals on disk yet — return whatever is live.
			return mapValues(live), nil
		}
		return nil, fmt.Errorf("router: list %s: %w", sessionsDir, err)
	}

	seen := make(map[string]struct{}, len(live))
	var out []supervisor.SessionInfo
	for _, de := range des {
		if !de.IsDir() {
			continue
		}
		slug := de.Name()
		ids, err := s.store.List(ctx, slug)
		if err != nil {
			return nil, fmt.Errorf("router: list project %s: %w", slug, err)
		}
		for _, id := range ids {
			seen[id] = struct{}{}
			if info, ok := live[id]; ok {
				out = append(out, info)
				continue
			}
			path := filepath.Join(sessionsDir, slug, id+".jsonl")
			out = append(out, diskSessionInfo(id, slug, path))
		}
	}
	// A live session whose journal is not on disk yet (a just-spawned worker
	// mid-first-write) still belongs in the list.
	for id, info := range live {
		if _, ok := seen[id]; !ok {
			out = append(out, info)
		}
	}
	return out, nil
}

// History returns id's folded conversation from disk — the durable truth, read
// the same for a live or offline session (never asked of the worker). It opens
// the journal through a THROWAWAY store so the fold always reflects the latest
// on-disk state (a long-lived store would serve a cached, stale fold for a live
// session the worker is still appending to); the store is closed on return,
// releasing the read handle.
func (s *Supervisor) History(ctx context.Context, id string) ([]provider.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	store, err := session.NewFileStore(session.WithRoot(s.root))
	if err != nil {
		return nil, fmt.Errorf("router: history %s: open store: %w", id, err)
	}
	defer func() { _ = store.Close() }()

	j, err := store.Open(ctx, id)
	if err != nil {
		// A LIVE session whose journal is not on disk yet (a just-adopted or
		// just-spawned worker that has not written its first entry) has no folded
		// history to replay — return empty rather than failing session/load's
		// attach, which §7 needs to succeed for an adopted session so a client can
		// see and answer its re-surfaced permission. An OFFLINE id with no journal
		// is a genuine not-found and still errors.
		if _, live := s.get(id); live && errors.Is(err, session.ErrSessionNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("router: history %s: %w", id, err)
	}
	return j.Fold(), nil
}

// AwaitSettled blocks until id's owning worker reports its session idle
// ([supervisor.StatusNeedsInput] in the pushed roster cache), or returns nil at
// once when there is no live handle — an OFFLINE session's on-disk journal has
// no live writer and is already durable. It is the router half of the issue #137
// fix: a worker journals a turn's assistant/tool entries ASYNCHRONOUSLY after the
// turn.finished event a client observes, so a session/load adopting that worker
// mid-flush would read a SHORT history without this wait.
//
// It observes the cached row's Status — maintained by this handle's watchSession
// goroutine from the worker's reconstructed event stream (see [workerHandle.applyRosterEvent])
// — and blocks on the handle's settleCh, which that same goroutine pokes after
// folding each event, rather than polling. It is BEST-EFFORT: the caller
// (handleSessionLoad) bounds it via ctx, a ctx deadline returns ctx.Err() for the
// caller to treat as "fold whatever is durable", and an unseeded row (nil cache,
// the degraded path) returns nil rather than blindly waiting — so a worker
// blocked mid-turn (design §7) never deadlocks the load.
func (s *Supervisor) AwaitSettled(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h, ok := s.get(id)
	if !ok {
		// Offline / no live worker: the journal is durable, nothing to settle.
		return nil
	}
	for {
		info := h.info.Load()
		if info == nil || info.Status == supervisor.StatusNeedsInput {
			// nil: the roster cache never seeded (degraded path) — we cannot observe
			// settledness, so don't block the load; ctx still bounds it regardless.
			return nil
		}
		select {
		case <-h.settleCh:
			// Re-read the authoritative status at the top of the loop.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Kill terminates sessionID's worker (keeping its journal). It first asks the
// worker to emit session.killed (gofer/kill, best-effort so attached peers see a
// clean terminal event), then SIGKILLs the now-empty single-session worker
// process — a worker daemon does not exit merely because its one session was
// killed — and lets the reaper drop the handle and reconcile.
//
// The teardown is UNCONDITIONAL: the process is killed and the handle reaped on
// every path, including when the gofer/kill reply errors or its bound expires.
// A failing reply is reported to the caller AFTER that teardown, matching the
// in-process supervisor, whose Kill likewise stops the session and only then
// returns its Close error (see [supervisor.Supervisor.Kill]). A non-nil error
// therefore means "the worker reported a problem while ending the session", NOT
// "the session may still be live" — every caller that treats it as advisory
// (the daemon's gofer/kill handler surfaces it to the client; the TUI shows it)
// stays correct.
//
// ctx is read exactly once, by the admission check below; neither the handle
// lookup nor the SIGKILL takes a context, and the write runs under an owned
// bound (see [wireCallCtx]).
func (s *Supervisor) Kill(ctx context.Context, sessionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h, ok := s.take(sessionID)
	if !ok {
		return fmt.Errorf("router: kill %s: %w", sessionID, ErrNotLive)
	}
	kctx, cancel := wireCallCtx()
	_, callErr := h.client.Call(kctx, methodGoferKill, map[string]string{"sessionId": sessionID})
	cancel()
	// Terminal-event race (accepted, best-effort): gofer/kill's Call returns
	// when the worker ACKs, but the session.killed it emits travels as an async
	// gofer/event notification. Killing the process immediately can drop that
	// frame before it is reconstructed for attached peers — who then observe the
	// socket-close terminal error instead. Either way peers see a terminal
	// event; a drain/settle before the kill would tighten it but is not required
	// for this slice.
	killHandleProcess(h)
	if callErr != nil {
		// Report, never abort: the handle is already taken and the process is
		// already signalled above, so this return is pure signal — it can never
		// leave a live worker behind a caller who believes it dead. (This is why
		// Kill must NOT copy Archive's return-before-teardown shape: Archive
		// bails to keep a rejected session live, which for Kill would strand a
		// process.) The reply is the operator's only channel for a worker-side
		// failure to finish the session; the in-process daemon surfaced it, so
		// dropping it here would be a worker-mode regression.
		return fmt.Errorf("router: kill %s: %w", sessionID, callErr)
	}
	return nil
}

// Archive drops sessionID from the live set, keeping its journal. If a worker is
// live, it asks the worker to archive (emitting session.archived; the worker
// rejects a running session, surfaced as the Call error) and then terminates the
// now-empty worker. An offline session (no live worker) is already retired from
// the live set — its journal persists — so archiving it is an idempotent no-op.
//
// ctx is read exactly once, by the admission check below; the write runs under
// an owned bound (see [wireCallCtx]).
func (s *Supervisor) Archive(ctx context.Context, sessionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h, ok := s.get(sessionID)
	if !ok {
		// Offline: nothing live to drop; the journal already persists.
		return nil
	}
	actx, cancel := wireCallCtx()
	_, err := h.client.Call(actx, methodGoferArchive, map[string]string{"sessionId": sessionID})
	cancel()
	if err != nil {
		// The worker rejected the archive (e.g. the session is still running):
		// leave the handle live and surface the error, matching the in-process
		// supervisor's reject-if-busy contract.
		return fmt.Errorf("router: archive %s: %w", sessionID, err)
	}
	// Archived on the worker; terminate the now-empty worker and let the reaper
	// drop the handle. The get-then-take split (peek before the RPC, remove
	// after) is deliberate: it keeps the handle LIVE if the archive Call is
	// rejected above (reject-if-busy), which a single take-first could not.
	// The gap between get and take is a benign race — if the worker crashed (or
	// a concurrent Kill/Archive fired) in between, its reaper already removed
	// the handle, so take returns taken=false and this simply skips a
	// now-pointless Kill. One session maps to one handle for its lifetime, so
	// take never returns a DIFFERENT worker than get observed.
	if hh, taken := s.take(sessionID); taken {
		killHandleProcess(hh)
	}
	return nil
}

// liveInfoByID snapshots the live roster into a by-id map for List's overlay.
func (s *Supervisor) liveInfoByID(ctx context.Context) map[string]supervisor.SessionInfo {
	infos, err := s.Roster(ctx)
	if err != nil {
		return nil
	}
	out := make(map[string]supervisor.SessionInfo, len(infos))
	for _, info := range infos {
		out[info.ID] = info
	}
	return out
}

// mapValues flattens a by-id info map to a slice (order unspecified — the daemon
// sorts).
func mapValues(m map[string]supervisor.SessionInfo) []supervisor.SessionInfo {
	if len(m) == 0 {
		return nil
	}
	out := make([]supervisor.SessionInfo, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// toSupervisorInfo translates one reconstructed wire roster row into the daemon's
// snapshot type, marked Live (it came from a live worker). Status is carried as a
// string on the wire so the enums can drift independently; statusFromWire maps it
// back.
//
// binaryVersion is stamped by the ROUTER from the owning handle's gofer/hello
// result, not read off the row: a worker's own roster reports the sessions IT
// hosts and has no reason to know it is being proxied, so the version knowledge
// lives with the router's handle. This is what lets session/list show mixed
// binary versions while a daemon upgrade drains old workers (design §11 Phase 3).
func toSupervisorInfo(d wirestream.SessionInfo, binaryVersion string) supervisor.SessionInfo {
	return supervisor.SessionInfo{
		BinaryVersion: binaryVersion,
		ID:            d.ID,
		Title:         d.Title,
		Status:        statusFromWire(d.Status),
		Model:         d.Model,
		Cost:          d.Cost,
		Usage:         d.Usage,
		Pending:       d.Pending,
		Queued:        d.Queued,
		Created:       d.Created,
		Updated:       d.Updated,
		Project:       d.Project,
		Live:          true,
		Cwd:           d.Cwd,
	}
}

// statusFromWire maps the daemon's roster Status string (literally
// [supervisor.SessionStatus.String]'s output) back to the enum. An unrecognized
// value falls back to StatusNeedsInput rather than the zero-value StatusWorking,
// so a wire/enum drift never makes an idle session look busy — mirroring
// internal/daemonbridge's statusFromWire.
func statusFromWire(s string) supervisor.SessionStatus {
	switch s {
	case "working":
		return supervisor.StatusWorking
	case "finished":
		return supervisor.StatusFinished
	default:
		return supervisor.StatusNeedsInput
	}
}

// diskSessionInfo builds an offline [supervisor.SessionInfo] for id from its
// journal, read-only via [session.ReadEntries] — the same enrichment
// [supervisor.Supervisor.List] applies to a disk-only entry: Cwd from the meta
// root entry, Title from the first user message, Created/Updated from the first
// and last entry times. A read error or an empty journal degrades to the bare
// {ID, Project, JournalPath, Live:false} snapshot rather than failing List.
func diskSessionInfo(id, slug, path string) supervisor.SessionInfo {
	info := supervisor.SessionInfo{
		ID:          id,
		Project:     slug,
		JournalPath: path,
		Live:        false,
	}

	entries, err := session.ReadEntries(path)
	if err != nil || len(entries) == 0 {
		return info
	}

	info.Created = entries[0].Time
	info.Updated = entries[len(entries)-1].Time

	if entries[0].Type == session.EntryMeta {
		if meta, metaErr := entries[0].Meta(); metaErr == nil {
			info.Cwd = meta.Cwd
		}
	}

	for _, e := range entries {
		if e.Type != session.EntryMessage {
			continue
		}
		msg, msgErr := e.Message()
		if msgErr != nil || msg.Role != provider.RoleUser {
			continue
		}
		if text := msg.Text(); text != "" {
			info.Title = snippet(text)
			break
		}
	}

	return info
}

// snippetMax bounds a disk-only session's derived title.
const snippetMax = 80

// snippet renders the first line of text, trimmed and truncated to snippetMax
// runes, as an offline session's title — the router-local mirror of the
// supervisor's own title derivation.
func snippet(text string) string {
	text = strings.TrimSpace(text)
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = strings.TrimSpace(text[:i])
	}
	r := []rune(text)
	if len(r) > snippetMax {
		return string(r[:snippetMax-1]) + "…"
	}
	return text
}
