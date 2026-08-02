package supervisor

import (
	"context"
	"sync"
)

// watcher is one WatchRoster subscriber. The supervisor pushes the latest
// roster snapshot into a single-slot buffer (latest); the watcher's own
// goroutine forwards it to out. Push never blocks the supervisor: it only
// updates latest and pokes signal (buffered 1), so a slow consumer coalesces
// intermediate snapshots into the newest rather than stalling any pump or
// supervisor operation (drop-old semantics).
type watcher struct {
	out    chan []SessionInfo
	signal chan struct{}

	mu     sync.Mutex
	latest []SessionInfo
}

func newWatcher() *watcher {
	return &watcher{
		out:    make(chan []SessionInfo),
		signal: make(chan struct{}, 1),
	}
}

// push stores snap as the latest pending snapshot and wakes the forwarder. It
// never blocks: the signal send is non-blocking and a poke already pending is
// enough (the forwarder always reads latest, not a queued value).
func (w *watcher) push(snap []SessionInfo) {
	w.mu.Lock()
	w.latest = snap
	w.mu.Unlock()
	select {
	case w.signal <- struct{}{}:
	default:
	}
}

// take returns the latest pending snapshot.
func (w *watcher) take() []SessionInfo {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.latest
}

// WatchRoster returns a channel that receives a fresh, full live-roster
// snapshot on subscribe and again on every roster change (create, kill,
// archive, idle⇄running transition, and per-turn cost/usage update). Delivery
// is coalescing drop-old: a slow consumer never blocks the supervisor, and
// may miss intermediate snapshots but always converges to the latest. The
// channel is closed and the watcher goroutine exits when ctx is cancelled or
// [Supervisor.Close] is called.
func (s *Supervisor) WatchRoster(ctx context.Context) (<-chan []SessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	w := newWatcher()

	s.watchMu.Lock()
	if s.watchClosed {
		s.watchMu.Unlock()
		return nil, ErrClosed
	}
	s.watchers[w] = struct{}{}
	s.watchWG.Add(1)
	// Seed the initial snapshot before the consumer can miss any change: the
	// forwarder delivers it first, then live updates.
	//
	// Seeded UNDER watchMu, together with the registration, for the reason
	// [Supervisor.notify] documents: the seed is one more compute-then-push, so
	// leaving it outside the lock lets it overwrite a NEWER snapshot a
	// concurrent notify already pushed (gofer#138). That is the worst instance
	// of the lost update, because a fresh subscriber is exactly the one with no
	// earlier snapshot to fall back on.
	seed := s.snapshotLive()
	if watchSeedTestHook != nil {
		watchSeedTestHook()
	}
	w.push(seed)
	s.watchMu.Unlock()

	go s.runWatcher(ctx, w)
	return w.out, nil
}

// watchSeedTestHook, when non-nil, runs inside [Supervisor.WatchRoster]'s
// registration critical section, between the seed snapshot and its push. It is
// nil in every production build.
//
// It exists because the defect this critical section closes is a pure
// interleaving: there is no input a test can supply to provoke it, only a
// scheduling window of a few microseconds. gofer#138 went unreproduced across
// three milestones for exactly that reason — `-count=200`, `-count=50 -race`,
// and a saturated 12-core machine all came back clean, and the mechanism was
// only established by widening this window by hand.
//
// Parking the seed here makes the interleaving deterministic in BOTH
// directions, which is what makes the regression test evidence rather than
// decoration: holding watchMu across the seed, a concurrent notify waits and the
// watcher still converges; computing the seed outside the lock, the parked seed
// overwrites the newer snapshot and the watcher never converges at all.
var watchSeedTestHook func()

// runWatcher forwards coalesced snapshots to w.out until ctx or the
// supervisor's watch shutdown fires, then deregisters w and closes its
// channel exactly once.
func (s *Supervisor) runWatcher(ctx context.Context, w *watcher) {
	defer s.watchWG.Done()
	defer close(w.out)
	defer s.removeWatcher(w)

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.watchDone:
			return
		case <-w.signal:
			snap := w.take()
			select {
			case w.out <- snap:
			case <-ctx.Done():
				return
			case <-s.watchDone:
				return
			}
		}
	}
}

// removeWatcher drops w from the registry. It is idempotent: a double removal
// (e.g. Close already cleared the map) is a no-op.
func (s *Supervisor) removeWatcher(w *watcher) {
	s.watchMu.Lock()
	delete(s.watchers, w)
	s.watchMu.Unlock()
}

// notify pushes a fresh live-roster snapshot to every current watcher. It is
// called after any roster change; it must never be called while holding
// s.mu or a managed's mu (it snapshots the whole roster, taking each mu).
//
// # Why the snapshot is computed UNDER watchMu, not before it
//
// Delivery is latest-wins (see [watcher.push]), so a watcher's terminal state
// is whatever the LAST push wrote — which is only correct if pushes land in the
// order their snapshots were computed. Computing outside the lock breaks that:
// two callers can read the roster in one order and reach watchMu in the other,
// and the loser overwrites `latest` with a snapshot that is already stale.
//
// That is not a transient blip. A roster is quiescent between changes, so
// nothing pushes again to correct it: the stale snapshot is the last word until
// the NEXT roster change, which for a session settling to needs-input may never
// come. Every waiter on the terminal transition — [Supervisor.AwaitSettled],
// hence `session/load`'s settle barrier, and the TUI's roster — then waits out
// its full budget against a row frozen at "working" (gofer#138: the test
// harness's awaitFoldComplete timing out at 5s with a provably COMPLETE fold).
//
// Holding watchMu across the snapshot serializes compute-and-push into one
// critical section, so push order is snapshot order by construction and a lost
// update is unrepresentable. It costs concurrency between simultaneous
// notifies, never any extra work: the snapshot was already being built per
// notify.
func (s *Supervisor) notify() {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	snap := s.snapshotLive()
	for w := range s.watchers {
		w.push(snap)
	}
}
