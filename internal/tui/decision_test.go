package tui

// decision_test.go is the inline decision prompt's Update-level layer: one
// test per key the prompt binds, driven through App's real Update dispatch so
// the precedence checks, the pump, and the Supervisor call are all exercised
// rather than stubbed. It lives in package tui (not tui_test) for the same
// reason app_internal_test.go does — it constructs the app root's unexported
// decision messages directly, which is the only way to seed the pump without
// spinning a bubbletea runtime.
//
// The fake Supervisor's gates are REAL decision.Gates (see
// internal/internalFakeSup), so openDecision below opens a genuinely blocked
// ask_user request: the request the widget renders is one an agent's turn is
// actually parked on, and answering it actually releases that turn.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/acp"

	"github.com/jedwards1230/gofer/internal/decision"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// decisionQuestions is the fixture every test below asks: two options (the
// second recommended) plus both escape hatches, with ids stamped the way
// decision.Gate.Request stamps them.
func decisionQuestions() []acp.DecisionQuestion {
	return []acp.DecisionQuestion{{
		Title:    "Pick a migration strategy",
		Question: "Which approach should I take?",
		Options: []acp.DecisionOption{
			{Label: "In-place ALTER", Rationale: "fastest, but locks the table for the duration"},
			{Label: "Shadow table + backfill", Rationale: "online, but doubles disk until cutover", Recommended: true},
		},
		AllowFreeText: decision.DefaultAllowFreeText,
		AllowChat:     decision.DefaultAllowChat,
	}}
}

// blockedRequest is one in-flight ask_user call: the goroutine parked inside
// decision.Gate.Request, plus the channel its answers land on.
type blockedRequest struct {
	answers <-chan []acp.DecisionAnswer
	errs    <-chan error
}

// attachForDecisionTest attaches into the roster's selected session (like
// attachForDialogTest) and hands the App a live subscription to that session's
// decision gate — the decisionSubReadyMsg App.Update would otherwise receive
// from the subscribeDecisions Cmd armed off subReadyMsg.
func attachForDecisionTest(t *testing.T, sup *internalFakeSup) App {
	t.Helper()
	a := attachForDialogTest(t, sup)
	sub, err := sup.Decisions(context.Background(), a.sessID)
	if err != nil {
		t.Fatalf("Decisions(%s): %v", a.sessID, err)
	}
	t.Cleanup(sub.Close)

	mdl, _ := a.Update(decisionSubReadyMsg{id: a.sessID, sub: sub})
	return mdl.(App)
}

// openDecision starts a genuinely blocked ask_user call against a's attached
// session's gate and pumps the resulting UpdateRequested into the App through
// the same waitForDecision Cmd the live pump uses. It returns the updated App
// (now rendering the prompt) and a handle on the blocked call, so a test can
// assert on the answers the agent's turn actually received.
func openDecision(t *testing.T, sup *internalFakeSup, a App) (App, blockedRequest) {
	t.Helper()
	return openDecisionWith(t, sup, a, decisionQuestions())
}

// openDecisionWith is openDecision over an arbitrary question set — what the
// multi-question tests (decision_multi_test.go) open their batches through.
func openDecisionWith(t *testing.T, sup *internalFakeSup, a App, questions []acp.DecisionQuestion) (App, blockedRequest) {
	t.Helper()
	gate := sup.gate(a.sessID)

	answers := make(chan []acp.DecisionAnswer, 1)
	errs := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		got, err := gate.Request(ctx, questions)
		if err != nil {
			errs <- err
			return
		}
		answers <- got
	}()

	// waitForDecision blocks on the subscription's channel, which is exactly
	// what we want here: it returns as soon as the goroutine above publishes
	// its UpdateRequested, with no polling and no sleep.
	mdl, _ := a.Update(waitForDecision(a.sessID, a.decSub)())
	a = mdl.(App)
	if !a.sess.HasPendingDecision() {
		t.Fatal("expected a pending decision after the gate opened a request")
	}
	return a, blockedRequest{answers: answers, errs: errs}
}

