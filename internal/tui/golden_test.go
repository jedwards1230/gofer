package tui_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

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

// attributionSuffix is the exact substring the approval prompt's header
// carries when — and only when — the gated call is attributed to an agent.
// Its ABSENCE is the un-attributed contract (see
// TestGoldenApprovalUnattributed), so both tests below assert on this one
// string rather than two independently-drifting literals.
const attributionSuffix = " · from the "

// attributedCall builds the two events that attribute a gated call to an
// agent: a tool.call.started carrying ev.Agent, then the permission request
// for the SAME id (event.PermissionRequested.ID *is* the tool call id — see
// Model.toolAgents). agent == "" produces the un-attributed stream: the
// started event with no Agent at all, exactly what a single-agent session
// emits.
func attributedCall(agent string) []event.Event {
	started := event.NewToolCallStarted(sid, "call-1", "bash", json.RawMessage(`{"cmd":"go test -race ./..."}`))
	started.Agent = agent
	return []event.Event{
		started,
		event.NewPermissionRequested(sid, "call-1", "bash",
			map[string]any{"cmd": "go test -race ./...", "description": "Run the test suite with race detection", "timeout": 120},
			tui.GoldenTrace()),
	}
}

// TestGoldenApprovalAttributed covers the prompt's provenance header: a
// subagent's tool.call.started carried Agent="researcher", so the permission
// request correlated to that call renders "· from the `researcher` agent"
// after the title.
func TestGoldenApprovalAttributed(t *testing.T) {
	events := attributedCall("researcher")
	render(t, "approval_attributed", events...)

	got := testkit.Render(ingest(events...), testkit.Width, testkit.Height)
	if want := "bash command" + attributionSuffix + "`researcher` agent"; !strings.Contains(got, want) {
		t.Errorf("attributed approval header missing %q:\n%s", want, got)
	}
}

// TestGoldenApprovalUnattributed is the fallback contract: the same stream
// with NO agent on the tool call renders the bare title and no attribution
// clause at all — not a placeholder, not an empty pair of backticks. Asserted
// as the ABSENCE of the substring, because a golden alone can only prove "the
// bytes are these", never "this thing can't appear".
func TestGoldenApprovalUnattributed(t *testing.T) {
	events := attributedCall("")
	render(t, "approval_unattributed", events...)

	got := testkit.Render(ingest(events...), testkit.Width, testkit.Height)
	if !strings.Contains(got, "bash command") {
		t.Fatalf("un-attributed approval is missing the plain title:\n%s", got)
	}
	if strings.Contains(got, attributionSuffix) {
		t.Errorf("un-attributed approval rendered an attribution clause %q:\n%s", attributionSuffix, got)
	}
}

// multilineCommand is a shell command spanning several physical lines, one of
// them far wider than the 80-cell golden width — the shape a heredoc or a
// backslash-continued pipeline takes, and the case the pre-redesign one-line
// "cmd=…" summary silently truncated.
const multilineCommand = "go test -race ./... \\\n  -run 'TestApproval|TestGolden' \\\n  -count=1 -timeout 120s -v -args -update -someveryverylongflagvalue=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// TestGoldenApprovalMultilineCommand covers a command body with embedded
// newlines and an over-width physical line: every physical line becomes one
// or more rendered rows, and no row overruns the frame.
func TestGoldenApprovalMultilineCommand(t *testing.T) {
	events := []event.Event{
		event.NewPermissionRequested(sid, "perm-1", "bash", map[string]any{"cmd": multilineCommand}, tui.GoldenTrace()),
	}
	render(t, "approval_multiline_command", events...)

	got := testkit.Render(ingest(events...), testkit.Width, testkit.Height)
	for i, line := range strings.Split(got, "\n") {
		if w := ansi.StringWidth(line); w > testkit.Width {
			t.Errorf("line %d exceeds width %d cells (got %d): %q", i, testkit.Width, w, line)
		}
	}
	// The body must actually be spread over rows, not collapsed onto one:
	// every physical line's leading token appears on a row of its own.
	for _, token := range []string{"go test -race ./... \\", "-run 'TestApproval|TestGolden' \\", "-count=1"} {
		if !strings.Contains(got, token) {
			t.Errorf("multi-line body missing %q:\n%s", token, got)
		}
	}
}

