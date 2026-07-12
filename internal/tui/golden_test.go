package tui_test

import (
	"encoding/json"
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

// TestGoldenApproval covers a pending permission request, rendered as a
// display-only line (no interactive approval pipeline in M1).
func TestGoldenApproval(t *testing.T) {
	render(t, "approval",
		event.NewPermissionRequested(sid, "perm-1", "bash", map[string]any{"cmd": "rm -rf /tmp/x"}, []string{"no rule"}),
	)
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
