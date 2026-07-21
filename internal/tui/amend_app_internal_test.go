package tui

// amend_app_internal_test.go covers Tab-to-amend end to end through App's
// Update loop — the key routing (dialog.go), the state transitions
// (amend.go), and the reply the commit actually sends. It lives in package
// tui for the same reason app_internal_test.go does: seeding a pending
// approval needs the unexported sessEventMsg. The editor's own keymap is
// unit-tested in amend_internal_test.go.

import (
	"encoding/json"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/tui/testkit"
)

// requestApprovalSpec is [requestApproval] with a caller-chosen spec — the
// seam the no-edit-target and full-input-preservation cases need.
func requestApprovalSpec(t *testing.T, a App, id string, spec map[string]any) App {
	t.Helper()
	mdl, _ := a.Update(sessEventMsg{
		id: a.sessID,
		ev: event.NewPermissionRequested(a.sessID, id, "bash", spec, GoldenTrace()),
	})
	return mdl.(App)
}

// amendingApp attaches, raises a permission request over spec, and opens the
// amend editor with Tab.
func amendingApp(t *testing.T, sup *internalFakeSup, spec map[string]any) App {
	t.Helper()
	a := requestApprovalSpec(t, attachForDialogTest(t, sup), "perm-1", spec)
	mdl, _ := a.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	a = mdl.(App)
	if !a.sess.AmendingApproval() {
		t.Fatal("expected the amend editor open after tab")
	}
	return a
}

// TestGoldenAppApprovalAmending locks the editor's frame: the header,
// attribution, body, and rationale still showing above, the decision row
// replaced by the editable command with its cursor, the no-re-validation
// warning, and the editor's own key hints.
func TestGoldenAppApprovalAmending(t *testing.T) {
	a := amendingApp(t, newInternalFakeSup(GoldenRoster()), map[string]any{"cmd": "rm -rf /tmp/x"})
	testkit.AssertGolden(t, "approval_amending", a.render())
}

// TestGoldenAppApprovalAmendingRemember is the same frame with remember
// toggled on BEFORE tab — the state that adds the sentence about the
// standing grant pinning the EDITED command.
func TestGoldenAppApprovalAmendingRemember(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := requestApprovalSpec(t, attachForDialogTest(t, sup), "perm-1", map[string]any{"cmd": "rm -rf /tmp/x"})

	mdl, _ := a.Update(tea.KeyPressMsg{Text: "r"})
	a = mdl.(App)
	mdl, _ = a.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	a = mdl.(App)

	if !a.sess.AmendingApproval() || !pendingRemember(t, a) {
		t.Fatal("expected the amend editor open with remember on")
	}
	testkit.AssertGolden(t, "approval_amending_remember", a.render())
}

// TestGoldenStyledAppApprovalAmending is the color oracle for the warning: a
// plain golden proves the sentence is there, but only a colored render can
// prove it is rendered in the theme's WARN style rather than as ordinary body
// text. A no-re-validation warning that reads like prose is a warning nobody
// sees.
func TestGoldenStyledAppApprovalAmending(t *testing.T) {
	a := newColorAppWithApproval(t, testkit.ColorTheme())
	mdl, _ := a.Update(tea.KeyPressMsg{Text: "r"})
	a = mdl.(App)
	mdl, _ = a.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	a = mdl.(App)
	if !a.sess.AmendingApproval() {
		t.Fatal("expected the amend editor open after tab")
	}
	testkit.AssertGoldenStyled(t, "approval_amending", a.render())
}

