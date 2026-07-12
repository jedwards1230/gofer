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
	s.watchMu.Unlock()

	// Seed the initial snapshot before the consumer can miss any change: the
	// forwarder delivers it first, then live updates.
	w.push(s.snapshotLive())

	go s.runWatcher(ctx, w)
	return w.out, nil
}

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
func (s *Supervisor) notify() {
	snap := s.snapshotLive()
	s.watchMu.Lock()
	for w := range s.watchers {
		w.push(snap)
	}
	s.watchMu.Unlock()
}