// TestGoldenApprovalTruncatedBody covers the row budget
// (config.TUI.ApprovalBodyLineLimit, default 12): a command with more lines
// than the cap renders the first cap-1 of them plus a muted "… +N more
// lines", so the question and the action row can never be pushed off the
// frame by a pasted script.
func TestGoldenApprovalTruncatedBody(t *testing.T) {
	lines := make([]string, 0, 20)
	for i := range 20 {
		lines = append(lines, fmt.Sprintf("echo step-%02d", i))
	}
	events := []event.Event{
		event.NewPermissionRequested(sid, "perm-1", "bash",
			map[string]any{"cmd": strings.Join(lines, "\n")}, tui.GoldenTrace()),
	}
	render(t, "approval_truncated_body", events...)

	got := testkit.Render(ingest(events...), testkit.Width, testkit.Height)
	// 20 body rows capped at 12: 11 shown, 9 collapsed.
	if want := "… +9 more lines"; !strings.Contains(got, want) {
		t.Errorf("over-cap body missing the collapse row %q:\n%s", want, got)
	}
	if !strings.Contains(got, "echo step-10") {
		t.Errorf("over-cap body dropped the last shown row (step-10):\n%s", got)
	}
	if strings.Contains(got, "echo step-11") {
		t.Errorf("over-cap body rendered past the cap (step-11 present):\n%s", got)
	}
	// The decision row must survive the truncation — that is the whole point
	// of capping the body.
	if !strings.Contains(got, "Do you want to proceed?") {
		t.Errorf("over-cap body pushed the question off the frame:\n%s", got)
	}
}

// TestGoldenApprovalNoTrace covers a permission request whose guard reported
// no trace at all: the rationale falls back to saying so plainly instead of
// rendering an empty "Policy:" line or panicking on the missing entries.
func TestGoldenApprovalNoTrace(t *testing.T) {
	events := []event.Event{
		event.NewPermissionRequested(sid, "perm-1", "bash", map[string]any{"cmd": "rm -rf /tmp/x"}, nil),
	}
	render(t, "approval_no_trace", events...)

	got := testkit.Render(ingest(events...), testkit.Width, testkit.Height)
	if want := "gofer could not determine why this call was gated."; !strings.Contains(got, want) {
		t.Errorf("empty-trace approval missing the fallback reason %q:\n%s", want, got)
	}
	if strings.Contains(got, "Policy:") {
		t.Errorf("empty-trace approval rendered a Policy paragraph with nothing to report:\n%s", got)
	}
}

// TestApprovalPromptDegenerateWidths pins the width guards: the prompt's own
// wrap budget (width-2) goes non-positive below width 3, and every paragraph
// it wraps is longer than one cell, so an unguarded budget would either panic
// or spin. Rendering at 0 and 1 must simply produce clipped rows. Zero-height
// is exercised too — the first frame arrives before WindowSizeMsg does.
func TestApprovalPromptDegenerateWidths(t *testing.T) {
	m := ingest(event.NewPermissionRequested(sid, "perm-1", "bash", map[string]any{"cmd": "rm -rf /tmp/x"}, tui.GoldenTrace()))
	for _, size := range []struct{ w, h int }{{0, 0}, {1, 1}, {2, 3}, {3, 24}} {
		t.Run(fmt.Sprintf("%dx%d", size.w, size.h), func(t *testing.T) {
			got := testkit.Render(m, size.w, size.h)
			for i, line := range strings.Split(got, "\n") {
				if w := ansi.StringWidth(line); w > max(size.w, 1) {
					t.Errorf("line %d exceeds width %d cells (got %d): %q", i, size.w, w, line)
				}
			}
		})
	}
}

// TestApprovalPromptEmptySpecKeepsNoArgs pins the degenerate spec cases: a
// permission request with no spec at all (or one carrying only the
// description, which renders as the subtitle) still shows the "(no args)"
// placeholder where the body would be, rather than an empty gap.
func TestApprovalPromptEmptySpecKeepsNoArgs(t *testing.T) {
	for _, spec := range []map[string]any{nil, {}, {"description": "sweep the workspace"}} {
		m := ingest(event.NewPermissionRequested(sid, "perm-1", "bash", spec, tui.GoldenTrace()))
		got := testkit.Render(m, testkit.Width, testkit.Height)
		if !strings.Contains(got, "(no args)") {
			t.Errorf("approval with spec %v is missing the (no args) placeholder:\n%s", spec, got)
		}
	}
}