// TestAppApprovalAmendCapturesEveryKey pins the key capture: with the editor
// open, the decision keys are just characters. Typing "a" must edit the
// command, not allow the call — resolving a permission request because the
// user typed a letter into a text field would be the worst possible bug in
// this feature.
func TestAppApprovalAmendCapturesEveryKey(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := amendingApp(t, sup, map[string]any{"cmd": "ls"})

	for _, k := range []string{"a", "d", "r", "y", "n", "1", "2"} {
		mdl, cmd := a.Update(tea.KeyPressMsg{Text: k})
		a = mdl.(App)
		if cmd != nil {
			t.Fatalf("key %q issued a Cmd while amending; want the key consumed by the editor", k)
		}
	}

	if !a.sess.AmendingApproval() {
		t.Fatal("a decision key closed the editor; want every key routed to it")
	}
	if !a.sess.HasPendingApproval() {
		t.Fatal("a decision key resolved the request while amending")
	}
	if got := a.sess.pending.amend.Text(); got != "lsadryn12" {
		t.Errorf("editor text = %q, want the decision keys typed into it", got)
	}
	if len(sup.replies) != 0 {
		t.Errorf("sup.replies = %+v, want none — no key may answer the request while amending", sup.replies)
	}
}

// TestAppApprovalAmendEscCancels pins esc: the editor closes, the request
// stays pending with its ORIGINAL spec, and nothing is sent. A cancelled
// amend must be indistinguishable from never having pressed tab.
func TestAppApprovalAmendEscCancels(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	spec := map[string]any{"cmd": "rm -rf /tmp/x", "timeout": float64(120)}
	a := amendingApp(t, sup, spec)

	mdl, _ := a.Update(tea.KeyPressMsg{Text: "!!!"})
	a = mdl.(App)
	mdl, cmd := a.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	a = mdl.(App)

	if cmd != nil {
		t.Error("esc while amending issued a Cmd; no reply is sent")
	}
	if a.sess.AmendingApproval() {
		t.Error("esc left the amend editor open")
	}
	if !a.sess.HasPendingApproval() {
		t.Fatal("esc while amending dismissed the request; it must stay pending")
	}
	if got := a.sess.pending.spec["cmd"]; got != "rm -rf /tmp/x" {
		t.Errorf("pending spec cmd = %v, want the original command untouched", got)
	}
	if len(sup.replies) != 0 {
		t.Errorf("sup.replies = %+v, want none after esc", sup.replies)
	}
	// The prompt is back to its decision row, not the editor's hints.
	if got := a.render(); !strings.Contains(got, "Do you want to proceed?") {
		t.Errorf("esc did not restore the decision row:\n%s", got)
	}
}

// TestAppApprovalAmendCommitSendsAmendedAllow pins ctrl+s: exactly one reply,
// allow, carrying the FULL original spec with only the command replaced.
// Both halves matter — the SDK substitutes Input wholesale, so a reply
// carrying the command alone would erase the other arguments.
func TestAppApprovalAmendCommitSendsAmendedAllow(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := amendingApp(t, sup, map[string]any{"cmd": "rm -rf /tmp/x", "timeout": float64(120)})

	mdl, _ := a.Update(tea.KeyPressMsg{Text: " --dry-run"})
	a = mdl.(App)
	mdl, cmd := a.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	a = mdl.(App)

	if a.sess.HasPendingApproval() {
		t.Fatal("expected the pending approval cleared immediately on ctrl+s")
	}
	if cmd == nil {
		t.Fatal("expected a Reply cmd after ctrl+s")
	}
	cmd()

	if len(sup.replies) != 1 {
		t.Fatalf("sup.replies = %+v, want exactly one entry", sup.replies)
	}
	got := sup.replies[0]
	if got.id != "perm-1" || !got.allow || got.remember {
		t.Errorf("reply = %+v, want an allow for perm-1 without remember", got)
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(got.input), &input); err != nil {
		t.Fatalf("reply input %q is not JSON: %v", got.input, err)
	}
	want := map[string]any{"cmd": "rm -rf /tmp/x --dry-run", "timeout": float64(120)}
	if len(input) != len(want) || input["cmd"] != want["cmd"] || input["timeout"] != want["timeout"] {
		t.Errorf("reply input = %v, want %v", input, want)
	}
}