// await returns the answers the blocked ask_user call received, failing the
// test if the call is still parked — an answer that never reaches the gate is
// the failure mode every test here is really guarding against.
func (b blockedRequest) await(t *testing.T) []acp.DecisionAnswer {
	t.Helper()
	select {
	case got := <-b.answers:
		return got
	case err := <-b.errs:
		t.Fatalf("the blocked ask_user call failed instead of being answered: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("the blocked ask_user call was never answered — the TUI's answer did not reach the gate")
	}
	return nil
}

// stillBlocked asserts the ask_user call has NOT been answered — the property
// esc-dismisses-without-answering turns on.
func (b blockedRequest) stillBlocked(t *testing.T) {
	t.Helper()
	select {
	case got := <-b.answers:
		t.Fatalf("the ask_user call was answered %+v; want it still blocked", got)
	case err := <-b.errs:
		t.Fatalf("the ask_user call failed with %v; want it still blocked", err)
	case <-time.After(50 * time.Millisecond):
	}
}

// selectedOption asserts answers is exactly one DecisionOutcomeSelected naming
// wantOption on wantQuestion.
func selectedOption(t *testing.T, answers []acp.DecisionAnswer, wantQuestion, wantOption string) {
	t.Helper()
	if len(answers) != 1 {
		t.Fatalf("answers = %+v; want exactly one (one per question)", answers)
	}
	if answers[0].QuestionID != wantQuestion {
		t.Errorf("answers[0].QuestionID = %q; want %q", answers[0].QuestionID, wantQuestion)
	}
	sel, ok := answers[0].Outcome.(acp.DecisionOutcomeSelected)
	if !ok {
		t.Fatalf("answers[0].Outcome = %#v; want a DecisionOutcomeSelected", answers[0].Outcome)
	}
	if sel.OptionID != wantOption {
		t.Errorf("selected option = %q; want %q", sel.OptionID, wantOption)
	}
}

// press sends one key through App.Update and runs whatever Cmd came back,
// feeding its message back in — the Supervisor calls this prompt makes are
// Cmds, so a test that never runs them asserts on nothing.
func pressDecision(t *testing.T, a App, msg tea.KeyPressMsg) App {
	t.Helper()
	mdl, cmd := a.Update(msg)
	a = mdl.(App)
	if cmd != nil {
		if out := cmd(); out != nil {
			mdl, _ = a.Update(out)
			a = mdl.(App)
		}
	}
	return a
}

// TestDecisionPromptOpensOnRequest is the pump's own test: an UpdateRequested
// read off a real gate's subscription reaches Model as a pending decision and
// renders the prompt, and the read is re-armed rather than consumed once.
func TestDecisionPromptOpensOnRequest(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDecisionTest(t, sup)
	a, req := openDecision(t, sup, a)
	defer req.stillBlocked(t)

	id, ok := a.sess.PendingDecision()
	if !ok || id != "dec-1" {
		t.Fatalf("PendingDecision() = %q, %v; want the gate's own dec-1", id, ok)
	}
	got := a.render()
	for _, want := range []string{
		"decision · Pick a migration strategy",
		"Which approach should I take?",
		"1  In-place ALTER",
		"2  Shadow table + backfill  (Recommended)",
		"› Type something.",
		"↳ Chat about this",
		"Enter to select · ↑/↓ to navigate · Esc to cancel",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("attach render missing decision prompt content %q:\n%s", want, got)
		}
	}
}

