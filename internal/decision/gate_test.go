package decision

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
)

// twoQuestions is the fixture most gate tests open a request with: one
// two-option question and one free-text-only question, so a single request
// exercises both the option-id space and the "no options" shape.
func twoQuestions() []acp.DecisionQuestion {
	return []acp.DecisionQuestion{
		{
			Title:    "Migration strategy",
			Question: "Which approach should I take?",
			Options: []acp.DecisionOption{
				{Label: "In-place ALTER", Rationale: "fastest, but locks the table"},
				{Label: "Shadow table + backfill", Rationale: "online, but doubles disk", Recommended: true},
			},
			AllowFreeText: true,
			AllowChat:     true,
		},
		{
			Title:         "Retention",
			Question:      "How long should we keep the old table?",
			AllowFreeText: true,
		},
	}
}

// openRequest opens a request on g from a background goroutine and returns the
// channel its result will arrive on, plus the request the subscriber observed.
// It fails the test if no UpdateRequested shows up promptly — every caller
// needs the request id before it can do anything useful.
func openRequest(t *testing.T, ctx context.Context, g *Gate, sub *Subscription, questions []acp.DecisionQuestion) (<-chan requestResult, Request) {
	t.Helper()
	out := make(chan requestResult, 1)
	go func() {
		answers, err := g.Request(ctx, questions)
		out <- requestResult{answers: answers, err: err}
	}()
	up := recvUpdate(t, sub)
	if up.Kind != UpdateRequested {
		t.Fatalf("first update = %v, want requested", up.Kind)
	}
	return out, up.Request
}

// requestResult is one Gate.Request return, ferried off its goroutine.
type requestResult struct {
	answers []acp.DecisionAnswer
	err     error
}

// recvUpdate reads one update or fails the test — a missing update is always a
// bug in the code under test, never a slow machine, since every publish here is
// non-blocking and happens before the call that triggers it returns.
func recvUpdate(t *testing.T, sub *Subscription) Update {
	t.Helper()
	select {
	case up, ok := <-sub.C:
		if !ok {
			t.Fatal("subscription closed while waiting for an update")
		}
		return up
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a decision update")
		return Update{}
	}
}

// recvResult reads one Gate.Request return or fails the test.
func recvResult(t *testing.T, out <-chan requestResult) requestResult {
	t.Helper()
	select {
	case r := <-out:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Request to return")
		return requestResult{}
	}
}

func TestGateRequestNoSubscriber(t *testing.T) {
	g := NewGate("sess-1")

	answers, err := g.Request(context.Background(), twoQuestions())

	if !errors.Is(err, ErrNoClient) {
		t.Fatalf("err = %v, want ErrNoClient", err)
	}
	if answers != nil {
		t.Fatalf("answers = %v, want nil", answers)
	}
	// The whole point of failing fast is that nothing is left behind for a
	// later client to discover and answer.
	if open := g.Open(); len(open) != 0 {
		t.Fatalf("open = %v, want none", open)
	}
}

func TestGateRequestAssignsIDs(t *testing.T) {
	g := NewGate("sess-1")
	sub := g.Subscribe(4)
	defer sub.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out, req := openRequest(t, ctx, g, sub, twoQuestions())

	if req.ID != "dec-1" {
		t.Errorf("request id = %q, want dec-1", req.ID)
	}
	if req.SessionID != "sess-1" {
		t.Errorf("session id = %q, want sess-1", req.SessionID)
	}
	got := []string{req.Questions[0].QuestionID, req.Questions[1].QuestionID}
	if want := []string{"q1", "q2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("question ids = %v, want %v", got, want)
	}
	gotOpts := []string{req.Questions[0].Options[0].OptionID, req.Questions[0].Options[1].OptionID}
	if want := []string{"q1o1", "q1o2"}; !reflect.DeepEqual(gotOpts, want) {
		t.Errorf("option ids = %v, want %v", gotOpts, want)
	}

	cancel()
	recvResult(t, out)
}