// TestAppApprovalAmendCommitHonorsRemember pins that a commit carries the
// remember toggle set before the editor opened — the case whose consequence
// (the standing grant pins the EDITED call) the editor's warning spells out.
func TestAppApprovalAmendCommitHonorsRemember(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := requestApprovalSpec(t, attachForDialogTest(t, sup), "perm-1", map[string]any{"cmd": "ls"})

	mdl, _ := a.Update(tea.KeyPressMsg{Text: "r"})
	a = mdl.(App)
	mdl, _ = a.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	a = mdl.(App)
	_, cmd := a.Update(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	if cmd == nil {
		t.Fatal("expected a Reply cmd after ctrl+s")
	}
	cmd()

	if len(sup.replies) != 1 {
		t.Fatalf("sup.replies = %+v, want exactly one entry", sup.replies)
	}
	if got := sup.replies[0]; !got.allow || !got.remember || got.input != `{"cmd":"ls"}` {
		t.Errorf("reply = %+v, want an amended allow with remember=true", got)
	}
}

// TestAppApprovalAmendNoEditTargetIsANoOp pins the refusal path: a call whose
// spec carries no command-ish key leaves tab inert, with a status note saying
// why rather than an empty editor.
func TestAppApprovalAmendNoEditTargetIsANoOp(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := requestApprovalSpec(t, attachForDialogTest(t, sup), "perm-1", map[string]any{"query": "gofer", "limit": float64(5)})

	mdl, cmd := a.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	a = mdl.(App)

	if cmd != nil {
		t.Error("tab with nothing to amend issued a Cmd")
	}
	if a.sess.AmendingApproval() {
		t.Fatal("tab opened an editor over a spec with no command to edit")
	}
	if !a.sess.HasPendingApproval() {
		t.Fatal("tab dismissed the request")
	}
	if a.status == "" {
		t.Fatal("tab with nothing to amend left no status note")
	}
	if !strings.Contains(a.render(), a.status) {
		t.Errorf("the status note %q never reached the frame:\n%s", a.status, a.render())
	}
	if len(sup.replies) != 0 {
		t.Errorf("sup.replies = %+v, want none", sup.replies)
	}
}

// TestAppApprovalAmendCtrlEIsLineMotionNotExplain pins the deliberate
// resolution of the one key ctrl+e and the amend editor both want (PR #206
// bound it to explain on this prompt; the editor inherits the app's readline
// keymap, where it is jump-to-end-of-line).
//
// Inside the editor it is LINE MOTION: no ExplainPermission call is made, and
// the cursor lands at the end of the line. Firing an explain here would
// repaint the rationale under a live cursor and resize the block mid-edit; the
// explain loses nothing by waiting, since esc restores the prompt with the
// request still pending — which the second half of this test proves.
func TestAppApprovalAmendCtrlEIsLineMotionNotExplain(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	sup.explainRationale = explainedRationale()
	a := amendingApp(t, sup, map[string]any{"cmd": "rm -rf /tmp/x"})

	// Move off the end so a jump-to-end is observable, then press ctrl+e.
	mdl, _ := a.Update(tea.KeyPressMsg{Code: tea.KeyHome})
	a = mdl.(App)
	mdl, cmd := a.Update(ctrlE)
	a = mdl.(App)

	if cmd != nil {
		t.Fatal("ctrl+e while amending issued a Cmd; want the key consumed by the editor as line motion")
	}
	if len(sup.explains) != 0 {
		t.Errorf("sup.explains = %+v, want none — ctrl+e in the editor is line motion, not explain", sup.explains)
	}
	if a.sess.ApprovalExplaining() {
		t.Error("ctrl+e while amending marked the prompt as explaining")
	}
	if !a.sess.AmendingApproval() {
		t.Fatal("ctrl+e closed the amend editor")
	}
	if got := a.sess.pending.amend.cur().Cursor(); got != len("rm -rf /tmp/x") {
		t.Errorf("cursor = %d, want the line end %d (ctrl+e is MoveEnd in a text field)", got, len("rm -rf /tmp/x"))
	}

	// And the explain is not lost: esc leaves the editor, and ctrl+e explains.
	mdl, _ = a.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	a = mdl.(App)
	mdl, cmd = a.Update(ctrlE)
	a = mdl.(App)
	if cmd == nil {
		t.Fatal("ctrl+e after leaving the editor issued no explain Cmd")
	}
	if _, _ = a.Update(cmd()); len(sup.explains) != 1 {
		t.Errorf("sup.explains = %+v, want exactly one after esc + ctrl+e", sup.explains)
	}
}

// TestAppApprovalExplainLandingMidEditKeepsTheEditor covers the one ordering
// that still puts an explain result underneath an open editor: ctrl+e, then
// tab before the answer lands. The rationale block swaps to the agent's answer
// (that is what was asked for) and the editor — its text, its cursor, its
// warning — is untouched. An in-flight read must never cost the user the edit
// they were typing.
func TestAppApprovalExplainLandingMidEditKeepsTheEditor(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	sup.explainRationale = explainedRationale()
	a := requestApprovalSpec(t, attachForDialogTest(t, sup), "perm-1", map[string]any{"cmd": "rm -rf /tmp/x"})

	mdl, cmd := a.Update(ctrlE)
	a = mdl.(App)
	if cmd == nil {
		t.Fatal("expected an ExplainPermission cmd after ctrl+e")
	}
	mdl, _ = a.Update(tea.KeyPressMsg{Code: tea.KeyTab}) // tab before the answer lands
	a = mdl.(App)
	mdl, _ = a.Update(tea.KeyPressMsg{Text: " --dry-run"})
	a = mdl.(App)
	if !a.sess.AmendingApproval() {
		t.Fatal("expected the amend editor open")
	}

	mdl, _ = a.Update(cmd()) // the explain answer lands under the open editor
	a = mdl.(App)

	if !a.sess.AmendingApproval() {
		t.Fatal("the explain result closed the amend editor")
	}
	if got := a.sess.pending.amend.Text(); got != "rm -rf /tmp/x --dry-run" {
		t.Errorf("editor text = %q, want the in-progress edit preserved", got)
	}
	frame := a.render()
	if !strings.Contains(frame, "sandbox profile denies deletes") {
		t.Errorf("the agent's rationale never reached the frame:\n%s", frame)
	}
	if !strings.Contains(flattenPrompt(frame), warnAmendOverride) {
		t.Errorf("the explain repaint dropped the amend warning:\n%s", frame)
	}
}

// TestAppApprovalAmendCtrlCStillQuits pins the one key the editor does NOT
// swallow: ctrl+c quits from here exactly as it does everywhere else.
func TestAppApprovalAmendCtrlCStillQuits(t *testing.T) {
	a := amendingApp(t, newInternalFakeSup(GoldenRoster()), map[string]any{"cmd": "ls"})
	if _, cmd := a.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}); cmd == nil {
		t.Fatal("ctrl+c while amending issued no Cmd; want tea.Quit")
	}
}

