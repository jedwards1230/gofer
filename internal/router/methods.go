package router

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
// its worker — a notification, per ACP. The bounded context keeps a wedged
// worker socket from blocking the handler.
//
// ctx is read exactly once, by the admission check below; the write runs under
// an owned bound (see [wireCallCtx]). Interrupt is the likeliest trigger for
// that hazard in practice — Ctrl-C then quit cancels the peer request that
// carried the session/cancel — and borrowing here would have let the quit
// destroy the router's link to a still-healthy worker.
func (s *Supervisor) Interrupt(ctx context.Context, sessionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	h, ok := s.get(sessionID)
	if !ok {
		return fmt.Errorf("router: interrupt %s: %w", sessionID, ErrNotLive)
	}
	cctx, cancel := wireCallCtx()
	defer cancel()
	if err := h.client.Notify(cctx, acp.MethodSessionCancel, acp.CancelNotification{SessionID: sessionID}); err != nil {
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
// the owning worker as a bare notification. It takes no context in the interface
// signature, so it derives a BOUNDED one from context.Background — a wedged
// worker socket must not block the reply forever. The op carries no session id
// (the worker resolves the request by call id at its own gate), but the router
// still looks the handle up by sessionID to reach the right worker's connection.
func (s *Supervisor) Reply(sessionID string, op event.PermissionReply) error {
	h, ok := s.get(sessionID)
	if !ok {
		return fmt.Errorf("router: reply %s: %w", sessionID, ErrNotLive)
	}
	ctx, cancel := context.WithTimeout(context.Background(), replyCallTimeout)
	defer cancel()
	params := struct {
		ID       string        `json:"id"`
		Verdict  event.Verdict `json:"verdict"`
		Remember bool          `json:"remember,omitempty"`
	}{ID: op.ID, Verdict: op.Verdict, Remember: op.Remember}
	if err := h.client.Notify(ctx, methodPermissionReply, params); err != nil {
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

// Resume attaches to a session this router already hosts LIVE (adopted or
// created): it returns a minimal live snapshot so the daemon's session/load
// handler succeeds and registers the calling peer in the session's fan-out set.
// That attach path is what lets a client of a restarted router SEE and answer an
// adopted session's re-surfaced permission (design §7): handleSessionLoad only
// needs Resume to succeed (it reads History and replays pending permissions
// separately), so a snapshot carrying just the id + Live is sufficient.
//
// Spawning a FRESH worker for an OFFLINE (or old-binary) session — the other
// meaning of resume — remains Phase 4: an id with no live handle returns
// [ErrResumeUnsupported], which the daemon surfaces as a clean application error.
func (s *Supervisor) Resume(ctx context.Context, id string, _ supervisor.ResumeOptions) (supervisor.SessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return supervisor.SessionInfo{}, err
	}
	if _, ok := s.get(id); ok {
		return supervisor.SessionInfo{ID: id, Live: true}, nil
	}
	return supervisor.SessionInfo{}, ErrResumeUnsupported
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

// Kill terminates sessionID's worker (keeping its journal). It first asks the
// worker to emit session.killed (gofer/kill, best-effort so attached peers see a
// clean terminal event), then SIGKILLs the now-empty single-session worker
// process — a worker daemon does not exit merely because its one session was
// killed — and lets the reaper drop the handle and reconcile.
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
	_, _ = h.client.Call(kctx, methodGoferKill, map[string]string{"sessionId": sessionID})
	cancel()
	// Terminal-event race (accepted, best-effort): gofer/kill's Call returns
	// when the worker ACKs, but the session.killed it emits travels as an async
	// gofer/event notification. Killing the process immediately can drop that
	// frame before it is reconstructed for attached peers — who then observe the
	// socket-close terminal error instead. Either way peers see a terminal
	// event; a drain/settle before the kill would tighten it but is not required
	// for this slice.
	killHandleProcess(h)
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
