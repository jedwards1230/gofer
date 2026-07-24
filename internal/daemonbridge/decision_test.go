package daemonbridge_test

// decision_test.go pins the two halves of the bridge's structured-decision
// contract against a raw WebSocket server — the same technique permission_test.go
// uses for permission.reply, and for the same reason: these tests are about the
// FRAME this package puts on the wire and the frame it reads off it, not about
// the daemon's behavior (internal/daemon's own decision suites cover that end to
// end).
//
//   - Supervisor.AnswerDecision sends a bare "decision.answer" notification with
//     params {sessionId, id, answers} — including the sessionId a permission
//     reply does not carry, because a decision request id is session-scoped.
//   - Supervisor.Decisions surfaces a gofer/decision_requested notification as a
//     decision.Update carrying the questions verbatim.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jedwards1230/agent-sdk-go/acp"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/daemonbridge"
	"github.com/jedwards1230/gofer/internal/decision"
)

// newDecisionWSServer starts an httptest.Server that accepts one WebSocket
// connection, PUSHES every frame sent on the returned push channel, and
// delivers every frame it reads on the returned frames channel. Any inbound
// JSON-RPC request (the reconstruction core's own gofer/roster + session/load on
// first reference of a session) is answered with an empty result, so the core's
// history load settles instead of blocking a goroutine for the whole test.
func newDecisionWSServer(t *testing.T) (url string, push chan<- any, frames <-chan map[string]any) {
	t.Helper()
	pushCh := make(chan any, 4)
	frameCh := make(chan map[string]any, 16)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
		ctx := r.Context()

		go func() {
			for {
				select {
				case v := <-pushCh:
					data, merr := json.Marshal(v)
					if merr != nil {
						return
					}
					if werr := conn.Write(ctx, websocket.MessageText, data); werr != nil {
						return
					}
				case <-ctx.Done():
					return
				}
			}
		}()

		for {
			_, data, rerr := conn.Read(ctx)
			if rerr != nil {
				return
			}
			var frame map[string]any
			if json.Unmarshal(data, &frame) != nil {
				continue
			}
			select {
			case frameCh <- frame:
			default:
			}
			// Answer any request so the core's first-reference history load
			// settles; a notification (no id) needs no response.
			if id, ok := frame["id"]; ok {
				resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
				if werr := conn.Write(ctx, websocket.MessageText, resp); werr != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + srv.URL[len("http"):], pushCh, frameCh
}

// newDecisionBridge dials the server at url and returns a Supervisor over it.
func newDecisionBridge(t *testing.T, url string) *daemonbridge.Supervisor {
	t.Helper()
	c, err := daemon.Dial(context.Background(), url, "")
	if err != nil {
		t.Fatalf("daemon.Dial: %v", err)
	}
	b := daemonbridge.New(c)
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// awaitFrame drains frames until one with method arrives.
func awaitFrame(t *testing.T, frames <-chan map[string]any, method string) map[string]any {
	t.Helper()
	deadline := time.After(defaultWait)
	for {
		select {
		case f := <-frames:
			if f["method"] == method {
				return f
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a %q frame", method)
		}
	}
}

// TestAnswerDecisionSendsBareNotification asserts the exact decision.answer
// frame: a notification (no "id" key — the daemon sends no response), method
// "decision.answer", params {sessionId, id, answers:[{questionId, outcome,
// notes}]} with the outcome in its acp tagged-union form.
func TestAnswerDecisionSendsBareNotification(t *testing.T) {
	url, _, frames := newDecisionWSServer(t)
	b := newDecisionBridge(t, url)

	answers := []acp.DecisionAnswer{{
		QuestionID: "q1",
		Outcome:    acp.DecisionOutcomeSelected{OptionID: "q1o2"},
		Notes:      "do it overnight",
	}}
	if err := b.AnswerDecision(context.Background(), "sess-1", "dec-1", answers); err != nil {
		t.Fatalf("AnswerDecision: %v", err)
	}

	frame := awaitFrame(t, frames, "decision.answer")
	if _, hasID := frame["id"]; hasID {
		t.Errorf("frame has an \"id\" key (%v): want a bare notification, not a Call", frame["id"])
	}
	params, ok := frame["params"].(map[string]any)
	if !ok {
		t.Fatalf("params = %v (%T), want an object", frame["params"], frame["params"])
	}
	// The sessionId is the field that distinguishes this op from permission.reply:
	// a decision request id is minted per session, so an id alone cannot name one.
	if params["sessionId"] != "sess-1" {
		t.Errorf("params.sessionId = %v, want %q", params["sessionId"], "sess-1")
	}
	if params["id"] != "dec-1" {
		t.Errorf("params.id = %v, want %q", params["id"], "dec-1")
	}
	list, ok := params["answers"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("params.answers = %v, want a one-element array", params["answers"])
	}
	answer, ok := list[0].(map[string]any)
	if !ok {
		t.Fatalf("answers[0] = %v (%T), want an object", list[0], list[0])
	}
	if answer["questionId"] != "q1" {
		t.Errorf("answers[0].questionId = %v, want %q", answer["questionId"], "q1")
	}
	if answer["notes"] != "do it overnight" {
		t.Errorf("answers[0].notes = %v, want the client's note", answer["notes"])
	}
	outcome, ok := answer["outcome"].(map[string]any)
	if !ok {
		t.Fatalf("answers[0].outcome = %v (%T), want an object", answer["outcome"], answer["outcome"])
	}
	if outcome["outcome"] != "selected" || outcome["optionId"] != "q1o2" {
		t.Errorf("outcome = %v, want {outcome:selected, optionId:q1o2}", outcome)
	}
}

// TestAnswerDecisionHonorsContext: a caller that has already given up must not
// put a frame on the shared daemon link.
func TestAnswerDecisionHonorsContext(t *testing.T) {
	url, _, _ := newDecisionWSServer(t)
	b := newDecisionBridge(t, url)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.AnswerDecision(ctx, "sess-1", "dec-1", nil); err == nil {
		t.Fatal("AnswerDecision(cancelled ctx) = nil, want the context error")
	}
}

// TestDecisionsSurfacesRequestedNotification: a gofer/decision_requested pushed
// by the daemon arrives on a Decisions subscription as an UpdateRequested
// carrying the questions verbatim — the client half of the relay. The pushed
// params are exactly internal/daemon's decisionRequestedParams shape.
func TestDecisionsSurfacesRequestedNotification(t *testing.T) {
	url, push, _ := newDecisionWSServer(t)
	b := newDecisionBridge(t, url)

	sub, err := b.Decisions(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("Decisions: %v", err)
	}
	defer sub.Close()

	push <- map[string]any{
		"jsonrpc": "2.0",
		"method":  "gofer/decision_requested",
		"params": map[string]any{
			"sessionId": "sess-1",
			"id":        "dec-1",
			"questions": []any{map[string]any{
				"questionId":    "q1",
				"title":         "Migration strategy",
				"question":      "Which approach should I take?",
				"allowFreeText": true,
				"allowChat":     true,
				"options": []any{
					map[string]any{"optionId": "q1o1", "label": "In-place ALTER", "rationale": "fastest"},
					map[string]any{"optionId": "q1o2", "label": "Shadow table", "recommended": true},
				},
			}},
		},
	}

	u := awaitUpdate(t, sub)
	if u.Kind != decision.UpdateRequested {
		t.Fatalf("update kind = %v, want requested", u.Kind)
	}
	if u.Request.ID != "dec-1" || u.Request.SessionID != "sess-1" {
		t.Fatalf("update request = %s/%s, want sess-1/dec-1", u.Request.SessionID, u.Request.ID)
	}
	if len(u.Request.Questions) != 1 {
		t.Fatalf("update carried %d questions, want 1", len(u.Request.Questions))
	}
	q := u.Request.Questions[0]
	if q.QuestionID != "q1" || q.Title != "Migration strategy" || q.Question != "Which approach should I take?" {
		t.Fatalf("question = %+v, want the pushed question verbatim", q)
	}
	// The escape hatches and the option metadata are what a client renders; a
	// lossy decode here would silently strip the agent's own affordances.
	if !q.AllowFreeText || !q.AllowChat {
		t.Fatalf("allowFreeText=%v allowChat=%v, want both true", q.AllowFreeText, q.AllowChat)
	}
	if len(q.Options) != 2 || q.Options[0].OptionID != "q1o1" || q.Options[0].Rationale != "fastest" ||
		q.Options[1].OptionID != "q1o2" || !q.Options[1].Recommended {
		t.Fatalf("options = %+v, want both options with their ids/rationale/recommended intact", q.Options)
	}

	// The matching resolution clears it.
	push <- map[string]any{
		"jsonrpc": "2.0",
		"method":  "gofer/decision_resolved",
		"params":  map[string]any{"sessionId": "sess-1", "id": "dec-1"},
	}
	r := awaitUpdate(t, sub)
	if r.Kind != decision.UpdateResolved || r.Request.ID != "dec-1" {
		t.Fatalf("update = %v/%s, want resolved/dec-1", r.Kind, r.Request.ID)
	}
}

// TestDecisionsReplaysOpenRequestToALateSubscriber: a request that arrived
// before anything subscribed is replayed to the next subscriber. That is what
// keeps a TUI switching sessions (or re-attaching) from missing a question the
// connection already delivered.
func TestDecisionsReplaysOpenRequestToALateSubscriber(t *testing.T) {
	url, push, _ := newDecisionWSServer(t)
	b := newDecisionBridge(t, url)

	// Reference the session first so the notification is not dropped, then let
	// the request arrive with nobody subscribed.
	first, err := b.Decisions(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("Decisions: %v", err)
	}
	push <- map[string]any{
		"jsonrpc": "2.0",
		"method":  "gofer/decision_requested",
		"params": map[string]any{
			"sessionId": "sess-1",
			"id":        "dec-7",
			"questions": []any{map[string]any{"questionId": "q1", "question": "Which?"}},
		},
	}
	awaitUpdate(t, first) // the request has landed on the stream
	first.Close()

	late, err := b.Decisions(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("Decisions (late): %v", err)
	}
	defer late.Close()

	u := awaitUpdate(t, late)
	if u.Kind != decision.UpdateRequested || u.Request.ID != "dec-7" {
		t.Fatalf("late subscriber saw %v/%s, want a replayed requested/dec-7", u.Kind, u.Request.ID)
	}
}

// TestDecisionsClosedByBridgeClose: closing the bridge closes every decision
// subscription, so a consumer's pump unwinds through the same "channel closed"
// path it uses for an event subscription rather than parking forever.
func TestDecisionsClosedByBridgeClose(t *testing.T) {
	url, _, _ := newDecisionWSServer(t)
	c, err := daemon.Dial(context.Background(), url, "")
	if err != nil {
		t.Fatalf("daemon.Dial: %v", err)
	}
	b := daemonbridge.New(c)

	sub, err := b.Decisions(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("Decisions: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	deadline := time.After(defaultWait)
	for {
		select {
		case _, ok := <-sub.C:
			if !ok {
				return // closed, as required
			}
		case <-deadline:
			t.Fatal("the decision subscription stayed open after the bridge closed")
		}
	}
}

// awaitUpdate reads one update off sub, failing on timeout or a closed stream.
func awaitUpdate(t *testing.T, sub *decision.Subscription) decision.Update {
	t.Helper()
	select {
	case u, ok := <-sub.C:
		if !ok {
			t.Fatal("decision subscription closed while waiting for an update")
		}
		return u
	case <-time.After(defaultWait):
		t.Fatal("timed out waiting for a decision update")
	}
	return decision.Update{}
}
