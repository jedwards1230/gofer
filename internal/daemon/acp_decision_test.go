package daemon_test

// acp_decision_test.go covers the spec-ACP half of the structured-decision
// relay: session/request_decision out to a pure ACP peer, its
// RequestDecisionResponse routed into the session's gate, and the first-answer-
// wins race against the gofer-native decision.answer surface. It is the
// decision-side twin of acp_permission_test.go.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// acpDecisionRequest is the subset of a session/request_decision request params
// a test asserts on and answers.
type acpDecisionRequest struct {
	SessionID string `json:"sessionId"`
	Questions []struct {
		QuestionID string `json:"questionId"`
		Title      string `json:"title"`
		Question   string `json:"question"`
		Options    []struct {
			OptionID string `json:"optionId"`
			Label    string `json:"label"`
		} `json:"options"`
	} `json:"questions"`
}

// awaitACPDecisionRequest drains a peer's inbound REQUESTS until it sees a
// session/request_decision and decodes it.
func awaitACPDecisionRequest(t *testing.T, c *wsClient) (json.RawMessage, acpDecisionRequest) {
	t.Helper()
	deadline := time.After(defaultWait)
	for {
		select {
		case f, ok := <-c.inboundRequests:
			if !ok {
				t.Fatal("connection closed waiting for session/request_decision")
			}
			if f.Method != acp.MethodSessionRequestDecision {
				t.Fatalf("inbound request method = %q, want %q", f.Method, acp.MethodSessionRequestDecision)
			}
			var dr acpDecisionRequest
			if err := json.Unmarshal(f.Params, &dr); err != nil {
				t.Fatalf("decode request_decision params: %v", err)
			}
			return f.ID, dr
		case <-deadline:
			t.Fatal("timed out waiting for session/request_decision")
		}
	}
}

// decisionResponse builds a session/request_decision response selecting
// optionID for questionID, with notes attached.
func decisionResponse(questionID, optionID, notes string) acp.RequestDecisionResponse {
	return acp.RequestDecisionResponse{Answers: []acp.DecisionAnswer{{
		QuestionID: questionID,
		Outcome:    acp.DecisionOutcomeSelected{OptionID: optionID},
		Notes:      notes,
	}}}
}

// TestACPDecisionRoundTrip is the phone-answer acceptance test: a pure ACP peer
// receives the spec session/request_decision REQUEST for a turn another peer
// drives, answers it, and the answer — including the free-text note attached to
// it — resolves the driving turn's blocked ask_user.
func TestACPDecisionRoundTrip(t *testing.T) {
	h := newDecisionHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cwd := t.TempDir()
	driver := dial(t, ctx, h.url, nil)
	phone := dial(t, ctx, h.url, nil)

	sid := newACPSession(t, driver, cwd)
	if lr := phone.request("session/load", map[string]any{"sessionId": sid, "cwd": cwd}); lr.Error != nil {
		t.Fatalf("session/load: %v", lr.Error)
	}

	promptDone := make(chan rpcFrame, 1)
	go func() {
		promptDone <- driver.request("session/prompt", map[string]any{"sessionId": sid, "text": "migrate the table"})
	}()

	reqID, dr := awaitACPDecisionRequest(t, phone)
	if dr.SessionID != sid {
		t.Fatalf("request_decision sessionId = %q, want %q", dr.SessionID, sid)
	}
	if len(dr.Questions) != 1 || dr.Questions[0].QuestionID != "q1" {
		t.Fatalf("request_decision questions = %+v, want one question q1", dr.Questions)
	}
	if len(dr.Questions[0].Options) != 2 || dr.Questions[0].Options[1].OptionID != "q1o2" ||
		dr.Questions[0].Options[1].Label != "Shadow table + backfill" {
		t.Fatalf("request_decision options = %+v, want the harness's two labeled options", dr.Questions[0].Options)
	}

	phone.respond(reqID, decisionResponse("q1", "q1o2", "do it overnight"))

	select {
	case res := <-h.fake(sid).results:
		if res.IsError {
			t.Fatalf("ask_user returned an error result: %s", res.Content)
		}
		if !strings.Contains(res.Content, "q1o2") || !strings.Contains(res.Content, "Shadow table + backfill") {
			t.Fatalf("tool result = %q, want the option the ACP peer selected", res.Content)
		}
		// The note is the part a projection is most likely to quietly drop: it is
		// what the human actually SAID, and only the answer carries it.
		if !strings.Contains(res.Content, "do it overnight") {
			t.Fatalf("tool result = %q, want the ACP answer's notes carried through", res.Content)
		}
	case <-time.After(defaultWait):
		t.Fatal("the gate did not unblock after the ACP peer answered")
	}

	// The gofer-native surface still sees the resolution, so a client rendering
	// the prompt clears it.
	waitForNotificationMethod(t, driver, daemon.MethodGoferDecisionResolved)

	select {
	case resp := <-promptDone:
		if resp.Error != nil {
			t.Fatalf("session/prompt: %v", resp.Error)
		}
	case <-time.After(defaultWait):
		t.Fatal("session/prompt did not return after the decision resolved")
	}

	// No daemon-side waiter dangles once the decision resolved.
	waitOutstandingDecisionReqs(t, h, 0)
	waitOpenDecisions(t, h, 0)
}

