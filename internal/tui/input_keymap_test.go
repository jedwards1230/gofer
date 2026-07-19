package tui_test

// input_keymap_test.go covers the native editing keymap (input_keymap.go)
// wired end to end through App's exported surface — both text-entry paths
// it applies to (the overview dispatch bar and the attach input), and the
// navigation-contract interplay with Left/Right (each screen's own arrow is
// conditional on its input being empty: a bare Right on the overview
// attaches only from an empty dispatch bar, a bare Left on the attach screen
// backs out only from an empty input). [inputBuffer]'s own edit operations
// are hard-unit-tested in isolation in inputbuf_test.go.

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/tui"
)

// altLeft/altRight/ctrlA/ctrlE/ctrlW/ctrlU/ctrlK build the KeyPressMsg a real
// terminal delivers for each binding — Option/Alt reliably arrives as
// [tea.ModAlt] in bubbletea v2.0.8; Ctrl letters arrive as the letter's Code
// with [tea.ModCtrl] set (the same shape every existing ctrl-c/ctrl-x
// binding in this package already uses).
func altLeft() tea.KeyPressMsg  { return tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModAlt} }
func altRight() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModAlt} }
func altBackspace() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: tea.KeyBackspace, Mod: tea.ModAlt}
}
func ctrlKey(r rune) tea.KeyPressMsg { return tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl} }

// TestOverviewDispatchLeftMovesCursorMidText covers Left moving the
// dispatch-bar cursor — free of the navigation contract's Right binding,
// which only bare Right claims (see TestOverviewDispatchBareRightAttaches).
func TestOverviewDispatchLeftMovesCursorMidText(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = type_(t, m, "abc")
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyLeft})
	m = press(t, m, tea.KeyPressMsg{Text: "X"})

	if got := content(m); !strings.Contains(got, "❯ abX▏c") {
		t.Fatalf("expected Left then a typed rune to insert mid-buffer, got:\n%s", got)
	}
}

// TestOverviewDispatchBareRightMovesCursorWithText pins the overview's half
// of the conditional-nav contract: with dispatch-bar text, a bare
// (unmodified) Right EDITS — it moves the cursor right one rune rather than
// attaching, the exact mirror of bare Left on the attach screen
// (TestAttachInputLeftBacksOutOnlyWhenEmpty). Before this the case claimed
// bare Right outright, so the dispatch-bar cursor could only ever move left.
func TestOverviewDispatchBareRightMovesCursorWithText(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = type_(t, m, "abc")
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyLeft})
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyLeft})
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
	m = press(t, m, tea.KeyPressMsg{Text: "X"})

	if got := content(m); !strings.Contains(got, "❯ abX▏c") {
		t.Fatalf("expected bare Right with text to move the dispatch-bar cursor, got:\n%s", got)
	}
	if got := content(m); strings.Contains(got, "> ▏") {
		t.Fatalf("expected to stay on the overview (Right must not attach with text), got:\n%s", got)
	}
}

// TestOverviewDispatchBareRightAttachesWhenEmpty pins the other half: with an
// EMPTY dispatch bar, bare Right stays the navigation contract's "attach the
// selected session".
func TestOverviewDispatchBareRightAttachesWhenEmpty(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight})

	if got := content(m); !strings.Contains(got, "> ▏") {
		t.Fatalf("expected bare Right to attach the selected session (empty attach input), got:\n%s", got)
	}
}

// TestOverviewDispatchBareRightNoOpWithoutSessions covers the third case: an
// empty dispatch bar with nothing selected (an empty roster) has nothing to
// attach, so bare Right is a no-op and the overview stays put.
func TestOverviewDispatchBareRightNoOpWithoutSessions(t *testing.T) {
	m := newTestApp(t, newFakeSup(nil))
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight})

	got := content(m)
	if !strings.Contains(got, "No sessions yet") {
		t.Fatalf("expected to stay on the overview with an empty roster, got:\n%s", got)
	}
	if strings.Contains(got, "> ▏") {
		t.Fatalf("expected bare Right with no selection to be a no-op, got:\n%s", got)
	}
}

// TestOverviewDispatchAltRightMovesWordCursor covers Alt+Right moving the
// cursor a word at a time — unlike bare Right, Alt+Right is NOT claimed by
// the navigation contract, so it reaches the input keymap.
func TestOverviewDispatchAltRightMovesWordCursor(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = type_(t, m, "foo bar")
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyHome})
	m = press(t, m, altRight())
	m = press(t, m, tea.KeyPressMsg{Text: "X"})

	if got := content(m); !strings.Contains(got, "❯ fooX▏ bar") {
		t.Fatalf("expected Home then Alt+Right to land after \"foo\", got:\n%s", got)
	}
}

// TestAttachInputAltLeftMovesWordCursor covers Alt+Left moving the cursor a
// word at a time on the attach input — Alt+Right's mirror
// (TestOverviewDispatchAltRightMovesWordCursor covers Alt+Right on the
// dispatch bar).
func TestAttachInputAltLeftMovesWordCursor(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach
	m = type_(t, m, "foo bar")
	m = press(t, m, altLeft())
	m = press(t, m, tea.KeyPressMsg{Text: "X"})

	if got := content(m); !strings.Contains(got, "> foo X▏bar") {
		t.Fatalf("expected Alt+Left to land the cursor at the start of \"bar\", got:\n%s", got)
	}
}

