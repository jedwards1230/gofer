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

// handleDecisionKey handles key presses while the attached session has a
// pending structured-decision request (see [Model.HasPendingDecision]),
// capturing all input until it resolves or is dismissed — [App.Update] routes
// here instead of the per-screen handlers whenever that's true and no approval
// is pending (an approval wins if both somehow are; see promptLines for why).
//
// Keymap: ↑/↓ move the focused row, 1-9 answer with that option directly,
// Enter resolves the focused row (an option answers with it; "Type
// something." enters typing mode and a SECOND Enter submits what was typed;
// "Chat about this" answers with the chat escape hatch), Esc leaves an editor
// or — when in neither — CANCELS the whole request (see [App.cancelDecision];
// the hint line has always promised "Esc to cancel"), and ctrl+c quits, the
// last of those exactly as [App.handleApprovalKey] does.
//
// A MULTI-question request binds three more: Tab/shift+tab (and ←/→, which is
// what the tab strip's end arrows advertise — a rendered affordance that did
// nothing when pressed would be a lie) move between questions and the Submit
// tab, and `n` opens the notes editor on the focused question's answer. All
// three are gated on [pendingDecision.multi] because there is nothing for them
// to do on a single question: one tab to switch to, and a notes affordance the
// single-question hint line does not advertise.
//
// While either editor is open, every key this switch does not itself claim
// falls through to the shared input keymap (input_keymap.go), so the free-text
// answer and the note get the same movement/insertion/deletion editing the
// attach input has — and the digit keys type digits instead of selecting
// options, which is why the numeric, arrow, tab and `n` cases are gated on
// !typing && !noting. With neither open, an unclaimed key is a no-op: the
// prompt owns the whole footer, so there is nothing else on screen for it to
// mean.
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
	editing := p.typing || p.noting
	tabs := p.multi() && !editing
	switch {
	case key.Mod.Contains(tea.ModCtrl) && key.Code == 'c':
		return a, tea.Quit

	// The notes editor claims Enter and Esc ahead of the row keymap below, so
	// saving a note can never be read as answering the question it annotates.
	case p.noting && key.Code == tea.KeyEnter:
		a.sess = a.sess.commitDecisionNote()
		return a, nil

	case key.Code == tea.KeyEscape:
		if p.noting {
			a.sess = a.sess.stopDecisionNoting()
			return a, nil
		}
		if p.typing {
			a.sess = a.sess.stopDecisionTyping()
			return a, nil
		}
		return a.cancelDecision()

	case key.Code == tea.KeyEnter:
		return a.resolveDecision()

	case tabs && key.Code == tea.KeyTab:
		// shift+tab steps backwards through the same strip; the strip draws an
		// arrow at each end, so both directions have to exist.
		delta := 1
		if key.Mod.Contains(tea.ModShift) {
			delta = -1
		}
		a.sess = a.sess.moveDecisionTab(delta)
		return a, nil

	case tabs && key.Code == tea.KeyLeft:
		a.sess = a.sess.moveDecisionTab(-1)
		return a, nil

	case tabs && key.Code == tea.KeyRight:
		a.sess = a.sess.moveDecisionTab(1)
		return a, nil

	// Not on the Submit tab: a note belongs to a question's answer, and the
	// Submit tab has no question to annotate.
	case tabs && !p.onSubmitTab() && key.Text == "n":
		a.sess = a.sess.startDecisionNoting()
		return a, nil

	case !editing && key.Code == tea.KeyUp:
		a.sess = a.sess.moveDecisionCursor(-1)
		return a, nil

	case !editing && key.Code == tea.KeyDown:
		a.sess = a.sess.moveDecisionCursor(1)
		return a, nil

	case !editing && len(key.Text) == 1 && key.Text[0] >= '1' && key.Text[0] <= '9':
		return a.selectDecisionOption(int(key.Text[0] - '1'))
	}

	switch {
	case p.noting:
		if buf, ok := applyInputKey(p.notes, key); ok {
			a.sess = a.sess.withDecisionNotes(buf)
		}
	case p.typing:
		if buf, ok := applyInputKey(p.input, key); ok {
			a.sess = a.sess.withDecisionInput(buf)
		}
	}
	return a, nil
}

// resolveDecision acts on the focused row: an option answers with it, the
// free-text row opens its editor (or, already open, records what was typed),
// the chat row hands the turn back to the conversation, and the Submit row —
// which only a multi-question request has — commits every drafted answer at
// once. A submit with nothing but whitespace typed is a no-op rather than an
// empty text answer — the agent asked a question, and "" is not an answer to
// it; the user can keep typing or press Esc.
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
		return a.answerDecision(acp.DecisionOutcomeSelected{OptionID: p.question().Options[row.opt].OptionID})

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

	case decisionRowSubmit:
		return a.submitDecision()
	}
	return a, nil
}

// selectDecisionOption answers the focused question with its i-th option
// (0-based; the 1-9 digit keys map onto it). A digit past the end of the option
// list is a no-op — a number key must never answer something other than the row
// it names — as is any digit on the Submit tab, whose zero question offers no
// options at all.
func (a App) selectDecisionOption(i int) (tea.Model, tea.Cmd) {
	p := a.sess.pendingDec
	if p == nil || i < 0 || i >= len(p.question().Options) {
		return a, nil
	}
	return a.answerDecision(acp.DecisionOutcomeSelected{OptionID: p.question().Options[i].OptionID})
}

// answerDecision records outcome as the FOCUSED question's answer, and — on a
// single-question request — submits it in the same breath.
//
// That split is the whole multi-question contract in one function. With one
// question there is nothing to batch, so the answer goes out immediately and
// the prompt clears, exactly as it shipped. With several, the answer becomes a
// draft the tab strip marks ✔, and nothing is sent until the Submit row
// (see [App.submitDecision]) commits them together — an agent that needed four
// sign-offs gets one round trip, not four.
func (a App) answerDecision(outcome acp.DecisionOutcome) (tea.Model, tea.Cmd) {
	p := a.sess.pendingDec
	if p == nil || p.onSubmitTab() {
		return a, nil
	}
	a.sess = a.sess.recordDecisionAnswer(outcome)
	if p.multi() {
		return a, nil
	}
	return a.submitDecision()
}

// submitDecision resolves the pending decision with every drafted answer at
// once — the Submit row, and the tail of the single-question path.
//
// Questions still unanswered are simply OMITTED (see
// [pendingDecision.submitAnswers]): decision.Gate.Answer fills each of them in
// as cancelled, which is tested behavior in internal/decision and must not be
// re-implemented here. So submitting two of four commits those two and cancels
// the other two — while esc (below) cancels all four.
func (a App) submitDecision() (tea.Model, tea.Cmd) {
	p := a.sess.pendingDec
	if p == nil {
		return a, nil
	}
	return a.sendDecision(p.submitAnswers())
}

// cancelDecision resolves the pending decision by answering EVERY question in
// the request — drafted or not, rendered or not — with
// [acp.DecisionOutcomeCancelled]. That is what esc means here, and what the
// prompt's hint line has always said it means. It deliberately discards the
// drafts and their notes: esc is "I am not answering this", and quietly
// committing half of it because the user had already ticked a box would be a
// different, more surprising thing than the key advertises.
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
	ids := p.questionIDs()
	answers := make([]acp.DecisionAnswer, 0, len(ids))
	for _, id := range ids {
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
