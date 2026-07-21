package tui

// dialog.go holds [App]'s key handling for the two prompts that commandeer
// the attach footer: a pending permission request and a pending structured
// decision. Both prompts' state, and both renders, live on [Model] (see
// approval.go and decision.go); this file is just the App-level routing that
// captures input while one is active and turns the user's choice into a
// [Supervisor.Reply] / [Supervisor.AnswerDecision] call.
// It replaces gofer's first interactive TUI dialog — a centered overlay box
// for the "approvals relay + phone approval UX" item — with key handling for
// the inline prompt now rendered in-flow by [Model.View].

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/acp"
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

// handleDecisionKey handles key presses while the attached session has a
// pending structured-decision request (see [Model.HasPendingDecision]),
// capturing all input until it resolves or is dismissed — [App.Update] routes
// here instead of the per-screen handlers whenever that's true and no approval
// is pending (an approval wins if both somehow are; see promptLines for why).
//
// Keymap: ↑/↓ move the focused row, 1-9 answer with that option directly,
// Enter resolves the focused row (an option answers with it; "Type
// something." enters typing mode and a SECOND Enter submits what was typed;
// "Chat about this" answers with the chat escape hatch), Esc leaves typing
// mode or — when not typing — CANCELS the request (see [App.cancelDecision];
// the hint line has always promised "Esc to cancel"), and ctrl+c quits, the
// last of those exactly as [App.handleApprovalKey] does.
//
// While typing, every key this switch does not itself claim falls through to
// the shared input keymap (input_keymap.go), so the free-text answer gets the
// same movement/insertion/deletion editing the attach input has — and the
// digit keys type digits instead of selecting options, which is why the
// numeric and arrow cases are gated on !typing. While NOT typing, an unclaimed
// key is a no-op: the prompt owns the whole footer, so there is nothing else
// on screen for it to mean.
//
// j/k are deliberately unbound. Every list in this TUI (roster, command menu,
// /config, /model) is arrow-only, so vi keys here alone would make the
// vocabulary inconsistent — and they would fight the free-text row the moment
// it is active.
func (a App) handleDecisionKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	p := a.sess.pendingDec
	if p == nil {
		return a, nil
	}
	key := msg.Key()
	switch {
	case key.Mod.Contains(tea.ModCtrl) && key.Code == 'c':
		return a, tea.Quit

	case key.Code == tea.KeyEscape:
		if p.typing {
			a.sess = a.sess.stopDecisionTyping()
			return a, nil
		}
		return a.cancelDecision()

	case key.Code == tea.KeyEnter:
		return a.resolveDecision()

	case !p.typing && key.Code == tea.KeyUp:
		a.sess = a.sess.moveDecisionCursor(-1)
		return a, nil

	case !p.typing && key.Code == tea.KeyDown:
		a.sess = a.sess.moveDecisionCursor(1)
		return a, nil

	case !p.typing && len(key.Text) == 1 && key.Text[0] >= '1' && key.Text[0] <= '9':
		return a.selectDecisionOption(int(key.Text[0] - '1'))
	}

	if p.typing {
		if buf, ok := applyInputKey(p.input, key); ok {
			a.sess = a.sess.withDecisionInput(buf)
		}
	}
	return a, nil
}

// resolveDecision acts on the focused row: an option answers with it, the
// free-text row opens its editor (or, already open, submits what was typed),
// and the chat row hands the turn back to the conversation. A submit with
// nothing but whitespace typed is a no-op rather than an empty text answer —
// the agent asked a question, and "" is not an answer to it; the user can keep
// typing or press Esc.
func (a App) resolveDecision() (tea.Model, tea.Cmd) {
	p := a.sess.pendingDec
	if p == nil {
		return a, nil
	}
	row, ok := p.focused()
	if !ok {
		return a, nil
	}
	switch row.kind {
	case decisionRowOption:
		return a.answerDecision(acp.DecisionOutcomeSelected{OptionID: p.question.Options[row.opt].OptionID})

	case decisionRowFreeText:
		if !p.typing {
			a.sess = a.sess.startDecisionTyping()
			return a, nil
		}
		text := strings.TrimSpace(p.input.String())
		if text == "" {
			return a, nil
		}
		return a.answerDecision(acp.DecisionOutcomeText{Text: text})

	case decisionRowChat:
		return a.answerDecision(acp.DecisionOutcomeChat{})
	}
	return a, nil
}

