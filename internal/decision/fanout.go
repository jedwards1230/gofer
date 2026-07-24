package decision

import (
	"sync"
	"sync/atomic"
)

// fanout is the subscriber registry both update sources in this package publish
// through: a [Gate] (the AGENT side, where an open request blocks a turn) and a
// [Stream] (the CLIENT side, where the same updates arrive off a daemon wire
// and nothing ever blocks).
//
// It exists so the two share ONE [Subscription] type. A consumer — the TUI —
// reads `*Subscription` without caring whether its supervisor is in-process
// (internal/tuibridge, subscribing to a real Gate) or daemon-backed
// (internal/daemonbridge, subscribing to a reconstructed Stream); a second,
// parallel subscription type would fork every one of those consumers for no
// semantic gain.
//
// The zero value is ready to use. Every method takes fanout's own lock and
// never calls back into its owner, so an owner may hold its own lock across a
// call here — and both do, deliberately: publishing under the owner's lock is
// what makes an update's ordering match the state change that produced it. The
// lock order is therefore always owner-then-fanout and never the reverse, so
// the two can not deadlock.
type fanout struct {
	mu   sync.Mutex
	subs map[*Subscription]struct{}
}

// count reports how many subscribers are registered. [Gate.Request] reads it to
// decide [ErrNoClient]; nothing else should need it.
func (f *fanout) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs)
}

// add registers sub for delivery.
func (f *fanout) add(sub *Subscription) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.subs == nil {
		f.subs = make(map[*Subscription]struct{})
	}
	f.subs[sub] = struct{}{}
}

// remove deregisters sub. Idempotent — a subscription closed twice, or closed
// after its owner already drained the registry, must not panic.
func (f *fanout) remove(sub *Subscription) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.subs, sub)
}

// publish fans u out to every subscriber, dropping (and counting — see
// [Subscription.Dropped]) on a full buffer rather than blocking. Never blocking
// is the whole contract: on the Gate side a wedged client must not be able to
// hang an agent turn from inside [Gate.Request], and on the Stream side it must
// not be able to stall the wire demuxer that feeds every other session.
func (f *fanout) publish(u Update) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for sub := range f.subs {
		select {
		case sub.ch <- u:
		default:
			sub.dropped.Add(1)
		}
	}
}

// drain deregisters every subscriber and returns them, for an owner shutting
// down: it is deliberately split from closing their channels so the owner can
// close them OUTSIDE its own lock, where a concurrent [Subscription.Close]
// (which takes this lock) cannot be racing the same map.
func (f *fanout) drain() []*Subscription {
	f.mu.Lock()
	defer f.mu.Unlock()
	subs := make([]*Subscription, 0, len(f.subs))
	for sub := range f.subs {
		subs = append(subs, sub)
	}
	clear(f.subs)
	return subs
}

// Subscription is a receive stream of [Update]s. Range over C to consume
// updates; call Close to unsubscribe. C is closed either by Close or by the
// source ending — [Gate.Close] when the session goes away, [Stream.Close] when
// the connection carrying it does — so a consumer must treat a closed channel
// as "this stream is over", exactly as it treats an event subscription closing.
type Subscription struct {
	// C receives updates. It is closed when the subscription is closed.
	C <-chan Update

	ch      chan Update
	src     *fanout
	dropped atomic.Uint64
	once    sync.Once
}

// Dropped returns how many updates were discarded because this subscriber's
// buffer was full — a diagnostic: a non-zero count means this client may be
// missing an open request. No consumer acts on it today (see [Gate.Subscribe]
// for why buffer sizing is the mitigation instead); reconciling against
// [Gate.Open] is what acting on it would mean.
func (s *Subscription) Dropped() uint64 { return s.dropped.Load() }

// Close unsubscribes and closes C. It is idempotent and safe to call
// concurrently with delivery: the unsubscribe takes the source's fanout lock,
// which a concurrent publish also holds, so no send can race the close.
func (s *Subscription) Close() {
	s.src.remove(s)
	s.once.Do(func() { close(s.ch) })
}
