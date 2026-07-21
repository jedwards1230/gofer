package tui

// dialog.go holds [App]'s key handling for a pending permission request. The
// pending-approval state itself, and its inline render, live on [Model] (see
// approval.go); this file is just the App-level routing that captures input
// while one is active and turns a decision into a [Supervisor.Reply] call.
// It replaces gofer's first interactive TUI dialog — a centered overlay box
// for the "approvals relay + phone approval UX" item — with key handling for
// the inline prompt now rendered in-flow by [Model.View].

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/acp"
)

// handleApprovalKey handles key presses while the peeked/attached session
// has a pending approval (see [Model.HasPendingApproval]), capturing all
// input until it resolves or is dismissed — [App.Update] routes here instead
// of the per-screen handlers whenever that's true. Keymap: a/y allow, d/n
// deny (both reply immediately and dismiss the prompt), r toggles remember,
// tab opens the inline amend editor (see [App.beginAmend]), esc dismisses
// the prompt without replying — the underlying request stays pending
// server-side (e.g. a matching [event.PermissionResolved] from another
// attached client, or a later re-attach to the same session, can still
// surface or settle it).
//
// 1/2 are aliases for allow/deny because the prompt itself renders the
// choices numbered ("1. [a] Yes   2. [d] No" — see renderApprovalPrompt): a
// rendered affordance that did nothing when pressed would be a lie, and
// numbered answers are the reflex a confirm prompt trains.
//
// ctrl+e is the one key here that answers nothing: it fetches the agent's own
// gating rationale (see [App.explainApproval]) and leaves the request exactly
// as it found it — pending, and still awaiting the human's decision.
//
// With the amend editor open every key EXCEPT ctrl+c goes to the editor
// ([App.handleAmendKey]) — a/d/r/1/2 type those characters into the command
// rather than resolving the request, which is the only sane behavior for a
// text field and is pinned by a test. ctrl+e is included in that: inside the
// editor it is the readline "jump to end of line" binding every other text
// surface in this app gives it (see applyInputKey), NOT an explain. That is a
// deliberate choice, not an oversight — an explain fired mid-edit would
// repaint the rationale block underneath a live cursor and change the prompt's
// height while the user is typing into it, and the explain loses nothing by
// waiting: esc closes the editor with the request still pending and the spec
// untouched, and ctrl+e works again the moment it does.
func (a App) handleApprovalKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.Key()
	if key.Mod.Contains(tea.ModCtrl) && key.Code == 'c' {
		return a, tea.Quit
	}
	if a.sess.AmendingApproval() {
		return a.handleAmendKey(key)
	}
	switch {
	case key.Text == "a" || key.Text == "y" || key.Text == "1":
		return a.resolveApproval(true)

	case key.Text == "d" || key.Text == "n" || key.Text == "2":
		return a.resolveApproval(false)

	case key.Text == "r":
		a.sess = a.sess.ToggleApprovalRemember()
		return a, nil

	case key.Mod.Contains(tea.ModCtrl) && key.Code == 'e':
		return a.explainApproval()

	case key.Code == tea.KeyTab:
		return a.beginAmend()

	case key.Code == tea.KeyEscape:
		a.sess = a.sess.DismissApproval()
		return a, nil
	}
	return a, nil
}

// beginAmend opens the inline editor over the gated call's command body.
// A call whose spec carries no editable command key (an edit tool's
// structured payload, a search query object) has nothing sensible to put in
// an editor, so Tab is a no-op there with a status note saying why — an
// empty editor whose commit would blank the call is strictly worse.
func (a App) beginAmend() (tea.Model, tea.Cmd) {
	next, ok := a.sess.BeginApprovalAmend()
	if !ok {
		a.setStatus(sevWarn, "This call has no editable command to amend.")
		return a, nil
	}
	a.sess = next
	return a, nil
}

// handleAmendKey routes one key press while the amend editor is open:
// ctrl+s approves the edited call, esc closes the editor leaving the request
// pending and the spec untouched, and everything else is an editing
// operation (see [amendEditor.applyKey], which reuses the app's own text
// keymap for the within-line keys).
//
// "Everything else" deliberately includes ctrl+e: in here it is the shared
// keymap's jump-to-end-of-line, not the prompt's explain (see
// handleApprovalKey's doc for why). An explain already in flight when the
// editor opened still lands normally — [App.applyPermissionExplained] only
// swaps the rationale block, leaving the editor and its text alone.
func (a App) handleAmendKey(key tea.Key) (tea.Model, tea.Cmd) {
	switch {
	case key.Mod.Contains(tea.ModCtrl) && key.Code == 's':
		return a.commitAmend()

	case key.Code == tea.KeyEscape:
		a.sess = a.sess.CancelApprovalAmend()
		return a, nil
	}
	a.sess = a.sess.ApplyApprovalAmendKey(key)
	return a, nil
}

