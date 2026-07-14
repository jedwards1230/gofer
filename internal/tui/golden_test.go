package tui_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

const sid = "0192a1b2-c3d4-7e5f-8a90-000000000001"

// ingest builds a Model from a fixed theme and replays events onto it in
// order, the pattern every golden test below shares.
func ingest(events ...event.Event) tui.Model {
	m := tui.New(theme.Test())
	for _, e := range events {
		m = m.Ingest(e)
	}
	return m
}

func render(t *testing.T, name string, events ...event.Event) {
	t.Helper()
	got := testkit.Render(ingest(events...), testkit.Width, testkit.Height)
	testkit.AssertGolden(t, name, got)
}

// TestGoldenPlainTextTurn is the first golden test: a turn that streams
// assistant text and finishes with usage, no reasoning or tools involved.
func TestGoldenPlainTextTurn(t *testing.T) {
	render(t, "plain_text_turn",
		event.NewTurnStarted(sid),
		event.NewMessageStarted(sid, event.MessageText),
		event.NewMessageDelta(sid, event.MessageText, "Hello"),
		event.NewMessageDelta(sid, event.MessageText, "! How can "),
		event.NewMessageDelta(sid, event.MessageText, "I help you today?"),
		event.NewMessageFinished(sid, event.MessageText, "Hello! How can I help you today?"),
		event.NewTurnFinished(sid, "end_turn", provider.Usage{InputTokens: 9, OutputTokens: 7}),
	)
}

// TestGoldenUserAndAssistantTurn covers a full turn including the user's own
// prompt: runner.Prompt publishes it as a MessageStarted/MessageFinished
// {MessageUser} pair with no deltas (see event.MessageUser's doc), which
// Ingest renders as one "you › " prefixed transcript item ABOVE the agent's
// reply — mirroring how itemAssistantReasoning prefixes its own line with
// "» ".
func TestGoldenUserAndAssistantTurn(t *testing.T) {
	render(t, "user_and_assistant_turn",
		event.NewMessageStarted(sid, event.MessageUser),
		event.NewMessageFinished(sid, event.MessageUser, "Say hello."),
		event.NewTurnStarted(sid),
		event.NewMessageStarted(sid, event.MessageText),
		event.NewMessageDelta(sid, event.MessageText, "Hello"),
		event.NewMessageDelta(sid, event.MessageText, "! How can "),
		event.NewMessageDelta(sid, event.MessageText, "I help you today?"),
		event.NewMessageFinished(sid, event.MessageText, "Hello! How can I help you today?"),
		event.NewTurnFinished(sid, "end_turn", provider.Usage{InputTokens: 9, OutputTokens: 7}),
	)
}

// TestUserMessageRendersWithoutMessageStarted verifies Ingest is robust to a
// MessageFinished{MessageUser} with no preceding MessageStarted{MessageUser}
// — exactly the shape internal/daemonbridge/reconstruct.go's
// handleUserMessage synthesizes is a full Started+Finished pair, but Ingest
// itself never depends on having seen the Started half first: it is a pure
// no-op for MessageKind==MessageUser (see Ingest's MessageStarted case), so
// ordering (or a missing Started altogether) can never lose the item.
func TestUserMessageRendersWithoutMessageStarted(t *testing.T) {
	m := ingest(event.NewMessageFinished(sid, event.MessageUser, "no preceding Started"))
	got := testkit.Render(m, testkit.Width, testkit.Height)
	if !strings.Contains(got, "you › no preceding Started") {
		t.Errorf("rendered output = %q, want it to contain the user item", got)
	}
}

// TestGoldenReasoningAndText covers a turn that streams reasoning before
// its settled text, mirroring the SDK faux provider's canned turn.
func TestGoldenReasoningAndText(t *testing.T) {
	render(t, "reasoning_and_text",
		event.NewTurnStarted(sid),
		event.NewMessageStarted(sid, event.MessageReasoning),
		event.NewMessageDelta(sid, event.MessageReasoning, "The user said hello. "),
		event.NewMessageDelta(sid, event.MessageReasoning, "I'll greet them back."),
		event.NewMessageFinished(sid, event.MessageReasoning, "The user said hello. I'll greet them back."),
		event.NewMessageStarted(sid, event.MessageText),
		event.NewMessageDelta(sid, event.MessageText, "Hello"),
		event.NewMessageDelta(sid, event.MessageText, "! How can "),
		event.NewMessageDelta(sid, event.MessageText, "I help you today?"),
		event.NewMessageFinished(sid, event.MessageText, "Hello! How can I help you today?"),
		event.NewTurnFinished(sid, "end_turn", provider.Usage{InputTokens: 9, OutputTokens: 7}),
	)
}

// TestGoldenToolCall covers a tool call from start through its settled
// result, rendered as one compact block.
func TestGoldenToolCall(t *testing.T) {
	render(t, "tool_call",
		event.NewToolCallStarted(sid, "call-1", "bash", json.RawMessage(`{"cmd":"echo hi"}`)),
		event.NewToolCallFinished(sid, "call-1", "hi", false, nil),
	)
}

// TestGoldenToolCallRunning covers a tool call that has started but not
// finished, rendered as a header line only with the streaming glyph — no
// result line, since none has settled yet.
func TestGoldenToolCallRunning(t *testing.T) {
	render(t, "tool_call_running",
		event.NewToolCallStarted(sid, "call-1", "bash", json.RawMessage(`{"cmd":"echo hi"}`)),
	)
}

