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
)

// handleApprovalKey handles key presses while the peeked/attached session
// has a pending approval (see [Model.HasPendingApproval]), capturing all
// input until it resolves or is dismissed — [App.Update] routes here instead
// of the per-screen handlers whenever that's true. Keymap: a/y allow, d/n
// deny (both reply immediately and dismiss the prompt), r toggles remember,
// esc dismisses the prompt without replying — the underlying request stays
// pending server-side (e.g. a matching [event.PermissionResolved] from
// another attached client, or a later re-attach to the same session, can
// still surface or settle it).
//
// 1/2 are aliases for allow/deny because the prompt itself renders the
// choices numbered ("1. [a] Yes   2. [d] No" — see renderApprovalPrompt): a
// rendered affordance that did nothing when pressed would be a lie, and
// numbered answers are the reflex a confirm prompt trains.
func (a App) handleApprovalKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.Key()
	switch {
	case key.Mod.Contains(tea.ModCtrl) && key.Code == 'c':
		return a, tea.Quit

	case key.Text == "a" || key.Text == "y" || key.Text == "1":
		return a.resolveApproval(true)

	case key.Text == "d" || key.Text == "n" || key.Text == "2":
		return a.resolveApproval(false)

	case key.Text == "r":
		a.sess = a.sess.ToggleApprovalRemember()
		return a, nil

	case key.Code == tea.KeyEscape:
		a.sess = a.sess.DismissApproval()
		return a, nil
	}
	return a, nil
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
	return a, a.doReply(a.sessID, id, allow, remember)
}

// doReply answers a pending permission request via the Supervisor.
func (a App) doReply(sessionID, id string, allow, remember bool) tea.Cmd {
	return func() tea.Msg {
		err := a.sup.Reply(context.Background(), sessionID, id, allow, remember)
		return opDoneMsg{err: err}
	}
}
