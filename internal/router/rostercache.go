package router

import (
	"context"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// rostercache.go implements the PUSH-based roster (M6 §8's "session/list" row,
// slice 3b). Before it, every [Supervisor.Roster] — and so every
// [Supervisor.List], every `gofer ps`, every TUI roster tick — issued one
// gofer/roster RPC PER LIVE WORKER, on the request's own goroutine. That cost
// scales with (sessions × poll rate) and puts a wedged worker socket on the
// latency path of an operator listing an unrelated session.
//
// The router already subscribes to every worker's reconstructed event stream
// (see [Supervisor.watchSession]), and a session's roster row is a projection of
// that stream. So the row is CACHED and pushed: seeded once per handle from a
// single gofer/roster call, then maintained from events. Roster serves from the
// cache; List unions that with disk. Steady state costs ZERO worker RPCs.
//
// # Concurrency model
//
// One snapshot per handle, published as an IMMUTABLE value behind
// [atomic.Pointer]: the handle's own watchSession goroutine is the SOLE writer
// (it seeds, then applies each event to a COPY and stores the copy), and every
// reader — Roster, List, and their callers — does a lock-free Load. No reader
// ever observes a half-updated row, and no roster read can ever block on the
// writer.
//
// # No TTL, by design
//
// A cached row has no expiry. Its lifetime IS the handle's lifetime: the row
// lives on the handle, so [Supervisor.reap] (crash) and [Supervisor.take]
// (Kill/Archive) are the ONLY evictors. That is the point — a TTL would be a
// second, independent liveness signal that could drift out of sync with the one
// that actually decides whether a session is live, producing a row that expired
// while its worker is fine or one that outlived its worker. Staleness is
// observable WITHOUT a TTL and without a new field:
// [supervisor.SessionInfo.Updated] already carries the snapshot's own time.
//
// If the reconstructed broker closes while the handle still lingers (the worker
// died; the reaper has not run yet), the watcher exits and the last-known
// snapshot keeps serving — correct, because that row is about to be reaped.
//
// # What the stream can and cannot answer
//
//   - Status, Title, Cost, Usage, Pending and Updated are maintained from
//     events; see [workerHandle.applyRosterEvent].
//   - Queued (the worker pump's queue depth) has NO event, so it keeps its
//     seeded value. It is a display-only field, and gofer's own clients prompt
//     only an idle session, so it is ~always 0. Documented rather than faked.
//   - Cost/Usage accumulate turn.finished deltas onto the seeded totals. The one
//     seam: a turn.finished landing between the seed RPC's snapshot on the worker
//     and the seed being applied here can be counted twice (once in the seed,
//     once as a delta). It is bounded to one turn of a display field, on an
//     adopted session only, and the alternative — a per-call RPC — is exactly what
//     this cache removes.

// seedRosterCache primes h's cached roster row with ONE gofer/roster call to its
// worker — the only worker RPC this cache ever makes on the happy path. It runs
// on h's watchSession goroutine (the cache's single writer) AFTER that goroutine
// has already subscribed, so events published during the call are queued on the
// subscription and applied on top of the seed rather than lost.
//
// A failed or row-less seed leaves the cache EMPTY on purpose: a nil snapshot is
// [Supervisor.Roster]'s cache-miss signal, which falls back to a live RPC for
// that handle. A degraded worker therefore degrades to the pre-cache behavior
// instead of vanishing from the roster.
func (s *Supervisor) seedRosterCache(h *workerHandle) {
	defer h.markSeeded()

	ctx, cancel := context.WithTimeout(context.Background(), wireCallTimeout)
	rows, err := h.rec.Roster(ctx)
	cancel()
	if err != nil {
		s.log.Debug("roster cache: seed failed; falling back to per-call RPCs", "session", h.id, "err", err)
		return
	}
	for _, r := range rows {
		if r.ID != h.id {
			continue
		}
		info := toSupervisorInfo(r, h.binaryVersion)
		h.info.Store(&info)
		return
	}
	s.log.Debug("roster cache: worker reported no row for its own session", "session", h.id)
}

// markSeeded closes h.seeded exactly once, whether the seed succeeded, failed,
// or was skipped (the watcher never started). Callers wait on h.seeded to know
// the cache has settled; leaving it open on a failure path would hang them.
func (h *workerHandle) markSeeded() {
	h.seedOnce.Do(func() { close(h.seeded) })
}

// applyRosterEvent folds one reconstructed event into h's cached roster row. It
// runs ONLY on h's watchSession goroutine (the cache's single writer): it copies
// the current immutable snapshot, mutates the copy, and publishes it. A nil
// current snapshot means the seed failed — there is no baseline to fold onto, and
// synthesizing one would report a row with no title/project/cost — so the handle
// stays on Roster's RPC fallback instead.
func (h *workerHandle) applyRosterEvent(ev event.Event) {
	cur := h.info.Load()
	if cur == nil {
		return
	}
	next := *cur

	// Updated is the snapshot's own time — what makes staleness observable
	// without a staleness field. The event's publish time is the router's own
	// clock (its broker stamps it on Publish), so it is directly comparable with
	// the times every other row carries.
	if t := ev.Time(); !t.IsZero() {
		next.Updated = t
	} else {
		next.Updated = time.Now()
	}

	switch e := ev.(type) {
	case event.TurnStarted:
		next.Status = supervisor.StatusWorking
	case event.TurnFinished:
		// "tool_use" is the loop's MID-turn marker (the model is about to run
		// tools and call again within the same dispatch), so it does not end the
		// turn — mirroring the same test in wirestream's handleGoferEvent and the
		// daemon's prompt handler.
		if e.StopReason != "tool_use" {
			next.Status = supervisor.StatusNeedsInput
		}
		next.Usage = next.Usage.Add(e.Usage)
		if e.Cost != nil {
			next.Cost = addCost(next.Cost, *e.Cost)
		}
	case event.SessionInfoUpdated:
		if e.Title != "" {
			next.Title = e.Title
		}
	case event.PermissionRequested, event.PermissionResolved:
		// The count is recomputed from the open-request set the watcher maintains
		// rather than incremented/decremented here, so a duplicate delivery (the
		// subscription replays a retained request) cannot drift it.
		next.Pending = h.pendingCount()
	}

	h.info.Store(&next)
}

// addCost sums two [provider.Cost] values bucket by bucket. provider.Usage has
// its own Add; Cost does not, so the router adds it here rather than reaching
// into the SDK for a helper that does not exist.
func addCost(a, b provider.Cost) provider.Cost {
	return provider.Cost{
		USD:           a.USD + b.USD,
		InputUSD:      a.InputUSD + b.InputUSD,
		OutputUSD:     a.OutputUSD + b.OutputUSD,
		CacheReadUSD:  a.CacheReadUSD + b.CacheReadUSD,
		CacheWriteUSD: a.CacheWriteUSD + b.CacheWriteUSD,
	}
}

// trackPermission records pe as an OPEN request on h. It serves two purposes at
// once: the cached row's Pending count, and the replay set
// [Supervisor.SetPermissionRelay] flushes when the daemon's relay is injected
// after the watchers are already running (see that method's doc).
func (h *workerHandle) trackPermission(pe event.PermissionRequested) {
	h.permMu.Lock()
	if h.openPerms == nil {
		h.openPerms = make(map[string]event.PermissionRequested)
	}
	h.openPerms[pe.ID] = pe
	h.permMu.Unlock()
}

// forgetPermission drops a resolved request's open-set entry. Idempotent: the
// same resolution can be observed twice (a retained request and its retained
// resolution both replay on a late subscribe).
func (h *workerHandle) forgetPermission(id string) {
	h.permMu.Lock()
	delete(h.openPerms, id)
	h.permMu.Unlock()
}

// pendingCount is the live number of outstanding permission requests on h — the
// cached row's Pending field, the same count the in-process supervisor keeps in
// its own managed bookkeeping.
func (h *workerHandle) pendingCount() int {
	h.permMu.Lock()
	defer h.permMu.Unlock()
	return len(h.openPerms)
}

// openPermissions snapshots h's still-open requests for a relay flush.
func (h *workerHandle) openPermissions() []event.PermissionRequested {
	h.permMu.Lock()
	defer h.permMu.Unlock()
	out := make([]event.PermissionRequested, 0, len(h.openPerms))
	for _, pe := range h.openPerms {
		out = append(out, pe)
	}
	return out
}