func TestGateAnswerRoundTrip(t *testing.T) {
	g := NewGate("sess-1")
	sub := g.Subscribe(4)
	defer sub.Close()

	out, req := openRequest(t, context.Background(), g, sub, twoQuestions())

	if err := g.Answer(req.ID, []acp.DecisionAnswer{
		{QuestionID: "q1", Outcome: acp.DecisionOutcomeSelected{OptionID: "q1o2"}, Notes: "stay online"},
		{QuestionID: "q2", Outcome: acp.DecisionOutcomeText{Text: "30 days"}},
	}); err != nil {
		t.Fatalf("Answer: %v", err)
	}

	res := recvResult(t, out)
	if res.err != nil {
		t.Fatalf("Request err = %v", res.err)
	}
	if len(res.answers) != 2 {
		t.Fatalf("answers = %d, want 2", len(res.answers))
	}
	if sel, ok := res.answers[0].Outcome.(acp.DecisionOutcomeSelected); !ok || sel.OptionID != "q1o2" {
		t.Errorf("answer 0 outcome = %#v, want selected q1o2", res.answers[0].Outcome)
	}
	if res.answers[0].Notes != "stay online" {
		t.Errorf("answer 0 notes = %q, want %q", res.answers[0].Notes, "stay online")
	}
	if txt, ok := res.answers[1].Outcome.(acp.DecisionOutcomeText); !ok || txt.Text != "30 days" {
		t.Errorf("answer 1 outcome = %#v, want text", res.answers[1].Outcome)
	}

	up := recvUpdate(t, sub)
	if up.Kind != UpdateResolved || up.Request.ID != req.ID {
		t.Errorf("resolve update = %v %q, want resolved %q", up.Kind, up.Request.ID, req.ID)
	}
	if open := g.Open(); len(open) != 0 {
		t.Errorf("open after answer = %v, want none", open)
	}
}

func TestGateAnswerFillsMissingAsCancelled(t *testing.T) {
	g := NewGate("sess-1")
	sub := g.Subscribe(4)
	defer sub.Close()

	out, req := openRequest(t, context.Background(), g, sub, twoQuestions())

	// Only q2 answered: the tool must still get one answer per question, in
	// question order, so it can format its result by iterating questions.
	if err := g.Answer(req.ID, []acp.DecisionAnswer{
		{QuestionID: "q2", Outcome: acp.DecisionOutcomeChat{}},
	}); err != nil {
		t.Fatalf("Answer: %v", err)
	}

	res := recvResult(t, out)
	if res.err != nil {
		t.Fatalf("Request err = %v", res.err)
	}
	if len(res.answers) != 2 {
		t.Fatalf("answers = %d, want 2", len(res.answers))
	}
	if res.answers[0].QuestionID != "q1" || res.answers[1].QuestionID != "q2" {
		t.Fatalf("answers out of question order: %q, %q", res.answers[0].QuestionID, res.answers[1].QuestionID)
	}
	if _, ok := res.answers[0].Outcome.(acp.DecisionOutcomeCancelled); !ok {
		t.Errorf("unanswered q1 outcome = %#v, want cancelled", res.answers[0].Outcome)
	}
	if _, ok := res.answers[1].Outcome.(acp.DecisionOutcomeChat); !ok {
		t.Errorf("q2 outcome = %#v, want chat", res.answers[1].Outcome)
	}
}

func TestGateAnswerRejects(t *testing.T) {
	tests := []struct {
		name    string
		answers []acp.DecisionAnswer
		want    string
	}{
		{
			name:    "unknown question",
			answers: []acp.DecisionAnswer{{QuestionID: "q9", Outcome: acp.DecisionOutcomeChat{}}},
			want:    "unknown question",
		},
		{
			name: "unknown option",
			answers: []acp.DecisionAnswer{
				{QuestionID: "q1", Outcome: acp.DecisionOutcomeSelected{OptionID: "q1o9"}},
			},
			want: "has no option",
		},
		{
			name: "option from another question",
			answers: []acp.DecisionAnswer{
				{QuestionID: "q2", Outcome: acp.DecisionOutcomeSelected{OptionID: "q1o1"}},
			},
			want: "has no option",
		},
		{
			name: "duplicate answer",
			answers: []acp.DecisionAnswer{
				{QuestionID: "q1", Outcome: acp.DecisionOutcomeChat{}},
				{QuestionID: "q1", Outcome: acp.DecisionOutcomeChat{}},
			},
			want: "more than once",
		},
		{
			name:    "no outcome",
			answers: []acp.DecisionAnswer{{QuestionID: "q1"}},
			want:    "no outcome",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGate("sess-1")
			sub := g.Subscribe(4)
			defer sub.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			out, req := openRequest(t, ctx, g, sub, twoQuestions())

			err := g.Answer(req.ID, tc.answers)
			if err == nil {
				t.Fatalf("Answer accepted %v, want a rejection", tc.answers)
			}
			if got := err.Error(); !strings.Contains(got, tc.want) {
				t.Errorf("err = %q, want it to mention %q", got, tc.want)
			}
			// A rejected answer must leave the request open so the client can
			// correct and retry.
			if open := g.Open(); len(open) != 1 || open[0].ID != req.ID {
				t.Errorf("open after rejection = %v, want the request still open", open)
			}

			cancel()
			recvResult(t, out)
		})
	}
}