// TestAppApprovalAmendAcceptsPaste pins paste.go's rule at this surface:
// anything a typed rune would edit, a paste edits. The pending-approval
// prompt swallows pastes, but the editor inside it is a real text field.
func TestAppApprovalAmendAcceptsPaste(t *testing.T) {
	a := amendingApp(t, newInternalFakeSup(GoldenRoster()), map[string]any{"cmd": "go test"})

	mdl, _ := a.Update(tea.PasteMsg{Content: " -race \\\n  -count=1"})
	a = mdl.(App)

	if got := a.sess.pending.amend.Text(); got != "go test -race \\\n  -count=1" {
		t.Errorf("editor text after paste = %q, want the pasted lines inserted", got)
	}
}

// TestAppApprovalPasteStillSwallowedWithoutEditor is the other half: with no
// editor open the prompt still swallows a paste, exactly as before.
func TestAppApprovalPasteStillSwallowedWithoutEditor(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := requestApproval(t, attachForDialogTest(t, sup), "perm-1")

	mdl, _ := a.Update(tea.PasteMsg{Content: "rm -rf /"})
	a = mdl.(App)

	if got := a.render(); strings.Contains(got, "rm -rf /\n") {
		t.Errorf("a paste landed somewhere while an unamended approval was pending:\n%s", got)
	}
	if !a.sess.HasPendingApproval() {
		t.Fatal("a paste resolved the pending approval")
	}
}