// TestDecisionNavigateMovesCursorAndClamps covers ↓/↑: the cursor walks the
// row list (options, then the two escape hatches) and CLAMPS at both ends
// rather than wrapping — wrapping from the last row back onto option 1 is
// exactly how a stray key press sends the wrong answer.
func TestDecisionNavigateMovesCursorAndClamps(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDecisionTest(t, sup)
	a, req := openDecision(t, sup, a)
	defer req.stillBlocked(t)

	if got := a.sess.pendingDec.cursor; got != 0 {
		t.Fatalf("initial cursor = %d; want 0 (the first option is focused)", got)
	}

	// 2 options + free text + chat = 4 rows; walk past the end.
	for range 6 {
		mdl, _ := a.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		a = mdl.(App)
	}
	if got := a.sess.pendingDec.cursor; got != 3 {
		t.Errorf("cursor after 6 × ↓ over a 4-row prompt = %d; want it clamped to 3, not wrapped", got)
	}
	if !strings.Contains(a.render(), "▸ ↳ Chat about this") {
		t.Errorf("expected the chat row to render focused:\n%s", a.render())
	}

	for range 9 {
		mdl, _ := a.Update(tea.KeyPressMsg{Code: tea.KeyUp})
		a = mdl.(App)
	}
	if got := a.sess.pendingDec.cursor; got != 0 {
		t.Errorf("cursor after walking back up = %d; want it clamped to 0", got)
	}
}

// TestDecisionEnterSelectsFocusedOption covers Enter on a focused option: it
// answers with THAT option's id, dismisses the prompt immediately, and the
// answer reaches the blocked ask_user call.
func TestDecisionEnterSelectsFocusedOption(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDecisionTest(t, sup)
	a, req := openDecision(t, sup, a)

	mdl, _ := a.Update(tea.KeyPressMsg{Code: tea.KeyDown}) // focus option 2
	a = mdl.(App)
	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyEnter})

	if a.sess.HasPendingDecision() {
		t.Fatal("expected the prompt cleared immediately on Enter (optimistic local dismiss)")
	}
	selectedOption(t, req.await(t), "q1", "q1o2")

	if len(sup.answers) != 1 {
		t.Fatalf("sup.answers = %+v; want exactly one AnswerDecision call", sup.answers)
	}
	if sup.answers[0].sessionID != a.sessID || sup.answers[0].requestID != "dec-1" {
		t.Errorf("AnswerDecision routed to %q/%q; want %q/dec-1",
			sup.answers[0].sessionID, sup.answers[0].requestID, a.sessID)
	}
}

// TestDecisionDigitSelectsOptionDirectly covers the 1-9 shortcut: "2" answers
// with the second option without moving the cursor there first, and a digit
// past the end of the list is a no-op rather than an answer to something else.
func TestDecisionDigitSelectsOptionDirectly(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDecisionTest(t, sup)
	a, req := openDecision(t, sup, a)

	// "9" names no option on a two-option question: nothing may be answered.
	mdl, cmd := a.Update(tea.KeyPressMsg{Text: "9"})
	a = mdl.(App)
	if cmd != nil {
		t.Error("expected a digit past the end of the option list to issue no Cmd")
	}
	if !a.sess.HasPendingDecision() {
		t.Fatal("expected the prompt to stay open after an out-of-range digit")
	}

	a = pressDecision(t, a, tea.KeyPressMsg{Text: "2"})
	if a.sess.HasPendingDecision() {
		t.Fatal("expected the prompt cleared after selecting option 2 by digit")
	}
	selectedOption(t, req.await(t), "q1", "q1o2")
}

