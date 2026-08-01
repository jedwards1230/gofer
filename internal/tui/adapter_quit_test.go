package tui_test

// adapter_quit_test.go covers [tui.Program]'s OWN ctrl+c double-tap quit
// confirm (gofer#314) — the single-session TUI cmd/gofer's driveTUI drives
// for `gofer run`/`gofer resume`. Program is architecturally a wholly
// separate tea.Model from App (see its type doc in adapter.go), so it
// mirrors rather than shares App's confirmQuit; these tests pin that the
// mirrored copy behaves identically, and that esc — the surface's only other
// way out — stays a single, un-confirmed quit throughout.

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

func newTestProgram(t *testing.T) tea.Model {
	t.Helper()
	var m tea.Model = tui.NewProgram(theme.Test())
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	return m
}

// TestProgramCtrlCArmsThenConfirms is the mutation anchor for Program's own
// arm/confirm halves: the first ctrl+c must issue no Cmd and show the armed
// note in the frame; the second, immediately following, must return
// tea.Quit.
func TestProgramCtrlCArmsThenConfirms(t *testing.T) {
	m := newTestProgram(t)

	m, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if cmd != nil {
		t.Fatalf("first ctrl-c returned %T, want no Cmd — it must only arm", cmd)
	}
	if got := m.View().Content; !strings.Contains(got, "ctrl-c again to quit") {
		t.Fatalf("expected the armed note in the frame; got:\n%s", got)
	}

	_, cmd = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if !isQuit(cmd) {
		t.Fatalf("second ctrl-c returned %T, want tea.Quit", cmd)
	}
}

// TestProgramCtrlCOtherKeyDisarms is the safety half: typing between the two
// ctrl+c presses must clear the arm, so a fresh ctrl+c after it re-arms
// rather than quitting immediately.
func TestProgramCtrlCOtherKeyDisarms(t *testing.T) {
	m := newTestProgram(t)

	m, _ = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}) // arm
	m, _ = m.Update(tea.KeyPressMsg{Text: "h"})                   // ordinary typing: disarm

	if got := m.View().Content; strings.Contains(got, "ctrl-c again to quit") {
		t.Fatalf("typing must clear the armed note; got:\n%s", got)
	}

	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if isQuit(cmd) {
		t.Fatal("ctrl-c after a disarming key returned tea.Quit; want it to re-arm instead")
	}
}

// TestProgramEscStaysUnconfirmedWhileArmed verifies esc — this surface's
// only immediate cancel/quit — is untouched by the ctrl+c arm: it must keep
// quitting on ONE press even with a ctrl+c arm standing, since there is no
// other screen or overlay for this minimal view to back out to first (see
// InterruptMsg's doc in adapter.go).
func TestProgramEscStaysUnconfirmedWhileArmed(t *testing.T) {
	m := newTestProgram(t)

	m, _ = m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}) // arm
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if !isQuit(cmd) {
		t.Fatalf("esc while armed returned %T, want tea.Quit", cmd)
	}
}