// TestAttachInputHomeEndCtrlAE covers Home/End and their Ctrl-A/Ctrl-E
// equivalents on the attach input.
func TestAttachInputHomeEndCtrlAE(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach
	m = type_(t, m, "hello")

	// Each check includes the "▏" cursor glyph at its actual post-insert
	// position — inserting at the cursor also advances it, so a typed rune
	// always renders immediately followed by "▏".
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyHome})
	m = press(t, m, tea.KeyPressMsg{Text: "X"})
	if got := content(m); !strings.Contains(got, "> X▏hello") {
		t.Fatalf("expected Home to land the cursor at the start, got:\n%s", got)
	}

	m = press(t, m, ctrlKey('e'))
	m = press(t, m, tea.KeyPressMsg{Text: "Y"})
	if got := content(m); !strings.Contains(got, "> XhelloY▏") {
		t.Fatalf("expected Ctrl+E to land the cursor at the end, got:\n%s", got)
	}

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnd})
	m = press(t, m, tea.KeyPressMsg{Text: "Z"})
	if got := content(m); !strings.Contains(got, "> XhelloYZ▏") {
		t.Fatalf("expected End to land the cursor at the end, got:\n%s", got)
	}

	m = press(t, m, ctrlKey('a'))
	m = press(t, m, tea.KeyPressMsg{Text: "W"})
	if got := content(m); !strings.Contains(got, "> W▏XhelloYZ") {
		t.Fatalf("expected Ctrl+A to land the cursor at the start, got:\n%s", got)
	}
}

// TestAttachInputLeftBacksOutOnlyWhenEmpty pins the attach screen's own
// navigation precedence: Left with a non-empty input edits (moves the
// cursor), and only an EMPTY input's Left backs out to the overview.
func TestAttachInputLeftBacksOutOnlyWhenEmpty(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach
	m = type_(t, m, "ab")
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyLeft})
	m = press(t, m, tea.KeyPressMsg{Text: "X"})

	if got := content(m); !strings.Contains(got, "> aX▏b") {
		t.Fatalf("expected Left with text to move the cursor (not back out), got:\n%s", got)
	}

	// Clear back to empty (cursor to the end first — Backspace deletes
	// before the cursor, and the cursor is mid-buffer after the insert
	// above), then Left backs out.
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnd})
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyLeft})
	if got := content(m); !strings.Contains(got, "enter peek") {
		t.Fatalf("expected Left with an empty input to back out to the overview, got:\n%s", got)
	}
}

// TestAttachInputAltBackspaceCtrlWDeleteWord covers Alt+Backspace and Ctrl+W
// both deleting the word before the cursor.
func TestAttachInputAltBackspaceCtrlWDeleteWord(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach
	m = type_(t, m, "foo bar")
	m = press(t, m, altBackspace())

	if got := content(m); !strings.Contains(got, "> foo ▏") {
		t.Fatalf("expected Alt+Backspace to delete \"bar\", got:\n%s", got)
	}

	m = press(t, m, ctrlKey('w'))
	if got := content(m); !strings.Contains(got, "> ▏") {
		t.Fatalf("expected Ctrl+W to delete the remaining word, got:\n%s", got)
	}
}

// TestAttachInputCtrlUCtrlK covers Ctrl+U (delete to line start) and Ctrl+K
// (delete to line end).
func TestAttachInputCtrlUCtrlK(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach
	m = type_(t, m, "hello world")
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyHome})
	m = press(t, m, ctrlKey('e'))

	// Move to just after "hello" (6 lefts from the end lands right before
	// the space... "hello world" has 11 runes; end=11, "hello" ends at 5).
	for i := 0; i < 6; i++ {
		m = press(t, m, tea.KeyPressMsg{Code: tea.KeyLeft})
	}
	m = press(t, m, ctrlKey('k'))
	if got := content(m); !strings.Contains(got, "> hello▏") {
		t.Fatalf("expected Ctrl+K to delete from the cursor to the end, got:\n%s", got)
	}

	// Buffer is now "hello" (cursor at the end, 5). End is a no-op here;
	// typing " again" inserts at the cursor, landing on "hello again" with
	// the cursor back at the end (11). 6 Lefts from there lands right back
	// at index 5 — between "hello" and " again" — the same boundary Ctrl+K
	// used above.
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnd})
	m = type_(t, m, " again")
	for i := 0; i < 6; i++ {
		m = press(t, m, tea.KeyPressMsg{Code: tea.KeyLeft})
	}
	m = press(t, m, ctrlKey('u'))
	if got := content(m); !strings.Contains(got, "> ▏ again") {
		t.Fatalf("expected Ctrl+U to delete from the start to the cursor, got:\n%s", got)
	}
}

// TestOverviewEscapeClearsWholeBufferRegardlessOfCursor is a regression
// guard: Escape must clear the WHOLE dispatch-bar buffer even when the
// cursor sits mid-buffer, not just the text before the cursor (a repeated
// Backspace-from-cursor loop would stall once the cursor, but not the
// buffer, reached 0).
func TestOverviewEscapeClearsWholeBufferRegardlessOfCursor(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = type_(t, m, "abc")
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyLeft})
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyLeft}) // cursor now mid-buffer, at index 1

	// Runs Update on a goroutine (not press(t, ...): testing.T's FailNow-based
	// helpers aren't safe to call off the test goroutine) so a regression to
	// the old repeated-Backspace loop hangs this goroutine instead of the
	// test itself, and the timeout below catches it deterministically.
	done := make(chan tea.Model, 1)
	go func() {
		next, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
		if cmd != nil {
			next, _ = next.Update(cmd())
		}
		done <- next
	}()

	select {
	case m = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Escape with a mid-buffer cursor did not return — regression: infinite Backspace loop")
	}

	if !strings.Contains(content(m), "describe a task for a new session") {
		t.Fatalf("expected Escape to clear the dispatch bar back to its placeholder, got:\n%s", content(m))
	}
}