func TestGateAnswerUnknownRequest(t *testing.T) {
	g := NewGate("sess-1")
	sub := g.Subscribe(4)
	defer sub.Close()

	err := g.Answer("dec-404", []acp.DecisionAnswer{{QuestionID: "q1", Outcome: acp.DecisionOutcomeChat{}}})

	if !errors.Is(err, ErrUnknownRequest) {
		t.Fatalf("err = %v, want ErrUnknownRequest", err)
	}
}

func TestGateAnswerTwiceIsUnknownTheSecondTime(t *testing.T) {
	g := NewGate("sess-1")
	sub := g.Subscribe(4)
	defer sub.Close()

	out, req := openRequest(t, context.Background(), g, sub, twoQuestions())
	answers := []acp.DecisionAnswer{{QuestionID: "q1", Outcome: acp.DecisionOutcomeChat{}}}
	if err := g.Answer(req.ID, answers); err != nil {
		t.Fatalf("first Answer: %v", err)
	}
	recvResult(t, out)

	// The peer-raced case: two clients both answer the same prompt. The second
	// must be told the request is gone rather than silently succeeding.
	if err := g.Answer(req.ID, answers); !errors.Is(err, ErrUnknownRequest) {
		t.Fatalf("second Answer err = %v, want ErrUnknownRequest", err)
	}
}

func TestGateRequestContextCancelReleasesAndDrops(t *testing.T) {
	g := NewGate("sess-1")
	sub := g.Subscribe(4)
	defer sub.Close()
	ctx, cancel := context.WithCancel(context.Background())

	out, req := openRequest(t, ctx, g, sub, twoQuestions())
	cancel()

	res := recvResult(t, out)
	if !errors.Is(res.err, context.Canceled) {
		t.Fatalf("Request err = %v, want context.Canceled", res.err)
	}
	// No map-entry leak: an interrupted turn's request must not linger where a
	// later client could see (and try to answer) it.
	if open := g.Open(); len(open) != 0 {
		t.Fatalf("open after cancel = %v, want none", open)
	}
	up := recvUpdate(t, sub)
	if up.Kind != UpdateResolved || up.Request.ID != req.ID {
		t.Errorf("update = %v %q, want resolved %q", up.Kind, up.Request.ID, req.ID)
	}
	// And the dropped request is unanswerable, which is what tells a client
	// still rendering the prompt that it is stale.
	if err := g.Answer(req.ID, nil); !errors.Is(err, ErrUnknownRequest) {
		t.Errorf("Answer after cancel = %v, want ErrUnknownRequest", err)
	}
}

func TestGateSubscribeReplaysOpenRequests(t *testing.T) {
	g := NewGate("sess-1")
	opener := g.Subscribe(4)
	defer opener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	out, req := openRequest(t, ctx, g, opener, twoQuestions())

	// A second client attaching mid-flight must see the prompt the agent is
	// blocked on, not an empty screen.
	late := g.Subscribe(4)
	defer late.Close()

	up := recvUpdate(t, late)
	if up.Kind != UpdateRequested || up.Request.ID != req.ID {
		t.Fatalf("replayed update = %v %q, want requested %q", up.Kind, up.Request.ID, req.ID)
	}
	if len(up.Request.Questions) != 2 {
		t.Errorf("replayed questions = %d, want 2", len(up.Request.Questions))
	}

	// And it can answer what it was replayed.
	if err := g.Answer(up.Request.ID, []acp.DecisionAnswer{
		{QuestionID: "q1", Outcome: acp.DecisionOutcomeSelected{OptionID: "q1o1"}},
	}); err != nil {
		t.Fatalf("Answer from late subscriber: %v", err)
	}
	if res := recvResult(t, out); res.err != nil {
		t.Fatalf("Request err = %v", res.err)
	}
}

