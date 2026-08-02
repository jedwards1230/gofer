package tui

// quit_confirm_internal_test.go covers the ctrl+c double-tap quit confirm
// (gofer#314) at the two dialog.go overlay sites the issue enumerates: the
// pending-approval prompt ([App.handleApprovalKey], including its nested
// amend editor) and the pending-decision prompt ([App.handleDecisionKey]).
// Both need App's unexported sessEventMsg/decisionSubReadyMsg plumbing to
// seed a genuinely pending prompt, so they live in package tui — see
// app_internal_test.go's file doc for why. The global-keymap and
// command-panel sites are covered in quit_confirm_test.go (package tui_test);
// the standalone [Program] adapter's mirrored implementation is covered in
// adapter_quit_test.go.

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

var ctrlC = tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}

// TestApprovalCtrlCArmsThenConfirms is the modal-path coverage the issue's
// definition of done calls out by name: the FIRST ctrl+c over a pending
// approval must not dismiss it or reply to it, only arm; the SECOND,
// immediately following, must return tea.Quit and still answer nothing —
// quitting is not the same as denying.
func TestApprovalCtrlCArmsThenConfirms(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDialogTest(t, sup)
	a = requestApproval(t, a, "perm-1")

	mdl, cmd := a.Update(ctrlC)
	a = mdl.(App)
	if cmd != nil {
		t.Fatalf("first ctrl+c over a pending approval returned a Cmd (%T), want none", cmd)
	}
	if !a.sess.HasPendingApproval() {
		t.Fatal("first ctrl+c dismissed the pending approval; it must only arm the quit confirm")
	}
	if !strings.Contains(a.status, quitArmedNote) {
		t.Fatalf("status = %q, want it to contain %q", a.status, quitArmedNote)
	}

	_, cmd = a.Update(ctrlC)
	if !isQuitCmd(cmd) {
		t.Fatal("second ctrl+c over a pending approval did not return tea.Quit")
	}
	if len(sup.replies) != 0 {
		t.Errorf("sup.replies = %+v, want none — quitting must not answer the pending approval", sup.replies)
	}
}

// TestApprovalCtrlCDisarmsThenEscStillExits is the "no modal state becomes
// inescapable" guard for the approval prompt: arming ctrl+c and then
// disarming it (any other key — here, the ↓ that moves the caret) must not
// cost the prompt its own un-confirmed Esc exit, which is what makes
// requiring a second ctrl+c to quit safe in the first place.
func TestApprovalCtrlCDisarmsThenEscStillExits(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDialogTest(t, sup)
	a = requestApproval(t, a, "perm-1")

	mdl, _ := a.Update(ctrlC) // arm
	a = mdl.(App)
	mdl, _ = a.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // moves the caret, disarms
	a = mdl.(App)
	if a.status != "" {
		t.Errorf("status = %q, want cleared after a non-ctrl+c key", a.status)
	}

	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyEscape})
	if a.sess.HasPendingApproval() {
		t.Fatal("esc after a disarmed ctrl+c no longer dismisses the approval prompt")
	}
}

// TestApprovalAmendCtrlCArmsThenConfirms pins the amend editor's special
// case: dialog.go's handleApprovalKey intercepts ctrl+c BEFORE routing into
// the editor, so the double-tap confirm applies there too (not "a" typed
// into the command buffer), and a first press must leave the editor's text
// untouched.
func TestApprovalAmendCtrlCArmsThenConfirms(t *testing.T) {
	a := amendingApp(t, newInternalFakeSup(GoldenRoster()), map[string]any{"cmd": "ls"})
	before := a.sess.pending.amend.Text()

	mdl, cmd := a.Update(ctrlC)
	a = mdl.(App)
	if cmd != nil {
		t.Fatalf("first ctrl+c while amending returned a Cmd (%T), want none", cmd)
	}
	if !a.sess.AmendingApproval() {
		t.Fatal("first ctrl+c closed the amend editor; it must only arm the quit confirm")
	}
	if got := a.sess.pending.amend.Text(); got != before {
		t.Errorf("amend editor text changed to %q after ctrl+c; want %q untouched", got, before)
	}

	if _, cmd := a.Update(ctrlC); !isQuitCmd(cmd) {
		t.Fatal("second ctrl+c while amending did not return tea.Quit")
	}
}

// TestDecisionCtrlCArmsThenConfirms is the decision prompt's twin of
// TestApprovalCtrlCArmsThenConfirms: quitting on the confirmed press must
// leave the blocked ask_user call unanswered, not silently resolve it.
func TestDecisionCtrlCArmsThenConfirms(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDecisionTest(t, sup)
	a, blocked := openDecision(t, sup, a)

	mdl, cmd := a.Update(ctrlC)
	a = mdl.(App)
	if cmd != nil {
		t.Fatalf("first ctrl+c over a pending decision returned a Cmd (%T), want none", cmd)
	}
	if !a.sess.HasPendingDecision() {
		t.Fatal("first ctrl+c dismissed the pending decision; it must only arm the quit confirm")
	}

	_, cmd = a.Update(ctrlC)
	if !isQuitCmd(cmd) {
		t.Fatal("second ctrl+c over a pending decision did not return tea.Quit")
	}
	blocked.stillBlocked(t)
}

// TestDecisionCtrlCDisarmsThenEscStillExits is
// TestApprovalCtrlCDisarmsThenEscStillExits's decision-prompt twin.
func TestDecisionCtrlCDisarmsThenEscStillExits(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDecisionTest(t, sup)
	a, blocked := openDecision(t, sup, a)

	mdl, _ := a.Update(ctrlC) // arm
	a = mdl.(App)
	mdl, _ = a.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // moves the cursor, disarms
	a = mdl.(App)

	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyEscape})
	if a.sess.HasPendingDecision() {
		t.Fatal("esc after a disarmed ctrl+c no longer clears the decision prompt")
	}
	if len(sup.interrupts) != 1 || sup.interrupts[0] != a.sessID {
		t.Errorf("sup.interrupts = %+v; want exactly one Interrupt for %q — esc under the default scope interrupts the turn", sup.interrupts, a.sessID)
	}
	blocked.stillBlocked(t) // default scope interrupts the turn; it does not answer the gate
}
