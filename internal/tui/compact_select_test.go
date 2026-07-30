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

// TestCompactBareAttachedDispatchesEmptyInstructions covers the bare
// `/compact` form: an attached session dispatches Compact with "" —
// runner.Runner.Compact's own signal to fall back to its default
// instructions — not a missing-argument error.
func TestCompactBareAttachedDispatchesEmptyInstructions(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := newTestApp(t, sup)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // attach the selected session

	m = dispatchSlash(t, m, "/compact")

	wantOp := "compact:" + attachedSessionID + ":"
	if len(sup.ops) != 1 || sup.ops[0] != wantOp {
		t.Fatalf("sup.ops = %v; want one entry %q", sup.ops, wantOp)
	}
	if got := content(m); !strings.Contains(got, "Compacting context") {
		t.Fatalf("expected the optimistic compacting status note, got:\n%s", got)
	}
}

// TestCompactArgAttachedForwardsInstructions covers `/compact <instructions>`:
// every argument is space-joined and forwarded verbatim as the summarizer's
// instructions.
func TestCompactArgAttachedForwardsInstructions(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := newTestApp(t, sup)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	m = dispatchSlash(t, m, "/compact focus on the open TODOs")

	wantOp := "compact:" + attachedSessionID + ":focus on the open TODOs"
	if len(sup.ops) != 1 || sup.ops[0] != wantOp {
		t.Fatalf("sup.ops = %v; want one entry %q", sup.ops, wantOp)
	}
	// The dispatch itself must be visibly acknowledged (the optimistic
	// "Compacting…" note — see runCompact's doc) rather than a silent op with
	// nothing on screen until the async result lands; the compaction's actual
	// transcript record (once session.compacted arrives) is pinned separately
	// by TestGoldenSessionCompacted.
	got := content(m)
	if !strings.Contains(got, "Compacting context") {
		t.Fatalf("expected the optimistic compacting status note, got:\n%s", got)
	}
}

// TestCompactFailurePropagatesAsDanger asserts a Supervisor-side rejection
// (here standing in for supervisor.ErrRunning / runner.ErrNothingToCompact)
// overrides the optimistic status note through the ordinary opDoneMsg error
// path, the same as any other dispatched op.
func TestCompactFailurePropagatesAsDanger(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	sup.compactErr = errors.New("session is running or has queued work")
	m := newTestApp(t, sup)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	m = dispatchSlash(t, m, "/compact")

	if got := content(m); !strings.Contains(got, "session is running or has queued work") {
		t.Fatalf("expected the propagated failure note, got:\n%s", got)
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