// TestDecisionFreeTextSubmits covers the free-text row end to end: the first
// Enter opens the editor (and the hint changes to say so), typed text lands in
// the buffer and renders in place of the placeholder, an empty submit is a
// no-op, and the second Enter sends a DecisionOutcomeText.
func TestDecisionFreeTextSubmits(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDecisionTest(t, sup)
	a, req := openDecision(t, sup, a)

	for range 2 { // walk down to "› Type something."
		mdl, _ := a.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		a = mdl.(App)
	}
	mdl, cmd := a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	a = mdl.(App)
	if cmd != nil {
		t.Error("expected the first Enter on the free-text row to open the editor, not answer")
	}
	if !a.sess.pendingDec.typing {
		t.Fatal("expected typing mode after Enter on the free-text row")
	}
	if got := a.render(); !strings.Contains(got, "Enter to submit · Esc to cancel") {
		t.Errorf("expected the typing-mode key hint:\n%s", got)
	}

	// An empty buffer must not submit — the agent asked a question, and "" is
	// not an answer to it.
	mdl, cmd = a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	a = mdl.(App)
	if cmd != nil || !a.sess.HasPendingDecision() {
		t.Fatal("expected an empty free-text submit to be a no-op")
	}

	for _, r := range "neither, shard it" {
		mdl, _ = a.Update(tea.KeyPressMsg{Text: string(r)})
		a = mdl.(App)
	}
	if got := a.render(); !strings.Contains(got, "› neither, shard it▏") {
		t.Errorf("expected the typed answer with its cursor on the free-text row:\n%s", got)
	}

	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyEnter})
	if a.sess.HasPendingDecision() {
		t.Fatal("expected the prompt cleared after submitting free text")
	}

	answers := req.await(t)
	if len(answers) != 1 {
		t.Fatalf("answers = %+v; want exactly one", answers)
	}
	text, ok := answers[0].Outcome.(acp.DecisionOutcomeText)
	if !ok {
		t.Fatalf("answers[0].Outcome = %#v; want a DecisionOutcomeText", answers[0].Outcome)
	}
	if text.Text != "neither, shard it" {
		t.Errorf("free-text answer = %q; want %q", text.Text, "neither, shard it")
	}
}

// TestDecisionEscLeavesTypingThenCancels covers Esc's two-step contract: the
// first press leaves typing mode (discarding the half-typed answer) and only
// the second cancels the request — so escape never throws away more than one
// thing at a time.
func TestDecisionEscLeavesTypingThenCancels(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDecisionTest(t, sup)
	a, req := openDecision(t, sup, a)

	for range 2 {
		mdl, _ := a.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		a = mdl.(App)
	}
	mdl, _ := a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	a = mdl.(App)
	for _, r := range "half typed" {
		mdl, _ = a.Update(tea.KeyPressMsg{Text: string(r)})
		a = mdl.(App)
	}

	mdl, cmd := a.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	a = mdl.(App)
	if cmd != nil {
		t.Error("expected the first Esc to issue no Cmd — nothing is resolved yet")
	}
	if !a.sess.HasPendingDecision() {
		t.Fatal("expected the first Esc to leave typing mode, not resolve the prompt")
	}
	if a.sess.pendingDec.typing || a.sess.pendingDec.input.String() != "" {
		t.Errorf("expected typing mode off and the buffer cleared, got typing=%v buf=%q",
			a.sess.pendingDec.typing, a.sess.pendingDec.input.String())
	}
	req.stillBlocked(t)

	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyEscape})
	if a.sess.HasPendingDecision() {
		t.Fatal("expected the second Esc to clear the prompt")
	}
	cancelledAnswers(t, req.await(t), "q1")
}

// TestDecisionChatAnswersWithChat covers the "↳ Chat about this" escape hatch:
// Enter on it answers with a DecisionOutcomeChat, which is a legitimate
// outcome (the user wants to talk it through), not a cancellation.
func TestDecisionChatAnswersWithChat(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDecisionTest(t, sup)
	a, req := openDecision(t, sup, a)

	for range 3 { // options 1, 2, free text, then chat
		mdl, _ := a.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		a = mdl.(App)
	}
	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyEnter})

	if a.sess.HasPendingDecision() {
		t.Fatal("expected the prompt cleared after choosing the chat escape hatch")
	}
	answers := req.await(t)
	if len(answers) != 1 {
		t.Fatalf("answers = %+v; want exactly one", answers)
	}
	if _, ok := answers[0].Outcome.(acp.DecisionOutcomeChat); !ok {
		t.Errorf("answers[0].Outcome = %#v; want a DecisionOutcomeChat", answers[0].Outcome)
	}
}

