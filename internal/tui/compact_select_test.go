package tui_test

// compact_select_test.go covers /compact and /context end to end through
// App's exported Update/View surface, reusing app_test.go's fakeSup/press/
// type_/content helpers and command_test.go's dispatchSlash — the same shape
// effort_select_test.go uses for /thinking.

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
)

// TestCompactNoAttachedSessionReportsDanger asserts /compact on the overview
// (no attached session) reports a danger note and never reaches the
// Supervisor — there is no "apply to the default" reading for a command that
// summarizes a SPECIFIC session's own history.
func TestCompactNoAttachedSessionReportsDanger(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := newTestApp(t, sup)

	m = dispatchSlash(t, m, "/compact")

	if len(sup.ops) != 0 {
		t.Fatalf("sup.ops = %v; want none — /compact must not reach the Supervisor with no attached session", sup.ops)
	}
	if got := content(m); !strings.Contains(got, "/compact needs an attached session") {
		t.Fatalf("expected the no-session danger note, got:\n%s", got)
	}
}

// dispatchCompact runs `/compact <args>` on an ATTACHED session and returns the
// model MID-FLIGHT — the compaction dispatched, its result not yet delivered —
// alongside the batch's resolved messages, which the caller replays to end it.
//
// It exists because runCompact returns a tea.Batch — the op call AND the
// indicator's tick — and the shared `press` helper drives exactly one Cmd, so a
// batch reaches App.Update as a tea.BatchMsg it has no case for and BOTH halves
// vanish silently (the failure mode TestSyncMenuReturnsAtMostOneCmd exists to
// catch elsewhere). Expanding the batch here is what keeps these tests
// exercising the real path rather than going quietly vacuous.
//
// The messages come back UNAPPLIED, and that split is the point: it is what
// lets a test observe the in-flight frame and the settled frame separately,
// which is the whole property under test.
func dispatchCompact(t *testing.T, m tea.Model, args string) (tea.Model, []tea.Msg) {
	t.Helper()
	m = type_(t, m, "/compact"+args)
	m, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("/compact returned no Cmd; the op must be dispatched")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatal("/compact must return a batch (the Compact op AND the indicator tick); " +
			"a single Cmd means one of the two was dropped")
	}
	msgs := make([]tea.Msg, 0, len(batch))
	for _, c := range batch {
		msgs = append(msgs, c())
	}
	return m, msgs
}

// settle replays every message dispatchCompact collected, ending the
// compaction.
func settle(m tea.Model, msgs []tea.Msg) tea.Model {
	for _, msg := range msgs {
		m, _ = m.Update(msg)
	}
	return m
}

// TestCompactBareAttachedDispatchesEmptyInstructions covers the bare
// `/compact` form: an attached session dispatches Compact with "" —
// runner.Runner.Compact's own signal to fall back to its default
// instructions — not a missing-argument error.
func TestCompactBareAttachedDispatchesEmptyInstructions(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := newTestApp(t, sup)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // attach the selected session

	m, _ = dispatchCompact(t, m, "")

	wantOp := "compact:" + attachedSessionID + ":"
	if len(sup.ops) != 1 || sup.ops[0] != wantOp {
		t.Fatalf("sup.ops = %v; want one entry %q", sup.ops, wantOp)
	}
	if got := content(m); !strings.Contains(got, "compacting context…") {
		t.Fatalf("expected the in-flight compacting indicator, got:\n%s", got)
	}
}

// TestCompactArgAttachedForwardsInstructions covers `/compact <instructions>`:
// every argument is space-joined and forwarded verbatim as the summarizer's
// instructions.
func TestCompactArgAttachedForwardsInstructions(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := newTestApp(t, sup)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	m, _ = dispatchCompact(t, m, " focus on the open TODOs")

	wantOp := "compact:" + attachedSessionID + ":focus on the open TODOs"
	if len(sup.ops) != 1 || sup.ops[0] != wantOp {
		t.Fatalf("sup.ops = %v; want one entry %q", sup.ops, wantOp)
	}
	// The dispatch must be visibly acknowledged while it runs (see runCompact's
	// doc) rather than a silent op with nothing on screen until the async result
	// lands; the compaction's actual transcript record (once session.compacted
	// arrives) is pinned separately by TestGoldenSessionCompacted.
	if got := content(m); !strings.Contains(got, "compacting context…") {
		t.Fatalf("expected the in-flight compacting indicator, got:\n%s", got)
	}
}

// TestCompactIndicatorRetiresOnSuccess is the regression anchor for the bug
// this indicator replaced: compaction's only progress signal used to be an
// OPTIMISTIC status note that nothing ever cleared, so a compaction that had
// fully finished — its summary already rendered in the transcript — still read
// as permanently in progress.
//
// The assertion is deliberately the PAIR. "Absent at the end" alone would pass
// against an indicator that never rendered at all, which is a different bug
// wearing the same result.
func TestCompactIndicatorRetiresOnSuccess(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := newTestApp(t, sup)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	m, msgs := dispatchCompact(t, m, "")
	if got := content(m); !strings.Contains(got, "compacting context…") {
		t.Fatalf("indicator missing while the compaction is in flight, got:\n%s", got)
	}

	m = settle(m, msgs)

	if got := content(m); strings.Contains(got, "compacting context…") {
		t.Fatalf("the indicator outlived the compaction that finished, got:\n%s", got)
	}
}

// TestCompactFailurePropagatesAsDanger asserts a Supervisor-side rejection
// (here standing in for supervisor.ErrRunning / runner.ErrNothingToCompact)
// surfaces as a danger note AND retires the indicator — a refused compaction
// must not keep claiming to be running any more than a finished one.
func TestCompactFailurePropagatesAsDanger(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	sup.compactErr = errors.New("session is running or has queued work")
	m := newTestApp(t, sup)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	m, msgs := dispatchCompact(t, m, "")
	m = settle(m, msgs)

	got := content(m)
	if !strings.Contains(got, "session is running or has queued work") {
		t.Fatalf("expected the propagated failure note, got:\n%s", got)
	}
	if strings.Contains(got, "compacting context…") {
		t.Fatalf("the indicator outlived the compaction that failed, got:\n%s", got)
	}
}

// TestGoldenPanelContext verifies /context opens the command panel on the
// Context tab. Opened from the overview (no attached session) it shows the
// honest empty state — the attached-session mapping is pinned at the
// contextView level (context_test.go).
func TestGoldenPanelContext(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = dispatchSlash(t, m, "/context")
	testkit.AssertGolden(t, "app_panel_context", content(m))
}

// TestContextReflectsAttachedSession verifies the end-to-end wiring: /context
// opened after attaching to a session with LastUsage/ContextWindow set shows
// the real numbers, not the overview's empty state.
func TestContextReflectsAttachedSession(t *testing.T) {
	roster := tui.GoldenRoster()
	roster[0].Model = "claude-sonnet-5"
	roster[0].LastUsage.InputTokens = 40000
	roster[0].ContextWindow = 200000

	m := newTestApp(t, newFakeSup(roster))
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // attach the selected session
	m = dispatchSlash(t, m, "/context")

	got := content(m)
	if !strings.Contains(got, "Context window: 200000 tokens") {
		t.Errorf("expected the attached session's context window, got:\n%s", got)
	}
	if !strings.Contains(got, "20%") {
		t.Errorf("expected the computed fill percentage, got:\n%s", got)
	}
}
