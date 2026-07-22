package decision

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/tool"
)

// askInput is the raw model-supplied JSON for a one-question call with two
// options, used by the tests that care about what comes BACK rather than what
// went in.
const askInput = `{"questions":[{
	"title": "Migration strategy",
	"question": "Which approach should I take?",
	"options": [
		{"label":"In-place ALTER","rationale":"fastest, but locks the table"},
		{"label":"Shadow table + backfill","rationale":"online, but doubles disk","recommended":true}
	]
}]}`

// runAsk drives one ask_user call to completion: it starts the tool, waits for
// the request to reach the subscriber, hands it to answer for resolution, and
// returns the tool's result. answer receives the observed request so a test can
// key its answers off the ids the gate assigned.
func runAsk(t *testing.T, input string, answer func(g *Gate, req Request)) (tool.Result, error) {
	t.Helper()
	g := NewGate("sess-1")
	sub := g.Subscribe(4)
	defer sub.Close()

	type outcome struct {
		res tool.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := NewAskUser(g).Run(context.Background(), json.RawMessage(input))
		done <- outcome{res, err}
	}()

	up := recvUpdate(t, sub)
	if up.Kind != UpdateRequested {
		t.Fatalf("first update = %v, want requested", up.Kind)
	}
	answer(g, up.Request)

	select {
	case o := <-done:
		return o.res, o.err
	case <-time.After(2 * time.Second):
		// A real deadline, like recvUpdate/recvResult use: t.Context() is only
		// cancelled at cleanup, so a wedged ask_user would hang here until the
		// whole package timed out instead of failing this test fast.
		t.Fatal("ask_user did not return")
		return tool.Result{}, nil
	}
}

func TestAskUserSpec(t *testing.T) {
	spec := (&AskUser{}).Spec()

	if spec.Type != "object" {
		t.Errorf("spec type = %q, want object", spec.Type)
	}
	if len(spec.Required) != 1 || spec.Required[0] != "questions" {
		t.Errorf("required = %v, want [questions]", spec.Required)
	}
	questions, ok := spec.Properties["questions"]
	if !ok {
		t.Fatal("spec has no questions property")
	}
	if questions.Type != "array" || questions.Items == nil {
		t.Fatalf("questions = %+v, want an array with items", questions)
	}
	question := *questions.Items
	for _, field := range []string{"title", "question", "context", "options", "allow_free_text", "allow_chat"} {
		if _, ok := question.Properties[field]; !ok {
			t.Errorf("question schema missing %q", field)
		}
	}
	// The escape hatches advertise their true-by-default to the model, so it
	// knows omitting them is not the same as switching them off.
	for _, field := range []string{"allow_free_text", "allow_chat"} {
		if got := question.Properties[field].Default; got != true {
			t.Errorf("%s default = %v, want true", field, got)
		}
	}
	options := question.Properties["options"]
	if options.Items == nil {
		t.Fatal("options schema has no items")
	}
	for _, field := range []string{"label", "rationale", "reference", "recommended"} {
		if _, ok := options.Items.Properties[field]; !ok {
			t.Errorf("option schema missing %q", field)
		}
	}
	// The whole spec must survive the marshalling loop.FromRegistry does when
	// it builds the provider.ToolSpec.
	if _, err := json.Marshal(spec); err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	if name := (&AskUser{}).Name(); name != "ask_user" {
		t.Errorf("name = %q, want ask_user", name)
	}
}

func TestAskUserEscapeHatchDefaults(t *testing.T) {
	tests := []struct {
		name              string
		question          string
		wantFree, wantCha bool
	}{
		{
			name:     "omitted defaults to true",
			question: `{"question":"Ship it?","options":[{"label":"yes"}]}`,
			wantFree: true, wantCha: true,
		},
		{
			name:     "explicit false is honored",
			question: `{"question":"Ship it?","options":[{"label":"yes"}],"allow_free_text":false,"allow_chat":false}`,
			wantFree: false, wantCha: false,
		},
		{
			name:     "explicit true is honored",
			question: `{"question":"Ship it?","options":[{"label":"yes"}],"allow_free_text":true,"allow_chat":true}`,
			wantFree: true, wantCha: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got acp.DecisionQuestion
			_, err := runAsk(t, `{"questions":[`+tc.question+`]}`, func(g *Gate, req Request) {
				got = req.Questions[0]
				if err := g.Answer(req.ID, nil); err != nil {
					t.Errorf("Answer: %v", err)
				}
			})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got.AllowFreeText != tc.wantFree {
				t.Errorf("AllowFreeText = %v, want %v", got.AllowFreeText, tc.wantFree)
			}
			if got.AllowChat != tc.wantCha {
				t.Errorf("AllowChat = %v, want %v", got.AllowChat, tc.wantCha)
			}
		})
	}
}

func TestAskUserMalformedCalls(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "no questions",
			input: `{"questions":[]}`,
			want:  "must not be empty",
		},
		{
			name:  "missing questions field",
			input: `{}`,
			want:  "must not be empty",
		},
		{
			name:  "blank question text",
			input: `{"questions":[{"title":"t","question":"   ","options":[{"label":"a"}]}]}`,
			want:  "no question text",
		},
		{
			name:  "no options and no free text",
			input: `{"questions":[{"question":"Ship it?","allow_free_text":false}]}`,
			want:  "no options and no free text",
		},
	}

	// A malformed call is the model's to correct, so it comes back as an
	// IsError Result (never a Go error, which would abort the turn) naming the
	// fix. The gate has a subscriber, so nothing here is ErrNoClient in
	// disguise.
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewGate("sess-1")
			sub := g.Subscribe(4)
			defer sub.Close()

			res, err := NewAskUser(g).Run(context.Background(), json.RawMessage(tc.input))

			if err != nil {
				t.Fatalf("Run err = %v, want a Result", err)
			}
			if !res.IsError {
				t.Errorf("IsError = false, want true (content %q)", res.Content)
			}
			if !strings.Contains(res.Content, tc.want) {
				t.Errorf("content = %q, want it to mention %q", res.Content, tc.want)
			}
			if open := g.Open(); len(open) != 0 {
				t.Errorf("open = %v, want a malformed call to open nothing", open)
			}
		})
	}
}

