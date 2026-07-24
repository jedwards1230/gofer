package tui_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/tui/testkit"
)

// askUserQuestions is the raw tool input the model sends for a single-question
// ask_user call — the tool's snake_case schema (see docs/TUI.md § "The
// `ask_user` tool"). It is the exact payload that, rendered verbatim, produced
// the raw-JSON wall this fix removes from the transcript header.
const askUserQuestions = `{"questions":[{"title":"Choose a task",` +
	`"question":"What would you like me to work on next?",` +
	`"options":[` +
	`{"label":"Inspect the transcript renderer","rationale":"trace how items build"},` +
	`{"label":"Fix the spacer gap","rationale":"one blank row above the input"}],` +
	`"allow_free_text":true,"allow_chat":true}]}`

// askUserAnswer is the tool's own result text (internal/decision.formatAnswers)
// — the answer line the transcript keeps.
const askUserAnswer = `q1 "What would you like me to work on next?" → selected q1o1 "Inspect the transcript renderer"`

// askUserCall replays a settled ask_user tool call: the model asked one titled
// question and the user picked the first option.
func askUserCall() []event.Event {
	return []event.Event{
		event.NewTurnStarted(sid),
		event.NewToolCallStarted(sid, "call-1", "ask_user", json.RawMessage(askUserQuestions)),
		event.NewToolCallFinished(sid, "call-1", json.RawMessage(askUserQuestions), askUserAnswer, false, nil),
		event.NewTurnFinished(sid, "end_turn", provider.Usage{InputTokens: 5, OutputTokens: 2}),
	}
}

// TestGoldenToolCallAskUser locks the clean scrollback render of an ask_user
// tool call: a `● ask_user(Choose a task)` header (the question's title, not
// the raw questions JSON) over its answer line, exactly like any other tool
// block's concise-header-plus-result shape.
func TestGoldenToolCallAskUser(t *testing.T) {
	render(t, "tool_call_ask_user", askUserCall()...)
}

// TestAskUserCallNoRawJSON is the mutation guard for bug #2: the ask_user tool
// CALL must never render its raw questions JSON in the transcript. Before the
// fix summarizeToolInput fell through to the compact JSON for this
// non-command-shaped input, so the header was the whole
// `{"questions":[{"title":…,"options":[…]}]}` blob. Neutralize the ask_user
// branch in renderToolLines (let it fall back to summarizeToolInput) and the
// `{"questions"` substring returns, flipping this red.
func TestAskUserCallNoRawJSON(t *testing.T) {
	got := testkit.Render(ingest(askUserCall()...), testkit.Width, testkit.Height)

	if strings.Contains(got, `{"questions"`) || strings.Contains(got, `"options"`) {
		t.Errorf("ask_user tool call leaked its raw questions JSON into the transcript:\n%s", got)
	}
	if !strings.Contains(got, "● ask_user(Choose a task)") {
		t.Errorf("ask_user tool call missing its clean `● ask_user(Choose a task)` header:\n%s", got)
	}
	// The answer line survives — the whole point is a readable question + answer.
	// (Asserted on the un-wrapped head of the line: at the golden width the
	// result wraps across rows, so the full string is not contiguous.)
	if !strings.Contains(got, "→ selected q1o1") {
		t.Errorf("ask_user tool call dropped its answer line:\n%s", got)
	}
}

// TestAskUserCallBatchHeader covers the multi-question header: a batch renders
// `● ask_user(N questions)`, mirroring the decision widget's own "N questions"
// tab-strip label, rather than the first title alone or the raw JSON.
func TestAskUserCallBatchHeader(t *testing.T) {
	const batch = `{"questions":[` +
		`{"title":"Slice M4","question":"How should M4 be sliced?","options":[{"label":"Renderer first"}]},` +
		`{"title":"Scope v1","question":"What is in v1?","options":[{"label":"Views only"}]}]}`
	events := []event.Event{
		event.NewToolCallStarted(sid, "call-1", "ask_user", json.RawMessage(batch)),
		event.NewToolCallFinished(sid, "call-1", json.RawMessage(batch),
			"q1 \"How should M4 be sliced?\" → selected q1o1 \"Renderer first\"", false, nil),
	}
	got := testkit.Render(ingest(events...), testkit.Width, testkit.Height)
	if !strings.Contains(got, "● ask_user(2 questions)") {
		t.Errorf("multi-question ask_user missing `● ask_user(2 questions)` header:\n%s", got)
	}
	if strings.Contains(got, `{"questions"`) {
		t.Errorf("multi-question ask_user leaked raw JSON:\n%s", got)
	}
}
