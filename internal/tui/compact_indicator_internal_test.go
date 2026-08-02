package tui

// compact_indicator_internal_test.go covers the AUTOMATIC-compaction progress
// indicator (gofer#300): the App-level latch driven by the
// session.compaction_started / .compacted / .compaction_failed contract.
//
// These live in the internal package because the latch is fed by sessEventMsg,
// which is unexported — the same reason app_internal_test.go's approval tests
// do. They reuse its attachForDialogTest/newInternalFakeSup helpers.
//
// The explicit `/compact` path is covered from outside in
// compact_select_test.go; what is new here is that a compaction NOBODY asked
// for from this client is visible at all.

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"
)

// closedSub returns an already-closed subscription.
//
// It exists so the Cmd sessEventMsg returns is SAFE TO INVOKE. That Cmd is
// either waitForEvent alone or a batch containing it, and waitForEvent reads
// sub.C — nil-dereferencing on a zero subscription and blocking forever on a
// live one. Neither lets a test tell the two shapes apart, and telling them
// apart is exactly how the indicator tick is verified. Against a closed
// subscription the read returns immediately, so cmd() resolves either shape.
func closedSub(t *testing.T) *event.Subscription {
	t.Helper()
	b := event.NewBroker()
	sub := b.Subscribe(event.FilterAll, 1)
	b.Close() // closes every subscription it holds
	return sub
}

// feedEvent delivers ev to a's attached session the way a real subscription
// read would, and returns the App plus the Cmd Update produced.
func feedEvent(t *testing.T, a App, ev event.Event) (App, tea.Cmd) {
	t.Helper()
	mdl, cmd := a.Update(sessEventMsg{id: a.sessID, ev: ev, sub: closedSub(t)})
	return mdl.(App), cmd
}

// publishedThroughBroker round-trips ev through a real event.Broker so it comes
// back STAMPED with the seq/time meta event.New* leaves zero. The timestamp is
// the point: it is what the indicator prefers over receipt time, and a
// constructor-built event cannot exercise that branch.
func publishedThroughBroker(t *testing.T, ev event.Event) event.Event {
	t.Helper()
	b := event.NewBroker()
	sub := b.Subscribe(event.FilterAll, 16)
	b.Publish(ev)
	got, ok := <-sub.C
	if !ok {
		t.Fatal("broker closed the subscription before delivering the event")
	}
	return got
}

// TestAutoCompactionStartLatchesIndicator is the headline property of
// gofer#300: an automatic compaction — no /compact, no client RPC in flight —
// puts the in-flight indicator on screen purely from the event contract.
func TestAutoCompactionStartLatchesIndicator(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDialogTest(t, sup)

	if !a.compactingSince.IsZero() {
		t.Fatal("indicator latched before any compaction started")
	}
	if got := a.render(); strings.Contains(got, "compacting context…") {
		t.Fatalf("indicator on screen before any compaction started:\n%s", got)
	}

	a, cmd := feedEvent(t, a, event.NewSessionCompactionStarted(a.sessID, "entry-9", 4))

	if a.compactingSince.IsZero() {
		t.Fatal("session.compaction_started did not latch the indicator")
	}
	if got := a.render(); !strings.Contains(got, "compacting context…") {
		t.Fatalf("expected the in-flight compacting indicator after an automatic start, got:\n%s", got)
	}

	// The tick must be armed alongside the subscription re-read, or the elapsed
	// counter freezes at its first value and the indicator silently stops being
	// evidence of progress. A plain Cmd here means one of the two was dropped —
	// the tea.BatchMsg trap CONTRIBUTING.md documents.
	if cmd == nil {
		t.Fatal("no Cmd after a compaction start; the redraw tick was not armed")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatal("a compaction start must return a batch (the subscription re-read AND the indicator tick); " +
			"a single Cmd means one of the two was dropped")
	}
	if len(batch) != 2 {
		t.Fatalf("batch has %d cmds, want 2 (subscription re-read + indicator tick)", len(batch))
	}
}

// TestAutoCompactionStartPrefersEventTime asserts the indicator counts from
// when the compaction actually began, not from when this client happened to
// receive the news. That is the difference between an honest counter and one
// that restarts at zero for a client attaching mid-compaction — the very case
// the SDK added this event for.
func TestAutoCompactionStartPrefersEventTime(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDialogTest(t, sup)

	stamped := publishedThroughBroker(t, event.NewSessionCompactionStarted(a.sessID, "entry-9", 4))
	if stamped.Time().IsZero() {
		t.Fatal("broker did not stamp the event; this test cannot prove anything")
	}

	a, _ = feedEvent(t, a, stamped)

	if !a.compactingSince.Equal(stamped.Time()) {
		t.Errorf("compactingSince = %v, want the event's own publish time %v — "+
			"the indicator must count from when the compaction started, not from receipt",
			a.compactingSince, stamped.Time())
	}
}