func TestGateSubscribeReplayFitsUnbufferedSubscriber(t *testing.T) {
	g := NewGate("sess-1")
	opener := g.Subscribe(4)
	defer opener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	openRequest(t, ctx, g, opener, twoQuestions())

	// buffer 0 must still receive the replay — the channel is sized to fit it
	// (matching event.Broker.Subscribe), or Subscribe would deadlock on itself.
	late := g.Subscribe(0)
	defer late.Close()
	if up := recvUpdate(t, late); up.Kind != UpdateRequested {
		t.Fatalf("replayed update = %v, want requested", up.Kind)
	}
}

func TestGateSlowSubscriberDoesNotBlock(t *testing.T) {
	g := NewGate("sess-1")
	wedged := g.Subscribe(0) // never drained past the initial (empty) replay
	defer wedged.Close()
	live := g.Subscribe(4)
	defer live.Close()

	// A client that stopped reading must not be able to hang the agent's turn
	// inside Request: the publish drops and counts instead of blocking.
	out, req := openRequest(t, context.Background(), g, live, twoQuestions())
	if err := g.Answer(req.ID, []acp.DecisionAnswer{{QuestionID: "q1", Outcome: acp.DecisionOutcomeChat{}}}); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if res := recvResult(t, out); res.err != nil {
		t.Fatalf("Request err = %v", res.err)
	}
	if got := wedged.Dropped(); got == 0 {
		t.Errorf("wedged subscriber dropped = 0, want the undelivered updates counted")
	}
}

func TestGateSubscriptionCloseIsIdempotent(t *testing.T) {
	g := NewGate("sess-1")
	sub := g.Subscribe(1)

	sub.Close()
	sub.Close() // must not panic on a double close

	if _, ok := <-sub.C; ok {
		t.Fatal("C delivered after Close, want it closed")
	}
	// A closed subscription no longer counts as an attached client.
	if _, err := g.Request(context.Background(), twoQuestions()); !errors.Is(err, ErrNoClient) {
		t.Fatalf("Request after Close = %v, want ErrNoClient", err)
	}
}

func TestGateRequestIDsAreMonotonic(t *testing.T) {
	g := NewGate("sess-1")
	sub := g.Subscribe(8)
	defer sub.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first, req1 := openRequest(t, ctx, g, sub, twoQuestions())
	if err := g.Answer(req1.ID, nil); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	recvResult(t, first)
	recvUpdate(t, sub) // the resolve

	second, req2 := openRequest(t, ctx, g, sub, twoQuestions())
	if req1.ID != "dec-1" || req2.ID != "dec-2" {
		t.Errorf("ids = %q, %q, want dec-1, dec-2", req1.ID, req2.ID)
	}

	cancel()
	recvResult(t, second)
}

func TestGateBindStampsSessionID(t *testing.T) {
	// The create path builds the gate before the runner mints the session id.
	g := NewGate("")
	g.Bind("sess-42")
	sub := g.Subscribe(4)
	defer sub.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, req := openRequest(t, ctx, g, sub, twoQuestions())

	if req.SessionID != "sess-42" {
		t.Errorf("session id = %q, want sess-42", req.SessionID)
	}
}

func TestGateRequestRejectsEmptyQuestions(t *testing.T) {
	g := NewGate("sess-1")
	sub := g.Subscribe(4)
	defer sub.Close()

	if _, err := g.Request(context.Background(), nil); err == nil {
		t.Fatal("Request accepted an empty question set, want an error")
	}
	if open := g.Open(); len(open) != 0 {
		t.Fatalf("open = %v, want none", open)
	}
}

func TestAssignIDsIsIdempotent(t *testing.T) {
	once := AssignIDs(twoQuestions())
	twice := AssignIDs(once)

	if !reflect.DeepEqual(once, twice) {
		t.Errorf("re-stamping changed the ids:\n once = %+v\ntwice = %+v", once, twice)
	}
	if AssignIDs(nil) != nil {
		t.Error("AssignIDs(nil) = non-nil, want nil")
	}
}
