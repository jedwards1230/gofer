package daemon

// observer_internal_test.go covers the REGISTRY half of the standing
// out-of-turn observer (observer.go): how many subscriptions a session ends up
// with, and when a closed one is allowed to be replaced.
//
// It is white-box on purpose. The delivery half — that an out-of-turn event
// actually reaches a merely-attached client through a real worker process — is
// pinned end to end by internal/router's
// TestOutOfTurnCompactionReachesAttachedClient, which is the only place the
// whole four-hop chain exists. What that test CANNOT see is a second
// subscription: two observers deliver the same frame twice, and a wire assertion
// for "exactly once" against two racing goroutines is a timing test, not a
// proof. Counting subscriptions at the seam is deterministic.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"
)

// observerStub is a [Supervisor] that answers SubscribeLive off a broker it
// owns, counting the calls. Every other method is inherited from the embedded
// nil interface — they are never reached on this path, and a nil-panic is the
// right outcome if that ever stops being true.
type observerStub struct {
	Supervisor

	broker *event.Broker

	mu   sync.Mutex
	subs int
}

func newObserverStub() *observerStub {
	return &observerStub{broker: event.NewBroker(event.WithReplay(8))}
}

func (s *observerStub) SubscribeLive(_ context.Context, _ string) (*event.Subscription, error) {
	s.mu.Lock()
	s.subs++
	s.mu.Unlock()
	return s.broker.SubscribeLive(event.FilterAll, 8), nil
}

func (s *observerStub) subCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subs
}

// TestStartSessionObserverSubscribesOncePerSession pins the property that keeps
// this from double-delivering: a session is loaded once per attaching client, so
// session/load runs many times for one session, and each extra subscription
// would put every out-of-turn event on the wire an extra time.
func TestStartSessionObserverSubscribesOncePerSession(t *testing.T) {
	sup := newObserverStub()
	d := New(sup, Config{RelayOutOfTurnEvents: true})
	t.Cleanup(func() { d.cancel(); sup.broker.Close() })

	for range 5 {
		d.startSessionObserver("s1")
	}
	if got := sup.subCount(); got != 1 {
		t.Errorf("5 starts for one session opened %d subscriptions, want 1", got)
	}

	// A DIFFERENT session is a different observer — the registry keys per
	// session, it does not cap the daemon at one.
	d.startSessionObserver("s2")
	if got := sup.subCount(); got != 2 {
		t.Errorf("a second session opened %d subscriptions in total, want 2", got)
	}
}

// TestStartSessionObserverOffByDefault pins the flag being a real gate. Off is
// the default precisely because the in-process `gofer daemon` already runs the
// equivalent watcher off the supervisor's OnRegister hook; a daemon that
// observed anyway would deliver every out-of-turn event twice there.
func TestStartSessionObserverOffByDefault(t *testing.T) {
	sup := newObserverStub()
	d := New(sup, Config{})
	t.Cleanup(func() { d.cancel(); sup.broker.Close() })

	d.startSessionObserver("s1")
	if got := sup.subCount(); got != 0 {
		t.Errorf("observer subscribed %d times with RelayOutOfTurnEvents unset, want 0", got)
	}
}

// TestStartSessionObserverReleasesOnBrokerClose pins the LIFETIME anchor: the
// observer goroutine ends when its subscription channel closes (which is what
// Supervisor.Kill/Close do to a session's broker), and — the part a leaked
// registry entry would break — it releases its slot on the way out, so a session
// that comes back is observed again rather than silently unobserved forever.
func TestStartSessionObserverReleasesOnBrokerClose(t *testing.T) {
	sup := newObserverStub()
	d := New(sup, Config{RelayOutOfTurnEvents: true})
	t.Cleanup(func() { d.cancel() })

	d.startSessionObserver("s1")
	if got := sup.subCount(); got != 1 {
		t.Fatalf("initial start opened %d subscriptions, want 1", got)
	}

	// Closing the broker closes every subscription channel, which is the
	// observer goroutine's own exit condition.
	sup.broker.Close()

	// The release happens on that goroutine, so it is an EVENTUAL property; the
	// deadline is a failure backstop, not a synchronization device. A registry
	// entry that leaked would keep every retry a no-op and fail here.
	sup.broker = event.NewBroker(event.WithReplay(8))
	t.Cleanup(func() { sup.broker.Close() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		d.startSessionObserver("s1")
		if sup.subCount() == 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("observer slot for s1 never released after its broker closed (subscriptions: %d, want 2)", sup.subCount())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