func TestAskUserUndecodableInputIsAnError(t *testing.T) {
	g := NewGate("sess-1")
	sub := g.Subscribe(4)
	defer sub.Close()

	// Input that is not the tool's object at all cannot be corrected from a
	// Result, so it is a Go error per tool.Tool's contract.
	if _, err := NewAskUser(g).Run(context.Background(), json.RawMessage(`["nope"]`)); err == nil {
		t.Fatal("Run accepted undecodable input, want an error")
	}
}

func TestAskUserNoClient(t *testing.T) {
	g := NewGate("sess-1") // nothing subscribed

	res, err := NewAskUser(g).Run(context.Background(), json.RawMessage(askInput))

	if err != nil {
		t.Fatalf("Run err = %v, want a Result", err)
	}
	if !res.IsError {
		t.Error("IsError = false, want true")
	}
	if res.Content != noClientContent {
		t.Errorf("content = %q, want %q", res.Content, noClientContent)
	}
}

func TestAskUserContextCancelIsAnError(t *testing.T) {
	g := NewGate("sess-1")
	sub := g.Subscribe(4)
	defer sub.Close()
	ctx, cancel := context.WithCancel(context.Background())

	type outcome struct {
		res tool.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := NewAskUser(g).Run(ctx, json.RawMessage(askInput))
		done <- outcome{res, err}
	}()
	recvUpdate(t, sub) // the request is open and blocked
	cancel()

	o := <-done
	// An interrupted turn must abort, not feed the model a "you were
	// interrupted" tool result.
	if !errors.Is(o.err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", o.err)
	}
	if o.res.Content != "" {
		t.Errorf("content = %q, want empty", o.res.Content)
	}
}

func TestAskUserResultText(t *testing.T) {
	const fourQuestions = `{"questions":[
		{"question":"Which approach should I take?","options":[{"label":"In-place ALTER"},{"label":"Shadow table + backfill"}]},
		{"question":"Which database?","options":[{"label":"mysql"}]},
		{"question":"Ship now?","options":[{"label":"yes"}]},
		{"question":"Retention?","options":[{"label":"30 days"}]}
	]}`

	res, err := runAsk(t, fourQuestions, func(g *Gate, req Request) {
		if err := g.Answer(req.ID, []acp.DecisionAnswer{
			{
				QuestionID: "q1",
				Outcome:    acp.DecisionOutcomeSelected{OptionID: "q1o2"},
				Notes:      "must stay online during business hours",
			},
			{QuestionID: "q2", Outcome: acp.DecisionOutcomeText{Text: "postgres, we already run it"}},
			{QuestionID: "q3", Outcome: acp.DecisionOutcomeChat{}},
			// q4 deliberately unanswered — it must come back cancelled.
		}); err != nil {
			t.Errorf("Answer: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.IsError {
		// chat and cancelled are legitimate outcomes, not failures.
		t.Errorf("IsError = true, want false (content %q)", res.Content)
	}

	want := strings.Join([]string{
		`q1 "Which approach should I take?" → selected q1o2 "Shadow table + backfill"`,
		`    notes: must stay online during business hours`,
		`q2 "Which database?" → text: postgres, we already run it`,
		`q3 "Ship now?" → chat (the user wants to discuss this instead of choosing)`,
		`q4 "Retention?" → cancelled (unanswered)`,
	}, "\n")
	if res.Content != want {
		t.Errorf("content =\n%s\n\nwant\n%s", res.Content, want)
	}
}

func TestAskUserMetadataAnswersRoundTrip(t *testing.T) {
	res, err := runAsk(t, askInput, func(g *Gate, req Request) {
		if err := g.Answer(req.ID, []acp.DecisionAnswer{
			{QuestionID: "q1", Outcome: acp.DecisionOutcomeSelected{OptionID: "q1o1"}, Notes: "locking is fine here"},
		}); err != nil {
			t.Errorf("Answer: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	raw, ok := res.Metadata.Extra["answers"].(json.RawMessage)
	if !ok {
		t.Fatalf("Extra[answers] = %T, want json.RawMessage", res.Metadata.Extra["answers"])
	}
	// The payload is the ACP session/request_decision response shape, so a
	// client (and the daemon relay in the follow-up PR) decodes it with the
	// acp unmarshaller that resolves the concrete outcome variant.
	var resp acp.RequestDecisionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal answers: %v", err)
	}
	if len(resp.Answers) != 1 {
		t.Fatalf("answers = %d, want 1", len(resp.Answers))
	}
	got := resp.Answers[0]
	if got.QuestionID != "q1" || got.Notes != "locking is fine here" {
		t.Errorf("answer = %+v, want q1 with its notes", got)
	}
	sel, ok := got.Outcome.(acp.DecisionOutcomeSelected)
	if !ok || sel.OptionID != "q1o1" {
		t.Errorf("outcome = %#v, want selected q1o1", got.Outcome)
	}
}