// selectDecisionOption answers the pending decision with the question's i-th
// option (0-based; the 1-9 digit keys map onto it). A digit past the end of
// the option list is a no-op — a number key must never answer something other
// than the row it names.
func (a App) selectDecisionOption(i int) (tea.Model, tea.Cmd) {
	p := a.sess.pendingDec
	if p == nil || i < 0 || i >= len(p.question.Options) {
		return a, nil
	}
	return a.answerDecision(acp.DecisionOutcomeSelected{OptionID: p.question.Options[i].OptionID})
}

// answerDecision resolves the pending decision by answering its rendered
// question with outcome.
//
// PR 1 answers the request's FIRST question only. That is not a lost answer:
// decision.Gate.Answer fills every question this call omits in as cancelled,
// so the tool downstream still receives exactly one answer per question.
func (a App) answerDecision(outcome acp.DecisionOutcome) (tea.Model, tea.Cmd) {
	p := a.sess.pendingDec
	if p == nil {
		return a, nil
	}
	return a.sendDecision([]acp.DecisionAnswer{{QuestionID: p.question.QuestionID, Outcome: outcome}})
}

// cancelDecision resolves the pending decision by answering EVERY question in
// the request — including the ones this PR does not render — with
// [acp.DecisionOutcomeCancelled]. That is what esc means here, and what the
// prompt's hint line has always said it means.
//
// Cancelling rather than dismissing locally is the whole point: a decision has
// no transcript badge and no event-stream replay to find it by again, so a
// prompt cleared without resolving would leave the agent's turn blocked forever
// with nothing on screen. Cancelled is a first-class outcome all the way down —
// the gate normalizes unanswered questions to it and the tool reports it to the
// model without IsError — so the model gets told "the user declined to choose"
// and can carry on in prose.
func (a App) cancelDecision() (tea.Model, tea.Cmd) {
	p := a.sess.pendingDec
	if p == nil {
		return a, nil
	}
	answers := make([]acp.DecisionAnswer, 0, len(p.questionIDs))
	for _, id := range p.questionIDs {
		answers = append(answers, acp.DecisionAnswer{QuestionID: id, Outcome: acp.DecisionOutcomeCancelled{}})
	}
	return a.sendDecision(answers)
}

// sendDecision sends answers via [Supervisor.AnswerDecision] and clears the
// prompt immediately — the same optimistic local dismiss [App.resolveApproval]
// makes. The gate's own UpdateResolved, when it lands on the decision
// subscription a moment later, is then a no-op in [Model.IngestDecision]: the
// id it carries no longer matches anything pending.
func (a App) sendDecision(answers []acp.DecisionAnswer) (tea.Model, tea.Cmd) {
	p := a.sess.pendingDec
	if p == nil {
		return a, nil
	}
	// Route on the session the REQUEST names, falling back to the attached
	// session for a gate that was never bound one (see decision.Gate.Bind).
	// The two agree in every wired path — a daemon-backed supervisor keys its
	// gates by exactly this id — so this is about which value is authoritative,
	// not about reconciling a disagreement.
	sessionID := p.session
	if sessionID == "" {
		sessionID = a.sessID
	}
	id := p.id
	a.sess = a.sess.DismissDecision()
	return a, a.doAnswerDecision(sessionID, id, answers)
}

// doAnswerDecision resolves a pending decision request via the Supervisor.
func (a App) doAnswerDecision(sessionID, requestID string, answers []acp.DecisionAnswer) tea.Cmd {
	return func() tea.Msg {
		err := a.sup.AnswerDecision(context.Background(), sessionID, requestID, answers)
		return opDoneMsg{err: err}
	}
}
