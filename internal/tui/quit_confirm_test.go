package tui_test

// quit_confirm_test.go covers the ctrl+c double-tap quit confirm (gofer#314)
// at the global-keymap and command-panel dispatch sites, driven entirely
// through App's exported Update/View surface — it reuses app_test.go's
// fakeSup/press/content/newTestApp/ctrl helpers and command_test.go's
// dispatchSlash. The approval and decision overlay sites need App's
// unexported messages to seed a pending prompt, so their tests live in
// quit_confirm_internal_test.go (package tui) instead; the single-session
// [tui.Program] adapter's mirrored implementation is covered separately in
// adapter_quit_test.go.

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/tui"
)

// quitArmedNote is the status-line text a first ctrl+c shows while armed
// (app.go's quitArmedNote, unexported — spelled once here rather than
// imported).
const quitArmedNote = "ctrl-c again to quit"

// TestGlobalCtrlCFirstPressArmsNoAction is the mutation anchor for the
// double-tap confirm's FIRST half: one ctrl+c on the roster must issue no
// Cmd (in particular, not tea.Quit) and must show the armed note on the
// status line. Neutralize confirmQuit's arm-instead-of-quit branch and this
// goes red.
func TestGlobalCtrlCFirstPressArmsNoAction(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))

	m, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: ctrl})
	if cmd != nil {
		t.Fatalf("first ctrl-c returned %T, want no Cmd — it must only arm", cmd)
	}
	if got := content(m); !strings.Contains(got, quitArmedNote) {
		t.Fatalf("expected the status line to show %q; got:\n%s", quitArmedNote, got)
	}
}

// TestGlobalCtrlCSecondPressQuits verifies the SECOND ctrl+c, pressed
// immediately after the first with no intervening key, returns tea.Quit.
func TestGlobalCtrlCSecondPressQuits(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))

	m, armCmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: ctrl}) // arm
	if armCmd != nil {
		t.Fatal("first ctrl-c returned a Cmd; want it to only arm")
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: ctrl}) // confirm
	if !isQuit(cmd) {
		t.Fatalf("second ctrl-c returned %T, want tea.Quit", cmd)
	}
}

// TestGlobalCtrlCOtherKeyDisarms is the SAFETY test, the ctrl+c sibling of
// app_test.go's TestNavCtrlXSelectionMoveResetsConfirm: an intervening key
// between the two ctrl+c presses must clear the arm, so a THIRD press (after
// the disarming key) re-arms rather than quitting. Removing the central
// disarm in App.Update turns this red (a bare ↓ would leave the arm standing
// and the next ctrl+c would quit immediately instead of re-arming).
func TestGlobalCtrlCOtherKeyDisarms(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))

	m, _ = m.Update(tea.KeyPressMsg{Code: 'c', Mod: ctrl}) // arm
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyDown})    // any other key: disarm

	if got := content(m); strings.Contains(got, quitArmedNote) {
		t.Fatalf("an intervening key must clear the armed note; got:\n%s", got)
	}

	// A fresh ctrl-c after the disarming key must RE-ARM, not act as the
	// confirming second press.
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: ctrl})
	if isQuit(cmd) {
		t.Fatal("ctrl-c after a disarming key returned tea.Quit; want it to re-arm instead")
	}
}

// TestPanelCtrlCArmsThenConfirms is the modal-path coverage for the command
// panel overlay (gofer#314's enumerated dialog.go/panel.go sites): ctrl+c
// over an open panel goes through [App.handlePanelKey], not the global
// table, and must show the same double-tap shape.
func TestPanelCtrlCArmsThenConfirms(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = dispatchSlash(t, m, "/status")
	if !strings.Contains(content(m), "[Status]") {
		t.Fatalf("expected the command panel open on the Status tab; got:\n%s", content(m))
	}

	m, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: ctrl})
	if cmd != nil {
		t.Fatalf("first ctrl-c over an open panel returned %T, want no Cmd", cmd)
	}
	if !strings.Contains(content(m), "[Status]") {
		t.Fatal("first ctrl-c closed the panel; it must only arm the quit confirm")
	}

	_, cmd = m.Update(tea.KeyPressMsg{Code: 'c', Mod: ctrl})
	if !isQuit(cmd) {
		t.Fatalf("second ctrl-c over an open panel returned %T, want tea.Quit", cmd)
	}
}

// TestPanelCtrlCDisarmsThenEscStillCloses is the "no modal state becomes
// inescapable" guard for the panel: arming ctrl+c and then disarming it (a
// tab switch, the panel's own navigation) must not cost the panel its
// un-confirmed Esc close — the property that makes requiring a second ctrl+c
// safe in the first place.
func TestPanelCtrlCDisarmsThenEscStillCloses(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = dispatchSlash(t, m, "/status")

	m, _ = m.Update(tea.KeyPressMsg{Code: 'c', Mod: ctrl}) // arm
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight})   // switch tabs: disarms
	if got := content(m); strings.Contains(got, quitArmedNote) {
		t.Fatalf("switching tabs must clear the armed note; got:\n%s", got)
	}

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEscape}) // the panel's own way out
	if strings.Contains(content(m), "[Usage]") || strings.Contains(content(m), "[Status]") {
		t.Fatalf("esc after a disarmed ctrl-c no longer closes the panel; got:\n%s", content(m))
	}
}