// TestDecisionEscCancelsTheRequest is the esc contract's load-bearing half,
// asserted against the GATE rather than just the widget: esc ANSWERS the
// request cancelled — every question in it, not just the rendered one — the
// blocked ask_user call is released with those answers, and nothing is left
// open on the gate.
//
// The alternative (clear the prompt, leave the request open) is what this
// replaced: a decision has no transcript badge and no event-stream replay, so
// an orphaned request blocks the agent's turn forever with nothing on screen
// pointing at it.
func TestDecisionEscCancelsTheRequest(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDecisionTest(t, sup)
	a, req := openDecision(t, sup, a)

	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyEscape})
	if a.sess.HasPendingDecision() {
		t.Fatal("expected the prompt cleared after esc")
	}

	if len(sup.answers) != 1 {
		t.Fatalf("sup.answers = %+v; want exactly one AnswerDecision call — esc must resolve, not orphan", sup.answers)
	}
	if sup.answers[0].sessionID != a.sessID || sup.answers[0].requestID != "dec-1" {
		t.Errorf("AnswerDecision routed to %q/%q; want %q/dec-1",
			sup.answers[0].sessionID, sup.answers[0].requestID, a.sessID)
	}
	cancelledAnswers(t, req.await(t), "q1")

	// Nothing is left open: the agent's turn is unblocked, not parked on a
	// request no client is rendering any more.
	if open := sup.gate(a.sessID).Open(); len(open) != 0 {
		t.Errorf("gate.Open() = %+v; want nothing open after esc cancelled the request", open)
	}
}

// TestDecisionEscCancelsEveryQuestion pins the multi-question half of esc: the
// agent is blocked on every question in the batch, so the cancel names every
// question id in the request explicitly rather than leaning on the gate's
// cancelled fill-in. (decision_multi_test.go asserts the same contract against
// a batch the user has already drafted answers into — esc discards those too.)
func TestDecisionEscCancelsEveryQuestion(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDecisionTest(t, sup)

	gate := sup.gate(a.sessID)
	questions := append(decisionQuestions(), acp.DecisionQuestion{
		Title: "Retention", Question: "How long do we keep the shadow table?",
		AllowFreeText: true, AllowChat: true,
	})
	answers := make(chan []acp.DecisionAnswer, 1)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		got, err := gate.Request(ctx, questions)
		if err != nil {
			t.Errorf("gate.Request: %v", err)
			return
		}
		answers <- got
	}()
	mdl, _ := a.Update(waitForDecision(a.sessID, a.decSub)())
	a = mdl.(App)

	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyEscape})

	if len(sup.answers) != 1 {
		t.Fatalf("sup.answers = %+v; want exactly one AnswerDecision call from esc", sup.answers)
	}
	sent := sup.answers[0].answers
	if len(sent) != 2 {
		t.Fatalf("esc sent %d answers for a two-question request: %+v", len(sent), sent)
	}
	select {
	case got := <-answers:
		cancelledAnswers(t, got, "q1", "q2")
	case <-time.After(2 * time.Second):
		t.Fatal("the blocked ask_user call was never released by esc")
	}
}

// cancelledAnswers asserts answers is exactly one DecisionOutcomeCancelled per
// wantQuestion, in order.
func cancelledAnswers(t *testing.T, answers []acp.DecisionAnswer, wantQuestions ...string) {
	t.Helper()
	if len(answers) != len(wantQuestions) {
		t.Fatalf("answers = %+v; want %d (one per question)", answers, len(wantQuestions))
	}
	for i, want := range wantQuestions {
		if answers[i].QuestionID != want {
			t.Errorf("answers[%d].QuestionID = %q; want %q", i, answers[i].QuestionID, want)
		}
		if _, ok := answers[i].Outcome.(acp.DecisionOutcomeCancelled); !ok {
			t.Errorf("answers[%d].Outcome = %#v; want a DecisionOutcomeCancelled", i, answers[i].Outcome)
		}
	}
}

