package supervisor_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"

	"github.com/jedwards1230/gofer/internal/decision"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// askTwoOptions is the model-side JSON the decision tests arm their fake
// session's turn with — one question, two options, escape hatches defaulted.
const askTwoOptions = `{"questions":[{
	"title": "Migration strategy",
	"question": "Which approach should I take?",
	"options": [
		{"label":"In-place ALTER"},
		{"label":"Shadow table + backfill","recommended":true}
	]
}]}`

// waitForDecision reads the next update off sub or fails the test.
func waitForDecision(t *testing.T, sub *decision.Subscription) decision.Update {
	t.Helper()
	select {
	case up, ok := <-sub.C:
		if !ok {
			t.Fatal("decision subscription closed unexpectedly")
		}
		return up
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a decision update")
		return decision.Update{}
	}
}

// TestAnswerDecisionRoundTrip is the supervisor-level decision round trip,
// driven through the REAL wiring: the turn resolves ask_user out of the
// registry the supervisor injected (proving WrapRegistry registered gofer's own
// tool), the request surfaces on SubscribeDecisions, and AnswerDecision
// unblocks the tool with a typed answer.
func TestAnswerDecisionRoundTrip(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	info, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: h.root, Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sub, err := h.sup.SubscribeDecisions(info.ID, 4)
	if err != nil {
		t.Fatalf("SubscribeDecisions: %v", err)
	}
	defer sub.Close()

	fs := h.session(info.ID)
	fs.setAskInput(askTwoOptions)
	if err := h.sup.Send(ctx, info.ID, "migrate the table"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fs.waitStarted(t)

	up := waitForDecision(t, sub)
	if up.Kind != decision.UpdateRequested {
		t.Fatalf("update = %v, want requested", up.Kind)
	}
	if up.Request.SessionID != info.ID {
		t.Errorf("request session = %q, want %q", up.Request.SessionID, info.ID)
	}
	if up.Request.ID != "dec-1" {
		t.Errorf("request id = %q, want dec-1", up.Request.ID)
	}

	if err := h.sup.AnswerDecision(info.ID, up.Request.ID, []acp.DecisionAnswer{
		{QuestionID: "q1", Outcome: acp.DecisionOutcomeSelected{OptionID: "q1o2"}},
	}); err != nil {
		t.Fatalf("AnswerDecision: %v", err)
	}

	waitForStatus(t, h.sup, info.ID, supervisor.StatusNeedsInput)

	res, runErr := fs.askOutcome()
	if runErr != nil {
		t.Fatalf("ask_user err = %v", runErr)
	}
	if res.IsError {
		t.Errorf("ask_user IsError = true, want false (content %q)", res.Content)
	}
	if want := `selected q1o2 "Shadow table + backfill"`; !strings.Contains(res.Content, want) {
		t.Errorf("ask_user content = %q, want it to contain %q", res.Content, want)
	}
	if resolved := waitForDecision(t, sub); resolved.Kind != decision.UpdateResolved {
		t.Errorf("second update = %v, want resolved", resolved.Kind)
	}
}

// TestAnswerDecisionRoutesToTheRightSession pins the per-session gate: a
// request id is unique only within its session, so answering session B with
// session A's id must not resolve anything.
func TestAnswerDecisionRoutesToTheRightSession(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	a, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: h.root, Model: "m"})
	if err != nil {
		t.Fatalf("Create a: %v", err)
	}
	b, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: h.root, Model: "m"})
	if err != nil {
		t.Fatalf("Create b: %v", err)
	}
	subA, err := h.sup.SubscribeDecisions(a.ID, 4)
	if err != nil {
		t.Fatalf("SubscribeDecisions a: %v", err)
	}
	defer subA.Close()

	fs := h.session(a.ID)
	fs.setAskInput(askTwoOptions)
	if err := h.sup.Send(ctx, a.ID, "ask"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fs.waitStarted(t)
	up := waitForDecision(t, subA)

	answers := []acp.DecisionAnswer{{QuestionID: "q1", Outcome: acp.DecisionOutcomeSelected{OptionID: "q1o1"}}}
	// b's gate has no such request — b must reject it, and a's must stay open.
	err = h.sup.AnswerDecision(b.ID, up.Request.ID, answers)
	if !errors.Is(err, decision.ErrUnknownRequest) {
		t.Fatalf("AnswerDecision(b) = %v, want ErrUnknownRequest", err)
	}

	if err := h.sup.AnswerDecision(a.ID, up.Request.ID, answers); err != nil {
		t.Fatalf("AnswerDecision(a): %v", err)
	}
	waitForStatus(t, h.sup, a.ID, supervisor.StatusNeedsInput)
}

