package wirestream

// subscribe_internal_test.go pins the distinction between the two subscribe
// modes on the reconstruction core: [Reconstructor.Subscribe] replays a
// session broker's retained must-deliver backlog to a late subscriber, while
// [Reconstructor.SubscribeLive] — the M6 no-replay path the router's live
// fan-out needs — does not. It drives handleNotification directly (no real
// *daemon.Client), the same bare-Reconstructor seam reconstruct_internal_test.go
// uses.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// replayGoferEvent marshals a source event to its gofer/event envelope and
// pushes it through handleNotification, exactly as the demuxer would for a
// wire notification — republishing it onto the named session's broker (and,
// for a must-deliver event, retaining it in the broker's replay backlog).
func replayGoferEvent(t *testing.T, r *Reconstructor, src event.Event) {
	t.Helper()
	raw, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal %T: %v", src, err)
	}
	r.handleNotification(daemon.Notification{Method: methodGoferEvent, Params: raw})
}

// TestSubscribeReplaysBacklogSubscribeLiveDoesNot is the focused proof for the
// no-replay subscribe. A must-deliver session.created is reconstructed onto
// sess-1's broker BEFORE either subscription exists, so it sits in the broker's
// replay backlog. A subsequent [Reconstructor.Subscribe] must observe it
// (replay); a [Reconstructor.SubscribeLive] must NOT (no backlog) — yet must
// still observe an event published AFTER it subscribes, proving it is a live
// stream, not a dead one.
func TestSubscribeReplaysBacklogSubscribeLiveDoesNot(t *testing.T) {
	const sid = "sess-1"
	r := newReconstructTestReconstructor()
	r.RegisterFresh(sid)

	// A retained must-deliver event enters the backlog before any subscriber.
	replayGoferEvent(t, r, event.NewSessionCreated(sid))

	// Subscribe replays the backlog: it sees the earlier session.created.
	replaySub, err := r.Subscribe(context.Background(), sid)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer replaySub.Close()
	select {
	case ev := <-replaySub.C:
		if ev.Kind() != event.KindSessionCreated {
			t.Fatalf("Subscribe replayed %q, want %q", ev.Kind(), event.KindSessionCreated)
		}
	case <-time.After(time.Second):
		t.Fatal("Subscribe did not replay the retained session.created backlog")
	}

	// SubscribeLive skips the backlog: it must NOT see the earlier event.
	liveSub, err := r.SubscribeLive(context.Background(), sid)
	if err != nil {
		t.Fatalf("SubscribeLive: %v", err)
	}
	defer liveSub.Close()
	select {
	case ev := <-liveSub.C:
		t.Fatalf("SubscribeLive replayed %q; want no backlog replay", ev.Kind())
	case <-time.After(100 * time.Millisecond):
	}

	// But it IS a live stream: an event published now reaches BOTH subscribers.
	replayGoferEvent(t, r, event.NewTurnStarted(sid))
	for name, sub := range map[string]*event.Subscription{"Subscribe": replaySub, "SubscribeLive": liveSub} {
		select {
		case ev := <-sub.C:
			if ev.Kind() != event.KindTurnStarted {
				t.Errorf("%s got %q after live publish, want %q", name, ev.Kind(), event.KindTurnStarted)
			}
		case <-time.After(time.Second):
			t.Errorf("%s did not observe the live turn.started publish", name)
		}
	}
}

// TestSubscribeLiveOnClosedReturnsErrClosed asserts SubscribeLive honors the
// same closed-Reconstructor contract as Subscribe: a nil session lookup after
// close returns [ErrClosed] rather than handing back a broker nothing will
// ever publish to or close.
func TestSubscribeLiveOnClosedReturnsErrClosed(t *testing.T) {
	r := newReconstructTestReconstructor()
	close(r.closed)
	if _, err := r.SubscribeLive(context.Background(), "sess-1"); err != ErrClosed {
		t.Fatalf("SubscribeLive after close = %v, want ErrClosed", err)
	}
}
