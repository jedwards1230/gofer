package decision

import (
	"slices"
	"sync"
)

// Stream is the CLIENT side of one session's decision set: the same [Update]s a
// [Gate] publishes, arriving off a daemon wire instead of from a blocked agent
// turn. internal/wirestream builds one per remote session and feeds it the
// daemon's gofer/decision_requested / gofer/decision_resolved notifications;
// internal/daemonbridge hands out subscriptions to it, so the TUI consumes a
// daemon-backed session's decisions through the identical [Subscription] it
// consumes an in-process gate's through.
//
// It is deliberately NOT a Gate. A Gate's central method blocks an agent turn
// until a human answers, and a client cannot answer anything locally — its
// answer has to travel back over the wire — so a client holding a Gate would
// own a blocking primitive it must never call and an answer path that would
// resolve nothing. Stream therefore exposes only what a client can honestly do:
// APPLY what the wire said, and let consumers subscribe to the result. The two
// share the [fanout] (and so the Subscription type) and nothing else.
//
// Like a Gate it replays the currently-open requests to a late subscriber, and
// for the same reason: a TUI attaching to a session already blocked on a
// question must see the question. That is the whole point of tracking the open
// set here rather than passing updates straight through.
//
// Build one with [NewStream]. Every method is safe for concurrent use.
type Stream struct {
	mu sync.Mutex
	// fan is the subscriber registry (see [fanout]). Published to under mu, so a
	// subscriber's view of the open set is ordered identically to this Stream's.
	fan fanout
	// open holds every request the wire says is currently outstanding, keyed by
	// request id; order keeps their arrival order for a deterministic replay.
	// A request id is unique only within its session (see [Request.ID]), which is
	// fine: one Stream is one session.
	open  map[string]Request
	order []string
	// closed reports that [Stream.Close] has run — the connection feeding this
	// stream is gone, so no further update may be applied and no new subscriber
	// may register.
	closed bool
}

// NewStream returns an empty Stream: no open requests, no subscribers.
func NewStream() *Stream {
	return &Stream{open: make(map[string]Request)}
}

// Apply folds one wire-observed update into the stream and publishes it to
// every subscriber. It never blocks (see [fanout.publish]) — it runs on the
// single wire demuxer goroutine that also drains the connection's control
// plane, so a wedged consumer must not be able to stall it.
//
// An [UpdateRequested] records the request so a later [Stream.Subscribe]
// replays it; a duplicate id (the same request re-broadcast by the daemon's
// replay-on-attach after this stream already saw it live) refreshes the entry
// in place and is published again, which a client renders as the same prompt it
// is already showing rather than a second one.
//
// An [UpdateResolved] drops the request and is published UNCONDITIONALLY, even
// for an id this stream never saw open. That asymmetry is deliberate: unlike a
// Gate — which is the authority on its own open set and cannot resolve what it
// never opened — a Stream is a lossy projection of one (a full subscriber
// buffer drops updates, see [Subscription.Dropped]), so swallowing a resolution
// for an id it happens to have missed is the one failure that would leave a
// prompt on screen forever with nothing behind it.
//
// An update applied after [Stream.Close] is dropped: the session's client is
// gone and every subscription has already been closed.
func (s *Stream) Apply(u Update) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	switch u.Kind {
	case UpdateRequested:
		if _, dup := s.open[u.Request.ID]; !dup {
			s.order = append(s.order, u.Request.ID)
		}
		s.open[u.Request.ID] = u.Request
	case UpdateResolved:
		s.removeLocked(u.Request.ID)
	}
	s.fan.publish(u)
}

// Open returns a snapshot of the requests the wire says are currently open, in
// arrival order — the client-side twin of [Gate.Open], for tests and for a
// consumer that wants "what is this remote session blocked on?" without
// subscribing. Each Request shares its Questions slice with the live entry;
// treat it as read-only.
func (s *Stream) Open() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.openLocked()
}

// Subscribe returns a stream of [Update]s, replaying an [UpdateRequested] for
// every currently-open request before any live update — the same contract
// [Gate.Subscribe] offers, including the drop-on-full delivery, the negative
// buffer clamp, the guaranteed room for the replay, and the already-closed
// subscription a closed source hands back so a consumer's pump learns the
// stream is over through one code path rather than two.
func (s *Stream) Subscribe(buffer int) *Subscription {
	if buffer < 0 {
		buffer = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		ch := make(chan Update)
		sub := &Subscription{C: ch, ch: ch, src: &s.fan}
		sub.once.Do(func() { close(ch) })
		return sub
	}

	replay := s.openLocked()
	capacity := buffer
	if len(replay) > capacity {
		capacity = len(replay)
	}
	ch := make(chan Update, capacity)
	sub := &Subscription{C: ch, ch: ch, src: &s.fan}
	for _, r := range replay {
		ch <- Update{Kind: UpdateRequested, Request: r} // fits: capacity >= len(replay)
	}
	s.fan.add(sub)
	return sub
}

// Close ends the stream for good: every request still open publishes its
// [UpdateResolved] first — so a client rendering a prompt clears it instead of
// leaving a question on screen that nothing can answer any more — and then
// every subscription's channel is closed, so its consumer's pump unwinds rather
// than parking forever.
//
// It is the client-side counterpart of [Gate.Close] and is called for the
// counterpart reason: the connection carrying this session's decisions has gone
// away (see internal/wirestream's Close), which for a client is the same event
// as the session ending.
//
// Close is idempotent and safe to call concurrently with Apply/Subscribe: it
// takes mu for the state transition, and the subscription channels are closed
// only after their subscriptions have left the registry publish walks, so no
// send can race a close.
func (s *Stream) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	for _, id := range slices.Clone(s.order) {
		req := s.open[id]
		s.removeLocked(id)
		s.fan.publish(Update{Kind: UpdateResolved, Request: Request{ID: id, SessionID: req.SessionID}})
	}
	subs := s.fan.drain()
	s.mu.Unlock()

	// Outside mu: a Subscription.Close racing this one also takes the fanout
	// lock, and the sync.Once is what makes exactly one of the two close the
	// channel.
	for _, sub := range subs {
		sub.once.Do(func() { close(sub.ch) })
	}
}

// removeLocked deletes requestID from both the open map and the order slice.
// Callers hold s.mu.
func (s *Stream) removeLocked(requestID string) {
	delete(s.open, requestID)
	for i, id := range s.order {
		if id == requestID {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

// openLocked snapshots the open requests in arrival order. Callers hold s.mu.
func (s *Stream) openLocked() []Request {
	if len(s.order) == 0 {
		return nil
	}
	out := make([]Request, 0, len(s.order))
	for _, id := range s.order {
		if r, ok := s.open[id]; ok {
			out = append(out, r)
		}
	}
	return out
}