// TestDecisionsUnknownSession pins ErrNotLive on both new methods, matching
// Reply's handling of a session that is not (or no longer) live.
func TestDecisionsUnknownSession(t *testing.T) {
	h := newHarness(t)

	err := h.sup.AnswerDecision("does-not-exist", "dec-1", nil)
	if !errors.Is(err, supervisor.ErrNotLive) {
		t.Errorf("AnswerDecision(unknown) = %v, want ErrNotLive", err)
	}
	sub, err := h.sup.SubscribeDecisions("does-not-exist", 1)
	if !errors.Is(err, supervisor.ErrNotLive) {
		t.Errorf("SubscribeDecisions(unknown) = %v, want ErrNotLive", err)
	}
	if sub != nil {
		t.Error("SubscribeDecisions(unknown) returned a subscription, want nil")
	}
}

// TestInterruptReleasesBlockedAskUser is the decision-side twin of
// TestCancelReleasesAwaitNoLeak: interrupting a turn blocked in ask_user
// unwinds it with the ctx error (aborting the turn rather than feeding the
// model a result), drops the open request, and leaves no goroutine behind —
// Kill joins the pump, so a leaked waiter would hang this test.
func TestInterruptReleasesBlockedAskUser(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	info, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: h.root, Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sub, err := h.sup.SubscribeDecisions(info.ID, 4)
	if err != nil {
		t.Fatalf("SubscribeDecisions: %v", err)
	}
	defer sub.Close()

	fs := h.session(info.ID)
	fs.setAskInput(askTwoOptions)
	if err := h.sup.Send(ctx, info.ID, "ask"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fs.waitStarted(t)
	up := waitForDecision(t, sub)

	if err := h.sup.Interrupt(ctx, info.ID); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	waitForStatus(t, h.sup, info.ID, supervisor.StatusNeedsInput)

	if _, runErr := fs.askOutcome(); !errors.Is(runErr, context.Canceled) {
		t.Errorf("ask_user err = %v, want context.Canceled", runErr)
	}
	if resolved := waitForDecision(t, sub); resolved.Kind != decision.UpdateResolved {
		t.Errorf("update after interrupt = %v, want resolved", resolved.Kind)
	}
	// The dropped request is unanswerable — that is what tells a client still
	// rendering the prompt that it is stale.
	if err := h.sup.AnswerDecision(info.ID, up.Request.ID, nil); !errors.Is(err, decision.ErrUnknownRequest) {
		t.Errorf("AnswerDecision after interrupt = %v, want ErrUnknownRequest", err)
	}

	done := make(chan error, 1)
	go func() { done <- h.sup.Kill(ctx, info.ID) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Kill: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Kill did not return — a session goroutine leaked")
	}
}

// TestAskUserWithNoSubscriberDoesNotHangTheTurn pins the ErrNoClient path end
// to end: with nothing attached, the tool returns an IsError result naming the
// alternative and the turn completes on its own rather than blocking until the
// session is interrupted.
func TestAskUserWithNoSubscriberDoesNotHangTheTurn(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	info, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: h.root, Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(info.ID)
	fs.setAskInput(askTwoOptions)

	if err := h.sup.Send(ctx, info.ID, "ask"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fs.waitStarted(t)
	waitForStatus(t, h.sup, info.ID, supervisor.StatusNeedsInput)

	res, runErr := fs.askOutcome()
	if runErr != nil {
		t.Fatalf("ask_user err = %v", runErr)
	}
	if !res.IsError {
		t.Errorf("IsError = false, want true (content %q)", res.Content)
	}
	if !strings.Contains(res.Content, "no client is attached") {
		t.Errorf("content = %q, want the no-client message", res.Content)
	}
}