// TestDecisionResolvedByAnotherPeerClears covers the UpdateResolved half of
// the pump: another attached client (or an interrupted turn) resolves the
// request, and this client's prompt clears without it ever answering. A
// resolution for a DIFFERENT id must leave the prompt alone.
func TestDecisionResolvedByAnotherPeerClears(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDecisionTest(t, sup)
	a, req := openDecision(t, sup, a)

	mdl, _ := a.Update(decisionMsg{
		id:  a.sessID,
		up:  decision.Update{Kind: decision.UpdateResolved, Request: decision.Request{ID: "dec-99"}},
		sub: a.decSub,
	})
	a = mdl.(App)
	if !a.sess.HasPendingDecision() {
		t.Fatal("a resolution for an unrelated request id cleared the prompt")
	}

	// The real thing: another peer answers dec-1 on the gate, and the
	// UpdateResolved it publishes travels the same pump this client reads.
	if err := sup.gate(a.sessID).Answer("dec-1", []acp.DecisionAnswer{
		{QuestionID: "q1", Outcome: acp.DecisionOutcomeSelected{OptionID: "q1o1"}},
	}); err != nil {
		t.Fatalf("peer Answer: %v", err)
	}
	mdl, _ = a.Update(waitForDecision(a.sessID, a.decSub)())
	a = mdl.(App)

	if a.sess.HasPendingDecision() {
		t.Fatal("expected the prompt cleared by the peer's UpdateResolved")
	}
	if len(sup.answers) != 0 {
		t.Errorf("sup.answers = %+v; want none — this client never answered", sup.answers)
	}
	selectedOption(t, req.await(t), "q1", "q1o1") // the peer's answer, not ours
}

// TestDecisionStaleUpdateIgnored covers the pump's staleness guard: an update
// tagged for a session this App has since navigated away from is dropped, not
// ingested — the decision-stream twin of TestAppStaleEventGuard.
func TestDecisionStaleUpdateIgnored(t *testing.T) {
	th := theme.Test()
	a := App{theme: th, sess: New(th), sessID: "session-b", scr: screenAttach}

	mdl, _ := a.Update(decisionMsg{
		id: "session-a",
		up: decision.Update{Kind: decision.UpdateRequested, Request: decision.Request{
			ID: "dec-1", SessionID: "session-a", Questions: decision.AssignIDs(decisionQuestions()),
		}},
	})
	if mdl.(App).sess.HasPendingDecision() {
		t.Fatal("a decision update from session-a opened a prompt while attached to session-b")
	}
}

// TestDecisionSubReadyStaleClosesSubscription covers the other staleness
// guard: a subscription that resolves after the user has moved on is CLOSED,
// not dropped. A gate treats "has a subscriber" as "a client can see this"
// (decision.Gate.Request returns ErrNoClient otherwise), so a leaked
// subscription would let an agent block on a question nobody renders.
func TestDecisionSubReadyStaleClosesSubscription(t *testing.T) {
	th := theme.Test()
	a := App{theme: th, sess: New(th), sessID: "session-b"}

	gate := decision.NewGate("session-a")
	sub := gate.Subscribe(1)
	mdl, _ := a.Update(decisionSubReadyMsg{id: "session-a", sub: sub})
	a = mdl.(App)

	if a.decSub != nil {
		t.Error("a stale decision subscription was adopted; want it discarded")
	}
	if _, ok := <-sub.C; ok {
		t.Error("expected the stale subscription's channel closed")
	}
	if _, err := gate.Request(context.Background(), decisionQuestions()); !errors.Is(err, decision.ErrNoClient) {
		t.Errorf("gate.Request after the stale subscribe = %v; want ErrNoClient — the subscription leaked", err)
	}
}

// TestDecisionSubReadySupersedesLiveSubscription is the adopt path's twin
// guard: a second subscription for the SAME session (Init subscribes, then an
// enter before subReadyMsg lands subscribes again) must close the one already
// held. An overwritten subscription stays in the gate's subscriber set forever,
// and since Gate.Request decides ErrNoClient from "are there subscribers", one
// orphan makes that fail-fast permanently unreachable — an agent would block on
// a question no client is rendering.
func TestDecisionSubReadySupersedesLiveSubscription(t *testing.T) {
	th := theme.Test()
	a := App{theme: th, sess: New(th), sessID: "session-a"}
	gate := decision.NewGate("session-a")

	first := gate.Subscribe(1)
	mdl, _ := a.Update(decisionSubReadyMsg{id: "session-a", sub: first})
	a = mdl.(App)
	second := gate.Subscribe(1)
	mdl, _ = a.Update(decisionSubReadyMsg{id: "session-a", sub: second})
	a = mdl.(App)

	if a.decSub != second {
		t.Error("the second subscription was not adopted")
	}
	if _, ok := <-first.C; ok {
		t.Error("expected the superseded subscription's channel closed")
	}

	// The real property: with the only live subscription closed, the gate must
	// have no subscribers left at all.
	second.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := gate.Request(ctx, decisionQuestions()); !errors.Is(err, decision.ErrNoClient) {
		t.Errorf("gate.Request after both subscriptions closed = %v; want ErrNoClient — one leaked", err)
	}
}