// TestGoldenApproval covers a pending permission request: the interactive
// inline prompt that commandeers the whole footer while it's unresolved (see
// Model.pending, approval.go) — the transcript's own itemApproval badge is
// suppressed while the prompt shows it (see transcriptLines).
func TestGoldenApproval(t *testing.T) {
	render(t, "approval",
		event.NewPermissionRequested(sid, "perm-1", "bash", map[string]any{"cmd": "rm -rf /tmp/x"}, tui.GoldenTrace()),
	)
}

// TestGoldenStyledApproval is TestGoldenApproval's styled-golden counterpart:
// locks the yellow "●" marker on the prompt's tool·args line and the muted
// footer — the pending state an Ascii golden can't distinguish from done or
// error.
func TestGoldenStyledApproval(t *testing.T) {
	renderStyled(t, "approval",
		event.NewPermissionRequested(sid, "perm-1", "bash", map[string]any{"cmd": "rm -rf /tmp/x"}, tui.GoldenTrace()),
	)
}

// TestGoldenApprovalPromptInline covers the same pending permission request
// as TestGoldenApproval, named explicitly for the inline prompt: the
// footer-commandeering block (tool·args, the question, the a/d/r action row,
// and a dim esc/session footer), replacing the old centered-overlay modal.
func TestGoldenApprovalPromptInline(t *testing.T) {
	render(t, "approval_prompt_inline",
		event.NewPermissionRequested(sid, "perm-1", "bash", map[string]any{"cmd": "rm -rf /tmp/x"}, tui.GoldenTrace()),
	)
}

// TestColorApprovalPromptInlineNarrow proves the inline prompt's lines clamp
// to a narrow width (24) instead of overrunning it — the #61 display-width
// lesson, checked here as a colored render since an Ascii golden alone can't
// catch an ANSI-width regression.
func TestColorApprovalPromptInlineNarrow(t *testing.T) {
	events := []event.Event{
		event.NewPermissionRequested(sid, "perm-1", "bash", map[string]any{"cmd": "rm -rf /tmp/x"}, tui.GoldenTrace()),
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

// longProse is a single ~130-char assistant sentence with no embedded
// newlines, every word distinct enough to track across wrapped rows, that
// clearly overflows the narrow render width the wrap test uses.
const longProse = "The quick brown fox jumps over the lazy sleeping dog while carefully reviewing every single distinct line of refactored authentication middleware code."

// TestGoldenWrapNarrowTranscript is the regression oracle for the transcript
// word-wrap fix (#—): a long assistant prose message rendered at a narrow
// width must WRAP across multiple rows, not clip at the right edge with a
// trailing "…". It captures a plain golden and additionally asserts, in code:
// every word of the message survives (nothing was truncated away), the body
// occupies multiple rows (it actually wrapped), no rendered line carries the
// "…" truncation ellipsis, and no line exceeds the render width.
func TestGoldenWrapNarrowTranscript(t *testing.T) {
	const width = 24
	m := ingest(
		event.NewMessageStarted(sid, event.MessageText),
		event.NewMessageFinished(sid, event.MessageText, longProse),
	)
	got := testkit.Render(m, width, testkit.Height)
	testkit.AssertGolden(t, "wrap_narrow_transcript", got)

	// Every word of the prose must appear somewhere — a truncated render
	// would drop the tail words entirely.
	for _, word := range strings.Fields(longProse) {
		if !strings.Contains(got, word) {
			t.Errorf("wrapped render dropped word %q; wrapping must preserve the whole message:\n%s", word, got)
		}
	}

	// The body must span multiple rows: count rendered lines that carry prose
	// words (excluding the footer rule and input line). One long sentence at
	// width 24 wraps to several rows — a single row would mean it was clipped.
	bodyRows := 0
	for _, line := range strings.Split(got, "\n") {
		if strings.ContainsAny(line, "abcdefghijklmnopqrstuvwxyz") && !strings.HasPrefix(line, "> ") {
			bodyRows++
		}
	}
	if bodyRows < 2 {
		t.Errorf("prose rendered on %d body row(s); want it wrapped across multiple rows", bodyRows)
	}

	// No line may carry the truncation ellipsis — the whole point of the fix —
	// and no line may exceed the render width.
	for i, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "…") {
			t.Errorf("line %d carries the truncation ellipsis; the body must wrap, not clip: %q", i, line)
		}
		if w := ansi.StringWidth(line); w > width {
			t.Errorf("line %d exceeds width %d cells (got %d): %q", i, width, w, line)
		}
	}
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
