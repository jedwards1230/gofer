package wirestream

// forcedrop_test.go covers the must-deliver FORCE-DROP contract this package's
// reconstructor explicitly leans on. reconstruct.go's demux doc (see the
// "bounded head-of-line characteristic" note) states the guarantee in prose: a
// single wedged subscriber can stall reconstruction for other sessions only up
// to the broker's block bound, because "the SDK force-drops the wedged
// subscriber and the demuxer resumes." That force-drop lives in the SDK
// ([event.Broker], pinned — not editable here), but gofer's RELIANCE on it had
// no independent coverage; this pins it.
//
// # What gofer does and does not do with a force-drop
//
// gofer has NO code path that OBSERVES a force-drop distinctly: nothing in the
// tree calls [event.Subscription.Forced]. Every subscription consumer (the
// daemon's session/prompt fan-out, the supervisor's permission watcher, this
// package's demuxer) treats a closed channel uniformly — a force-dropped close
// is indistinguishable from a normal Close or a broker shutdown, and that is by
// design (the journal is the durable transcript; a force-dropped live subscriber
// simply re-reads folded history on reattach). gofer relies on the force-drop
// purely as a LIVENESS guarantee: that a wedged must-deliver subscriber cannot
// block the publisher forever. This test verifies exactly that guarantee against
// a broker configured the way each reconstructed session's broker is (WithReplay
// at replayDepth), using gofer's own event types, with a short block bound so the
// force-drop is exercised in milliseconds rather than the SDK's 5s default.
import (
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"
)

// TestMustDeliverForceDropUnwedgesPublisher drives the must-deliver force-drop
// path gofer's reconstructor depends on: a wedged subscriber (buffer full, never
// drained) is force-unsubscribed once a must-deliver publish blocks past the
// block bound, so the publisher makes progress and a HEALTHY subscriber keeps
// receiving every event.
func TestMustDeliverForceDropUnwedgesPublisher(t *testing.T) {
	const blockBound = 100 * time.Millisecond
	// Mirror each reconstructed session broker's construction (see core.go's
	// sessionLocked: event.NewBroker(event.WithReplay(replayDepth))), plus a
	// short block bound purely to keep the force-drop fast and deterministic.
	b := event.NewBroker(event.WithReplay(replayDepth), event.WithBlockBound(blockBound))
	defer b.Close()

	// wedged: a tiny buffer we never drain. healthy: ample buffer, drained below.
	const wedgedBuf = 1
	wedged := b.Subscribe(event.FilterAll, wedgedBuf)
	healthy := b.Subscribe(event.FilterAll, subBuffer)

	// session.created/turn.started/turn.finished are must-deliver lifecycle
	// events (TierMustDeliver) — the exact tier the force-drop applies to and the
	// tier this package republishes onto session brokers. Publish more than the
	// wedged subscriber's buffer can hold so a publish blocks and trips the bound.
	sid := "sess-forcedrop"
	must := []event.Event{
		event.NewSessionCreated(sid),
		event.NewTurnStarted(sid),
		event.NewTurnFinished(sid, "end_turn", provider.Usage{}),
	}

	start := time.Now()
	for _, e := range must {
		b.Publish(e) // synchronous fan-out; blocks up to blockBound on the wedged sub
	}
	elapsed := time.Since(start)

	// The publisher was NOT wedged forever: total block is bounded by the SDK's
	// per-publish block bound, not unbounded. Generous ceiling (a few bounds worth
	// plus scheduling slack) — the point is "bounded", not a tight number.
	if elapsed > 2*time.Second {
		t.Fatalf("publish loop took %v — a wedged must-deliver subscriber blocked the publisher past the bound", elapsed)
	}

	// The wedged subscriber was force-dropped: its channel is closed and Forced()
	// reports it (as opposed to a caller Close or broker shutdown).
	drainClosed(t, wedged, "wedged")
	if !wedged.Forced() {
		t.Error("wedged subscriber: Forced() = false, want true after the block bound elapsed")
	}

	// The healthy subscriber is untouched by its sibling's force-drop: it observes
	// every published event, in order, and was never forced.
	got := drainAll(t, healthy)
	if len(got) != len(must) {
		t.Fatalf("healthy subscriber received %d events, want %d", len(got), len(must))
	}
	for i, e := range must {
		if got[i].Kind() != e.Kind() {
			t.Errorf("healthy event %d = %q, want %q", i, got[i].Kind(), e.Kind())
		}
	}
	if healthy.Forced() {
		t.Error("healthy subscriber: Forced() = true, want false — it was drained, not wedged")
	}
}

// drainClosed asserts sub's channel is closed within a short window, draining any
// buffered events first. A force-dropped subscription's channel is closed by the
// broker, so a receive eventually observes !ok.
func drainClosed(t *testing.T, sub *event.Subscription, name string) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-sub.C:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatalf("%s subscriber: channel not closed within 1s — it was not force-dropped", name)
		}
	}
}

// drainAll collects every event buffered on sub without blocking on more,
// stopping at the first drought or a closed channel.
func drainAll(t *testing.T, sub *event.Subscription) []event.Event {
	t.Helper()
	var out []event.Event
	for {
		select {
		case e, ok := <-sub.C:
			if !ok {
				return out
			}
			out = append(out, e)
		case <-time.After(200 * time.Millisecond):
			return out
		}
	}
}