// commitAmend approves the pending request with the editor's replacement
// input (ctrl+s), honoring whatever the remember toggle was set to before
// the editor opened — the SDK then runs the call with the edited arguments,
// and a remembered amend grants the EDITED call (see
// [PermissionDecision.Input]).
//
// The reply carries the call's FULL original spec with only the edited key
// replaced (see [Model.AmendedInput]): the SDK substitutes Input wholesale,
// so sending the command alone would erase every other argument. A spec that
// cannot be re-marshalled sends nothing at all and leaves the editor open
// with the failure on the status line — a permission reply is not the place
// to guess.
func (a App) commitAmend() (tea.Model, tea.Cmd) {
	id, remember, ok := a.sess.PendingApproval()
	if !ok {
		return a, nil
	}
	input, ok, err := a.sess.AmendedInput()
	if err != nil {
		a.setStatus(sevDanger, "amend: "+err.Error())
		return a, nil
	}
	if !ok {
		return a, nil
	}
	a.sess = a.sess.DismissApproval()
	return a, a.doReply(a.sessID, id, PermissionDecision{Allow: true, Remember: remember, Input: input})
}

// resolveApproval sends the pending approval's verdict via [Supervisor.Reply]
// and dismisses it immediately — an optimistic local dismiss. The matching
// [event.PermissionResolved], when it later arrives over the session's event
// stream, is then a no-op in [Model.Ingest], since the pending id it's
// looking for no longer matches (or nothing is pending at all).
func (a App) resolveApproval(allow bool) (tea.Model, tea.Cmd) {
	id, remember, ok := a.sess.PendingApproval()
	if !ok {
		return a, nil
	}
	a.sess = a.sess.DismissApproval()
	return a, a.doReply(a.sessID, id, PermissionDecision{Allow: allow, Remember: remember})
}

// doReply answers a pending permission request via the Supervisor.
func (a App) doReply(sessionID, id string, d PermissionDecision) tea.Cmd {
	return func() tea.Msg {
		err := a.sup.Reply(context.Background(), sessionID, id, d)
		return opDoneMsg{err: err}
	}
}

// explainApproval starts a ctrl+e explain for the pending request: it marks
// the prompt as explaining (so the rationale header shows the in-flight
// marker) and returns the [Supervisor.ExplainPermission] call as a Cmd.
//
// It resolves and dismisses NOTHING — unlike every other key handled beside
// it. An explain is a read: the request stays pending on the agent side and
// the prompt stays on screen, so the human still answers it afterwards. A
// second ctrl+e while one is already in flight is a no-op rather than a
// second call, so holding the key can't stack requests at the daemon.
func (a App) explainApproval() (tea.Model, tea.Cmd) {
	id, _, ok := a.sess.PendingApproval()
	if !ok || a.sess.ApprovalExplaining() {
		return a, nil
	}
	a.sess = a.sess.MarkApprovalExplaining()
	return a, a.doExplain(a.sessID, id)
}

// doExplain fetches the agent's gating rationale via the Supervisor. The
// session id and call id are captured HERE rather than read back in the
// closure, so the result msg is attributable to the request that asked for it
// even if the app has moved on by the time it lands (see
// [App.applyPermissionExplained]'s stale guard).
//
// It carries no deadline of its own: a daemon-backed Supervisor bounds the
// round trip itself (see internal/daemonbridge's ExplainPermission), and an
// in-process one answers from memory without doing any I/O at all.
func (a App) doExplain(sessionID, id string) tea.Cmd {
	return func() tea.Msg {
		rationale, err := a.sup.ExplainPermission(context.Background(), sessionID, id)
		return permissionExplainedMsg{id: id, rationale: rationale, err: err}
	}
}

// permissionExplainedMsg carries a ctrl+e explain's outcome back into the
// Update loop: the call id it was asked for (never dropped — it is what makes
// the result attributable), the agent's rationale, and the error when the
// request failed.
type permissionExplainedMsg struct {
	id        string
	rationale acp.PermissionRationale
	err       error
}

// applyPermissionExplained folds an explain's result into the pending prompt.
//
// The id check is a STALE-RESULT GUARD, not a formality: an explain is a
// round trip, and within it the request can resolve (another client answered
// it, or this one did), be dismissed with esc, or be superseded by a second
// request. Applying a rationale for a request that is no longer the one on
// screen would repaint the current prompt with an explanation of a different
// call — the most misleading thing this feature could do — so a msg that does
// not match the live pending id is dropped entirely.
//
// A failed explain keeps whatever rationale is already showing (the local
// derivation, or an earlier answer) and reports the failure on the status
// line. The prompt stays open and answerable either way: an explain that
// could not be fetched must never cost a user their ability to decide.
func (a App) applyPermissionExplained(msg permissionExplainedMsg) App {
	id, _, ok := a.sess.PendingApproval()
	if !ok || id != msg.id {
		return a
	}
	if msg.err != nil {
		a.sess = a.sess.ClearApprovalExplaining()
		a.setStatus(sevDanger, "explain: "+msg.err.Error())
		return a
	}
	a.sess = a.sess.SetApprovalRationale(msg.rationale)
	return a
}