// TestACPDecisionRaceGoferFirst: a gofer-native peer answers via decision.answer
// before the ACP peer responds. The gate takes the gofer answer, the ACP peer's
// outstanding session/request_decision is retracted (no dangling daemon waiter),
// and a late ACP answer is a harmless no-op rather than a second resolution.
func TestACPDecisionRaceGoferFirst(t *testing.T) {
	h := newDecisionHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cwd := t.TempDir()
	tui := dial(t, ctx, h.url, nil)
	phone := dial(t, ctx, h.url, nil)

	// The TUI identifies as gofer-native by polling the roster before it drives
	// anything, so the daemon never sends it a session/request_decision.
	if r := tui.request("gofer/roster", nil); r.Error != nil {
		t.Fatalf("gofer/roster: %v", r.Error)
	}
	sid := newACPSession(t, tui, cwd)
	if lr := phone.request("session/load", map[string]any{"sessionId": sid, "cwd": cwd}); lr.Error != nil {
		t.Fatalf("session/load: %v", lr.Error)
	}

	promptDone := make(chan rpcFrame, 1)
	go func() {
		promptDone <- tui.request("session/prompt", map[string]any{"sessionId": sid, "text": "ask me"})
	}()

	// The phone gets the spec request; the TUI answers first, gofer-natively.
	reqID, _ := awaitACPDecisionRequest(t, phone)
	req := awaitDecisionRequest(t, tui)
	tui.notify(daemon.MethodDecisionAnswer, selectAnswer(sid, req.ID, "q1", "q1o1", ""))

	select {
	case res := <-h.fake(sid).results:
		if !strings.Contains(res.Content, "q1o1") {
			t.Fatalf("tool result = %q, want the gofer-native answer", res.Content)
		}
	case <-time.After(defaultWait):
		t.Fatal("the gate did not unblock after the gofer-native answer")
	}
	if resp := <-promptDone; resp.Error != nil {
		t.Fatalf("session/prompt: %v", resp.Error)
	}

	// The phone's request was retracted — no daemon-side waiter dangles.
	waitOutstandingDecisionReqs(t, h, 0)
	waitOpenDecisions(t, h, 0)

	// A late ACP answer must not panic, error the connection, or deliver a second
	// resolution to a turn that already finished.
	phone.respond(reqID, decisionResponse("q1", "q1o2", ""))
	select {
	case res := <-h.fake(sid).results:
		t.Fatalf("late ACP answer delivered a second result %q — must be a no-op", res.Content)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestACPDecisionRacePhoneFirst: the ACP peer answers before the gofer-native
// one. The gate takes the ACP answer, the gofer-native peer's prompt clears via
// gofer/decision_resolved, and its own late decision.answer is rejected as
// naming a request that is no longer open rather than resolving anything.
func TestACPDecisionRacePhoneFirst(t *testing.T) {
	h := newDecisionHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cwd := t.TempDir()
	tui := dial(t, ctx, h.url, nil)
	phone := dial(t, ctx, h.url, nil)

	if r := tui.request("gofer/roster", nil); r.Error != nil {
		t.Fatalf("gofer/roster: %v", r.Error)
	}
	sid := newACPSession(t, tui, cwd)
	if lr := phone.request("session/load", map[string]any{"sessionId": sid, "cwd": cwd}); lr.Error != nil {
		t.Fatalf("session/load: %v", lr.Error)
	}

	promptDone := make(chan rpcFrame, 1)
	go func() {
		promptDone <- tui.request("session/prompt", map[string]any{"sessionId": sid, "text": "ask me"})
	}()

	reqID, _ := awaitACPDecisionRequest(t, phone)
	req := awaitDecisionRequest(t, tui)
	phone.respond(reqID, decisionResponse("q1", "q1o2", ""))

	select {
	case res := <-h.fake(sid).results:
		if !strings.Contains(res.Content, "q1o2") {
			t.Fatalf("tool result = %q, want the ACP peer's answer", res.Content)
		}
	case <-time.After(defaultWait):
		t.Fatal("the gate did not unblock after the ACP peer answered")
	}

	// The gofer-native peer's prompt clears.
	waitForNotificationMethod(t, tui, daemon.MethodGoferDecisionResolved)
	waitOpenDecisions(t, h, 0)

	// Its own late answer is refused, not silently accepted.
	resp := tui.request(daemon.MethodDecisionAnswer, selectAnswer(sid, req.ID, "q1", "q1o1", ""))
	if resp.Error == nil {
		t.Fatal("a late gofer-native answer to an already-resolved decision: want error, got success")
	}
	if r := <-promptDone; r.Error != nil {
		t.Fatalf("session/prompt: %v", r.Error)
	}
}

// TestACPDecisionClientRejectsRequest: a client that cannot answer
// session/request_decision (it replies with a JSON-RPC error) must not wedge the
// daemon — the error is a no-op, and a gofer-native answer still resolves the
// turn.
func TestACPDecisionClientRejectsRequest(t *testing.T) {
	h := newDecisionHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cwd := t.TempDir()
	tui := dial(t, ctx, h.url, nil)
	phone := dial(t, ctx, h.url, nil)

	if r := tui.request("gofer/roster", nil); r.Error != nil {
		t.Fatalf("gofer/roster: %v", r.Error)
	}
	sid := newACPSession(t, tui, cwd)
	if lr := phone.request("session/load", map[string]any{"sessionId": sid, "cwd": cwd}); lr.Error != nil {
		t.Fatalf("session/load: %v", lr.Error)
	}

	promptDone := make(chan rpcFrame, 1)
	go func() {
		promptDone <- tui.request("session/prompt", map[string]any{"sessionId": sid, "text": "ask me"})
	}()

	reqID, _ := awaitACPDecisionRequest(t, phone)
	phone.respondError(reqID, -32601, "method not found")

	req := awaitDecisionRequest(t, tui)
	tui.notify(daemon.MethodDecisionAnswer, selectAnswer(sid, req.ID, "q1", "q1o1", ""))
	select {
	case res := <-h.fake(sid).results:
		if !strings.Contains(res.Content, "q1o1") {
			t.Fatalf("tool result = %q, want the gofer-native answer", res.Content)
		}
	case <-time.After(defaultWait):
		t.Fatal("the gate did not unblock after the gofer-native answer")
	}
	if resp := <-promptDone; resp.Error != nil {
		t.Fatalf("session/prompt: %v", resp.Error)
	}
	waitOutstandingDecisionReqs(t, h, 0)
}

// TestACPDecisionInterruptWhilePending: a decision is outstanding at an ACP peer
// when the turn is interrupted (session/cancel). The interrupt drops the request
// at the gate, which must clear its route and retract the peer's outstanding
// session/request_decision rather than leaving either dangling — and every peer
// is told the prompt is gone.
func TestACPDecisionInterruptWhilePending(t *testing.T) {
	h := newDecisionHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cwd := t.TempDir()
	driver := dial(t, ctx, h.url, nil)
	phone := dial(t, ctx, h.url, nil)
	sid := newACPSession(t, driver, cwd)
	if lr := phone.request("session/load", map[string]any{"sessionId": sid, "cwd": cwd}); lr.Error != nil {
		t.Fatalf("session/load: %v", lr.Error)
	}

	promptDone := make(chan rpcFrame, 1)
	go func() {
		promptDone <- driver.request("session/prompt", map[string]any{"sessionId": sid, "text": "ask me"})
	}()

	// The phone holds the pending ACP request; there is exactly one open decision.
	awaitACPDecisionRequest(t, phone)
	awaitDecisionRequest(t, driver)
	waitOpenDecisions(t, h, 1)
	waitOutstandingDecisionReqs(t, h, 1)

	driver.notify("session/cancel", map[string]any{"sessionId": sid})

	// The interrupt releases the route AND retracts the phone's request.
	waitOpenDecisions(t, h, 0)
	waitOutstandingDecisionReqs(t, h, 0)
	if got := h.d.RetainedDecisionCount(); got != 0 {
		t.Fatalf("retained decisions after interrupt = %d, want 0", got)
	}

	// Both peers are told the prompt is gone, so neither is left rendering a
	// question nothing is waiting on.
	waitForNotificationMethod(t, driver, daemon.MethodGoferDecisionResolved)
	waitForNotificationMethod(t, phone, daemon.MethodGoferDecisionResolved)

	select {
	case <-promptDone:
	case <-time.After(defaultWait):
		t.Fatal("session/prompt did not return after the interrupt")
	}

	// An answer arriving after the interrupt resolves nothing and says so.
	resp := driver.request(daemon.MethodDecisionAnswer, selectAnswer(sid, "dec-1", "q1", "q1o1", ""))
	if resp.Error == nil {
		t.Fatal("answering an interrupted decision: want error, got success")
	}
}