// TestAutoCompactionSuccessClearsIndicator covers the success terminal: the
// transient indicator is retired and the durable itemSessionCompacted block
// takes over as the record.
func TestAutoCompactionSuccessClearsIndicator(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDialogTest(t, sup)

	a, _ = feedEvent(t, a, event.NewSessionCompactionStarted(a.sessID, "entry-9", 4))
	a, _ = feedEvent(t, a, event.NewSessionCompacted(a.sessID, "entry-9", 4, "claude-sonnet-5",
		provider.Usage{InputTokens: 4000, OutputTokens: 200}, "condensed summary"))

	if !a.compactingSince.IsZero() {
		t.Error("session.compacted did not clear the in-flight indicator")
	}
	if got := a.render(); strings.Contains(got, "compacting context…") {
		t.Errorf("in-flight indicator outlived the compaction it describes:\n%s", got)
	}
}

// TestAutoCompactionFailureClearsIndicatorAndReports covers the failure
// terminal. Clearing alone is not enough: an indicator that simply blinks out
// is indistinguishable from one that succeeded, which would leave the user
// believing their context was summarized when it was not.
func TestAutoCompactionFailureClearsIndicatorAndReports(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDialogTest(t, sup)

	a, _ = feedEvent(t, a, event.NewSessionCompactionStarted(a.sessID, "entry-9", 4))
	a, _ = feedEvent(t, a, event.NewSessionCompactionFailed(a.sessID, "entry-9", 4,
		"summarizer: context deadline exceeded"))

	if !a.compactingSince.IsZero() {
		t.Error("session.compaction_failed did not clear the in-flight indicator")
	}
	if a.statusSev != sevDanger {
		t.Errorf("statusSev = %v, want sevDanger — a failed compaction must be reported, not silently dropped", a.statusSev)
	}
	if !strings.Contains(a.status, "summarizer: context deadline exceeded") {
		t.Errorf("status = %q, want it to carry the failure reason", a.status)
	}
}

// TestCompactionIndicatorClearedOnClosedSubscription is the one the SDK's own
// contract doc calls out, and the only clear that is NOT driven by a terminal
// event.
//
// A subscription can be severed between the start and its terminal — the broker
// force-unsubscribes a subscriber it had to block on, or the broker is closed
// out from under the compaction (an ordinary ctrl+c). The terminal is then
// never received, so a latch waiting for one waits forever: "compacting
// context…" stays on screen counting up, describing nothing, for the rest of
// the session.
func TestCompactionIndicatorClearedOnClosedSubscription(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDialogTest(t, sup)

	a, _ = feedEvent(t, a, event.NewSessionCompactionStarted(a.sessID, "entry-9", 4))
	if a.compactingSince.IsZero() {
		t.Fatal("indicator did not latch; this test cannot prove anything")
	}

	mdl, _ := a.Update(sessClosedMsg{id: a.sessID})
	a = mdl.(App)

	if !a.compactingSince.IsZero() {
		t.Error("a closed subscription left the compaction indicator latched — " +
			"its terminal event is never coming, so it would count up forever")
	}
	if got := a.render(); strings.Contains(got, "compacting context…") {
		t.Errorf("compacting indicator survived a closed subscription:\n%s", got)
	}
}

// TestCompactionIndicatorClearedOnSessionSwitch asserts the latch does not ride
// along to another session's transcript. The old subscription's sessClosedMsg
// cannot do this job — switchSession has already moved sessID, so that message
// is dropped as stale.
func TestCompactionIndicatorClearedOnSessionSwitch(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDialogTest(t, sup)

	a, _ = feedEvent(t, a, event.NewSessionCompactionStarted(a.sessID, "entry-9", 4))
	if a.compactingSince.IsZero() {
		t.Fatal("indicator did not latch; this test cannot prove anything")
	}

	roster := GoldenRoster()
	var other string
	for _, s := range roster {
		if s.ID != a.sessID {
			other = s.ID
			break
		}
	}
	if other == "" {
		t.Fatal("golden roster has no second session to switch to")
	}
	a.switchSession(other)

	if !a.compactingSince.IsZero() {
		t.Errorf("switching to %s carried the previous session's compaction indicator across", other)
	}
}

// TestExplicitCompactLatchSurvivesItsOwnStartEvent guards the interaction
// between the two sources that set the latch. `/compact` latches at dispatch —
// an earlier and more truthful instant, since it covers the round trip — and
// the start event that follows must not reset that clock backwards or arm a
// second redraw tick on top of the one already running.
func TestExplicitCompactLatchSurvivesItsOwnStartEvent(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDialogTest(t, sup)

	stamped := publishedThroughBroker(t, event.NewSessionCompactionStarted(a.sessID, "entry-9", 4))
	// Stand in for /compact's dispatch-time latch, deliberately EARLIER than the
	// start event's own timestamp.
	dispatched := stamped.Time().Add(-2 * time.Second)
	a.compactingSince = dispatched

	a, cmd := feedEvent(t, a, stamped)

	if !a.compactingSince.Equal(dispatched) {
		t.Errorf("compactingSince = %v, want the earlier dispatch time %v — "+
			"the start event must not reset a latch /compact already took",
			a.compactingSince, dispatched)
	}
	if cmd != nil {
		if _, isBatch := cmd().(tea.BatchMsg); isBatch {
			t.Error("a second redraw tick was armed on top of the one /compact already started")
		}
	}
}
