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

// ingestColor is ingest's styled-golden counterpart: it builds the Model
// through testkit.ColorTheme so the marker vocabulary's state colors actually
// render, for renderStyled below to capture as a *.styled.golden.
func ingestColor(events ...event.Event) tui.Model {
	m := tui.New(testkit.ColorTheme())
	for _, e := range events {
		m = m.Ingest(e)
	}
	return m
}

func renderStyled(t *testing.T, name string, events ...event.Event) {
	t.Helper()
	got := testkit.Render(ingestColor(events...), testkit.Width, testkit.Height)
	testkit.AssertGoldenStyled(t, name, got)
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
// Ingest renders as one "○ "-marked transcript item ABOVE the agent's "● "
// reply — the only hollow marker in the vocabulary (see theme.GlyphHuman).
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

// TestGoldenStyledUserAndAssistantTurn is TestGoldenUserAndAssistantTurn's
// styled-golden counterpart: the same finished turn, rendered through a real
// color profile, locks the ink "○" human marker and the green "●" agent
// marker + status — the ok/finished state an Ascii golden can't distinguish
// from a streaming one.
func TestGoldenStyledUserAndAssistantTurn(t *testing.T) {
	renderStyled(t, "user_and_assistant_turn",
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
	if !strings.Contains(got, "○ no preceding Started") {
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

// TestEmptyReasoningRendersNoMarker covers a provider (Claude does this)
// emitting a reasoning block that settles with no content at all — Ingest
// still records the item, but renderItemLines must suppress it rather than
// show a bare "●" marker with nothing after it. The following turn's
// non-empty text still renders normally, proving the guard is scoped to
// empty content and doesn't swallow real turns.
func TestEmptyReasoningRendersNoMarker(t *testing.T) {
	m := ingest(
		event.NewTurnStarted(sid),
		event.NewMessageStarted(sid, event.MessageReasoning),
		event.NewMessageFinished(sid, event.MessageReasoning, ""),
	)
	got := testkit.Render(m, testkit.Width, testkit.Height)
	if strings.Contains(got, "●") {
		t.Errorf("empty reasoning item rendered a marker glyph; want no visible line:\n%s", got)
	}

	m = m.Ingest(event.NewMessageStarted(sid, event.MessageText))
	m = m.Ingest(event.NewMessageFinished(sid, event.MessageText, "Hello! How can I help you today?"))
	got = testkit.Render(m, testkit.Width, testkit.Height)
	if !strings.Contains(got, "● Hello! How can I help you today?") {
		t.Errorf("non-empty text after the empty reasoning item didn't render:\n%s", got)
	}
}

// TestEmptyAssistantTextRendersNoMarker is TestEmptyReasoningRendersNoMarker's
// itemAssistantText counterpart — an assistant-text item that settles with no
// content renders no marker line either.
func TestEmptyAssistantTextRendersNoMarker(t *testing.T) {
	m := ingest(
		event.NewTurnStarted(sid),
		event.NewMessageStarted(sid, event.MessageText),
		event.NewMessageFinished(sid, event.MessageText, ""),
	)
	got := testkit.Render(m, testkit.Width, testkit.Height)
	if strings.Contains(got, "●") {
		t.Errorf("empty assistant-text item rendered a marker glyph; want no visible line:\n%s", got)
	}
}

// TestGoldenToolCall covers a tool call from start through its settled
// result, rendered as one compact block.
func TestGoldenToolCall(t *testing.T) {
	render(t, "tool_call",
		event.NewToolCallStarted(sid, "call-1", "bash", json.RawMessage(`{"cmd":"echo hi"}`)),
		event.NewToolCallFinished(sid, "call-1", json.RawMessage(`{"cmd":"echo hi"}`), "hi", false, nil),
	)
}

// TestGoldenToolCallRunning covers a tool call that has started but not
// finished, rendered as a header line only with the streaming glyph — no
// result line, since none has settled yet. The started input is an empty
// seed ("{}", the shape a provider streams before the real arguments land),
// exercising the name-only header — a real command header only appears once
// ToolCallFinished's authoritative Input arrives.
func TestGoldenToolCallRunning(t *testing.T) {
	render(t, "tool_call_running",
		event.NewToolCallStarted(sid, "call-1", "bash", json.RawMessage(`{}`)),
	)
}

// TestGoldenToolCallMultiline covers a finished tool call whose result spans
// more lines than the collapsed tree block shows, exercising the "… +N
// lines" collapse line.
func TestGoldenToolCallMultiline(t *testing.T) {
	render(t, "tool_call_multiline",
		event.NewToolCallStarted(sid, "call-1", "bash", json.RawMessage(`{"cmd":"seq 1 6"}`)),
		event.NewToolCallFinished(sid, "call-1", json.RawMessage(`{"cmd":"seq 1 6"}`), "1\n2\n3\n4\n5\n6", false, nil),
	)
}

// TestGoldenToolCallError covers a finished tool call that reported an error
// (ToolCallFinished.IsError): the header still shows the real command, marked
// with the same "●" every other item uses. This Ascii golden locks the
// structure (marker + command header + result body); the color styling that
// sets an error apart — the red marker and dimmed body — can't show under
// termenv.Ascii and is asserted separately by the styled golden
// tool_call_error.styled.golden (see golden_test.go's TestGoldenStyledToolCallError).
func TestGoldenToolCallError(t *testing.T) {
	render(t, "tool_call_error",
		event.NewToolCallStarted(sid, "call-1", "bash", json.RawMessage(`{}`)),
		event.NewToolCallFinished(sid, "call-1", json.RawMessage(`{"command":"go test ./..."}`), "FAIL  session  0.1s", true, nil),
	)
}

// TestGoldenStyledToolCallError is TestGoldenToolCallError's styled-golden
// counterpart: proves the failed marker is actually red (DangerStyle), not
// merely structurally distinct — the Ascii golden above can't see color, and
// this replaces the old TestColorToolCallErrorStyling assertion-based test
// now that the styled golden is the state oracle.
func TestGoldenStyledToolCallError(t *testing.T) {
	renderStyled(t, "tool_call_error",
		event.NewToolCallStarted(sid, "call-1", "bash", json.RawMessage(`{}`)),
		event.NewToolCallFinished(sid, "call-1", json.RawMessage(`{"command":"go test ./..."}`), "FAIL  session  0.1s", true, nil),
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

// TestGoldenStyledMidStream is TestGoldenMidStream's styled-golden
// counterpart: locks the yellow "●" agent marker and yellow "streaming"
// status a mid-flight turn renders in — the in-progress state an Ascii
// golden can't distinguish from done or error.
func TestGoldenStyledMidStream(t *testing.T) {
	renderStyled(t, "mid_stream",
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

// TestGoldenApproval covers a pending permission request: the interactive
// inline prompt that commandeers the whole footer while it's unresolved (see
// Model.pending, approval.go) — the transcript's own itemApproval badge is
// suppressed while the prompt shows it (see transcriptLines).
func TestGoldenApproval(t *testing.T) {
	render(t, "approval",
		event.NewPermissionRequested(sid, "perm-1", "bash", map[string]any{"cmd": "rm -rf /tmp/x"}, []string{"no rule"}),
	)
}

// TestGoldenStyledApproval is TestGoldenApproval's styled-golden counterpart:
// locks the yellow "●" marker on the prompt's tool·args line and the muted
// footer — the pending state an Ascii golden can't distinguish from done or
// error.
func TestGoldenStyledApproval(t *testing.T) {
	renderStyled(t, "approval",
		event.NewPermissionRequested(sid, "perm-1", "bash", map[string]any{"cmd": "rm -rf /tmp/x"}, []string{"no rule"}),
	)
}

// TestGoldenApprovalPromptInline covers the same pending permission request
// as TestGoldenApproval, named explicitly for the inline prompt: the
// footer-commandeering block (tool·args, the question, the a/d/r action row,
// and a dim esc/session footer), replacing the old centered-overlay modal.
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
	colored := testkit.Render(build(testkit.ColorTheme()), width, testkit.Height)
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
		event.NewToolCallFinished(sid, "call-1", json.RawMessage(`{"cmd":"echo hi"}`), "hi", false, nil),
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

// TestGoldenInputBufferCursorMidText covers the cursor-aware buffer
// (inputbuf.go) rendering the "▏" glyph at its actual mid-text position
// after moving left, not always appended at the end the way the pre-cursor
// append-only buffer rendered it.
func TestGoldenInputBufferCursorMidText(t *testing.T) {
	m := tui.New(theme.Test())
	for _, r := range "help me" {
		m = m.TypeRune(r)
	}
	for i := 0; i < 3; i++ {
		m = m.MoveLeft()
	}
	got := testkit.Render(m, testkit.Width, testkit.Height)
	testkit.AssertGolden(t, "input_buffer_cursor_mid_text", got)
}