// TestDecisionPromptHiddenOutsideAttach mirrors the approval prompt's screen
// guard: a.sess carrying a pending decision while the overview is showing
// renders no prompt (and its keys stay the overview's), since only the attach
// screen is backed by a live a.sess.
func TestDecisionPromptHiddenOutsideAttach(t *testing.T) {
	th := theme.Test()
	sess := New(th).IngestDecision(decisionFixtureUpdate())
	a := App{
		theme:  th,
		over:   NewOverview(th, GoldenMeta()),
		sess:   sess,
		scr:    screenOverview,
		width:  testkit.Width,
		height: testkit.Height,
	}
	if strings.Contains(a.render(), "Which approach should I take?") {
		t.Error("overview render contains the decision prompt; want it hidden outside attach")
	}
}

// decisionFixtureUpdate is the package-internal twin of decision_golden_test's
// fixture, for the tests here that seed a Model directly rather than through a
// real gate.
func decisionFixtureUpdate() decision.Update {
	return decision.Update{
		Kind: decision.UpdateRequested,
		Request: decision.Request{
			ID:        "dec-1",
			SessionID: "sess-x",
			Questions: decision.AssignIDs(decisionQuestions()),
		},
	}
}

// TestDecisionPromptRendersWithoutRationaleOrChat covers the render's
// conditional rows: an option with no rationale renders no sub-line at all
// (rather than a blank indented one), and a question that opted out of both
// escape hatches shows neither row — the AllowFreeText/AllowChat defaults are
// opt-OUT, so this is the shape an agent has to ask for explicitly.
func TestDecisionPromptRendersWithoutRationaleOrChat(t *testing.T) {
	th := theme.Test()
	m := New(th).IngestDecision(decision.Update{
		Kind: decision.UpdateRequested,
		Request: decision.Request{
			ID: "dec-1", SessionID: "sess-x",
			Questions: decision.AssignIDs([]acp.DecisionQuestion{{
				Title:    "Ship it?",
				Question: "Ship now or hold?",
				Options:  []acp.DecisionOption{{Label: "Ship"}, {Label: "Hold"}},
			}}),
		},
	})

	got := strings.Join(renderDecisionPrompt(th, *m.pendingDec, testkit.Width), "\n")
	for _, unwanted := range []string{"Type something.", "Chat about this"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("render offers %q for a question that opted out of it:\n%s", unwanted, got)
		}
	}
	if strings.Contains(got, decisionRationaleIndent) {
		t.Errorf("render emitted a rationale sub-line for options that carry none:\n%s", got)
	}
	if rows := len(m.pendingDec.rows()); rows != 2 {
		t.Errorf("rows() = %d; want 2 — only the options are selectable here", rows)
	}
}

// TestRenderDecisionPromptFloorsWidth pins the width < 1 guard every component
// in this package shares: the rule is strings.Repeat'd, so an unfloored width
// would panic on a negative count rather than degrade.
func TestRenderDecisionPromptFloorsWidth(t *testing.T) {
	th := theme.Test()
	m := New(th).IngestDecision(decisionFixtureUpdate())
	for _, width := range []int{-5, 0, 1} {
		lines := renderDecisionPrompt(th, *m.pendingDec, width)
		if len(lines) == 0 || lines[0] != "─" {
			t.Errorf("renderDecisionPrompt at width %d: first line = %q; want the width-1 rule", width, lines[0])
		}
	}
}