// TestGoldenToolCallMultiline covers a finished tool call whose result spans
// more lines than the collapsed tree block shows, exercising the "… +N
// lines" collapse line.
func TestGoldenToolCallMultiline(t *testing.T) {
	render(t, "tool_call_multiline",
		event.NewToolCallStarted(sid, "call-1", "bash", json.RawMessage(`{"cmd":"seq 1 6"}`)),
		event.NewToolCallFinished(sid, "call-1", "1\n2\n3\n4\n5\n6", false, nil),
	)
}

// TestGoldenMidStream captures a turn mid-flight: deltas have arrived but
// MessageFinished and TurnFinished haven't, so the item is still open and
// the status line still reads streaming.
func TestGoldenMidStream(t *testing.T) {
	render(t, "mid_stream",
		event.NewTurnStarted(sid),
		event.NewMessageStarted(sid, event.MessageText),
		event.NewMessageDelta(sid, event.MessageText, "Hello"),
		event.NewMessageDelta(sid, event.MessageText, ", wor"),
	)
}

// TestGoldenSessionError covers a fatal session error with no turn in
// flight.
func TestGoldenSessionError(t *testing.T) {
	render(t, "session_error", event.NewSessionError(sid, "boom", true))
}

// TestGoldenApproval covers a pending permission request: the transcript's
// permanent ✋ badge (itemApproval) plus the interactive inline prompt that
// commandeers the bottom input line while it's unresolved (see Model.pending,
// approval.go).
func TestGoldenApproval(t *testing.T) {
	render(t, "approval",
		event.NewPermissionRequested(sid, "perm-1", "bash", map[string]any{"cmd": "rm -rf /tmp/x"}, []string{"no rule"}),
	)
}

// TestGoldenApprovalPromptInline covers the same pending permission request
// as TestGoldenApproval, named explicitly for the inline prompt this PR adds
// — the ✋ bash badge in the transcript, and at the bottom the input-replacing
// prompt (tool·args, the question, the a/d/r action row, and a dim esc/session
// footer), replacing the old centered-overlay modal.
func TestGoldenApprovalPromptInline(t *testing.T) {
	render(t, "approval_prompt_inline",
		event.NewPermissionRequested(sid, "perm-1", "bash", map[string]any{"cmd": "rm -rf /tmp/x"}, []string{"no rule"}),
	)
}

// TestColorApprovalPromptInlineNarrow proves the inline prompt's lines clamp
// to a narrow width (24) instead of overrunning it — the #61 display-width
// lesson, checked here as a colored render since an Ascii golden alone can't
// catch an ANSI-width regression.
func TestColorApprovalPromptInlineNarrow(t *testing.T) {
	events := []event.Event{
		event.NewPermissionRequested(sid, "perm-1", "bash", map[string]any{"cmd": "rm -rf /tmp/x"}, []string{"no rule"}),
	}
	build := func(th theme.Theme) tui.Model {
		m := tui.New(th)
		for _, e := range events {
			m = m.Ingest(e)
		}
		return m
	}

	const width = 24
	plain := testkit.Render(build(theme.Test()), width, testkit.Height)
	colored := testkit.Render(build(colorTheme()), width, testkit.Height)
	assertColorLayout(t, plain, colored, width)
}

// TestGoldenFullTranscript covers the exit-flush render: every transcript item
// plus the final status line, unclipped by height and with no input line —
// what the attach TUI writes to the scrollback when it exits.
func TestGoldenFullTranscript(t *testing.T) {
	m := ingest(
		event.NewTurnStarted(sid),
		event.NewMessageStarted(sid, event.MessageReasoning),
		event.NewMessageFinished(sid, event.MessageReasoning, "Plan: greet, then run a check."),
		event.NewMessageStarted(sid, event.MessageText),
		event.NewMessageFinished(sid, event.MessageText, "Hello! Running a quick check."),
		event.NewToolCallStarted(sid, "call-1", "bash", json.RawMessage(`{"cmd":"echo hi"}`)),
		event.NewToolCallFinished(sid, "call-1", "hi", false, nil),
		event.NewTurnFinished(sid, "end_turn", provider.Usage{InputTokens: 12, OutputTokens: 9}),
	)
	testkit.AssertGolden(t, "full_transcript", m.FullTranscript(testkit.Width))
}

// TestFullTranscriptEmpty verifies an untouched transcript flushes nothing, so
// an immediately-interrupted run doesn't print a bare status line.
func TestFullTranscriptEmpty(t *testing.T) {
	if got := tui.New(theme.Test()).FullTranscript(testkit.Width); got != "" {
		t.Errorf("empty FullTranscript = %q; want empty string", got)
	}
}

// TestGoldenInputBuffer covers the input line with a typed buffer, driven
// through Model's pure edit methods rather than a bubbletea Program.
func TestGoldenInputBuffer(t *testing.T) {
	m := tui.New(theme.Test())
	for _, r := range "help me" {
		m = m.TypeRune(r)
	}
	got := testkit.Render(m, testkit.Width, testkit.Height)
	testkit.AssertGolden(t, "input_buffer", got)
}
