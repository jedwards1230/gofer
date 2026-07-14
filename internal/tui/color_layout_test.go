package tui_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// colorTheme is theme.Test but with a real color profile, so lipgloss emits
// ANSI. The layout must be IDENTICAL to the colorless render — color changes
// styling, never geometry. Under the pre-fix rune-counting bug it did not,
// which is exactly what these tests lock down.
func colorTheme() theme.Theme {
	th := theme.Test()
	th.Profile = termenv.TrueColor
	return th
}

// assertColorLayout asserts the two invariants that must hold for any
// component rendered once plain (theme.Test()) and once colored
// (colorTheme()) at the same width:
//
//   - stripping ANSI from the colored render reproduces the plain render
//     byte-for-byte (color must never change geometry), and
//   - no line in the colored render exceeds width display cells (an
//     overflowing line is what wraps/tears the live terminal).
func assertColorLayout(t *testing.T, plain, colored string, width int) {
	t.Helper()

	if stripped := ansi.Strip(colored); stripped != plain {
		t.Errorf("colored render stripped of ANSI != plain render (color changed layout)\n--- stripped ---\n%s\n--- plain ---\n%s", stripped, plain)
	}

	for i, line := range strings.Split(colored, "\n") {
		if w := ansi.StringWidth(line); w > width {
			t.Errorf("line %d exceeds width %d cells (got %d): %q", i, width, w, line)
		}
	}
}

// colorOverviewFixture is a small roster including a pending session that
// renders the width-2 ✋ approval glyph — the direct reproduction case for
// the roster-alignment defect.
func colorOverviewFixture() []tui.SessionInfo {
	return []tui.SessionInfo{
		{
			ID:      "sess-1",
			Title:   "wire the websocket ACP listener",
			Summary: "blocked: approve Bash(kubectl delete pod)",
			Status:  tui.StatusWorking,
			Cost:    provider.Cost{USD: 0.0912},
			Pending: 2,
			Updated: overviewNow.Add(-30 * time.Second),
		},
		{
			ID:      "sess-2",
			Title:   "explore three agent ecosystems",
			Summary: "M2 launched; awaiting sketch review + 4 decisions",
			Status:  tui.StatusWorking,
			Cost:    provider.Cost{USD: 0.3821},
			Updated: overviewNow.Add(-2 * time.Minute),
		},
		{
			ID:      "sess-3",
			Title:   "keycloak path-b groundwork",
			Summary: "turn finished — awaiting the next prompt",
			Status:  tui.StatusNeedsInput,
			Cost:    provider.Cost{USD: 0.1204},
			Updated: overviewNow.Add(-5 * time.Minute),
		},
	}
}

// colorOverview builds an Overview over colorOverviewFixture rendering
// through th.
func colorOverview(th theme.Theme) tui.Overview {
	return tui.NewOverview(th, tui.OverviewMeta{
		App:     "gofer",
		Version: "0.2.0",
		Model:   "fable-5",
		Cwd:     "~/orchestration",
		Now:     overviewNow,
	}).WithSessions(colorOverviewFixture())
}

// TestColorOverviewApprovalLayout renders a roster containing a pending
// (✋2) session at two widths, plain and colored, and asserts the colored
// render's geometry matches the plain one exactly.
func TestColorOverviewApprovalLayout(t *testing.T) {
	for _, width := range []int{80, 120} {
		t.Run(fmt.Sprintf("width=%d", width), func(t *testing.T) {
			plain := testkit.Render(colorOverview(theme.Test()), width, testkit.Height)
			colored := testkit.Render(colorOverview(colorTheme()), width, testkit.Height)
			assertColorLayout(t, plain, colored, width)
		})
	}
}

// colorPeek builds a Peek over colorOverviewFixture with a non-empty reply
// buffer — exercising the ❯ input's width — rendering through th.
func colorPeek(th theme.Theme) tui.Peek {
	return tui.NewPeek(th, colorOverview(th), "status?")
}

// TestColorPeekCardLayout renders the peek summary card plain and colored at
// two widths and asserts the shared layout invariants hold. The card has no
// divider/split geometry to plumb any more — this is the peek half of the
// #61 display-width lesson now that the card is a single-column layout.
func TestColorPeekCardLayout(t *testing.T) {
	for _, width := range []int{80, 120} {
		t.Run(fmt.Sprintf("width=%d", width), func(t *testing.T) {
			plain := testkit.Render(colorPeek(theme.Test()), width, testkit.Height)
			colored := testkit.Render(colorPeek(colorTheme()), width, testkit.Height)
			assertColorLayout(t, plain, colored, width)
		})
	}
}

// longTranscriptEvents is a long, multi-item transcript — user prompt,
// reasoning, text, two tool calls (one erroring), more text, a finished
// turn — ending in a pending PermissionRequested, exercising truncation over
// a realistically busy attach screen.
func longTranscriptEvents() []event.Event {
	return []event.Event{
		event.NewMessageStarted(sid, event.MessageUser),
		event.NewMessageFinished(sid, event.MessageUser, "Please refactor the auth middleware and run the full test suite, reporting any flakes."),
		event.NewTurnStarted(sid),
		event.NewMessageStarted(sid, event.MessageReasoning),
		event.NewMessageFinished(sid, event.MessageReasoning, "I'll start by reading the middleware package, then touch the token refresh path."),
		event.NewMessageStarted(sid, event.MessageText),
		event.NewMessageFinished(sid, event.MessageText, "Reading internal/authmw now."),
		event.NewToolCallStarted(sid, "call-1", "read_file", json.RawMessage(`{"path":"internal/authmw/middleware.go"}`)),
		event.NewToolCallFinished(sid, "call-1", json.RawMessage(`{"path":"internal/authmw/middleware.go"}`), "package authmw\n\nfunc Handler() {}\n", false, nil),
		event.NewMessageStarted(sid, event.MessageText),
		event.NewMessageFinished(sid, event.MessageText, "Middleware looks fine; running the test suite next."),
		event.NewToolCallStarted(sid, "call-2", "bash", json.RawMessage(`{"cmd":"go test ./..."}`)),
		event.NewToolCallFinished(sid, "call-2", json.RawMessage(`{"cmd":"go test ./..."}`), "ok  authmw  1.2s\nok  handlers 0.8s\nFAIL session 0.1s", true, nil),
		event.NewMessageStarted(sid, event.MessageText),
		event.NewMessageFinished(sid, event.MessageText, "One package failed; I need to delete the stale session fixture before re-running."),
		event.NewTurnFinished(sid, "end_turn", provider.Usage{InputTokens: 120, OutputTokens: 64}),
		event.NewPermissionRequested(sid, "perm-1", "bash", map[string]any{"cmd": "rm -rf /tmp/session-fixtures"}, []string{"no rule"}),
	}
}

// colorTranscript builds a Model over longTranscriptEvents rendering through
// th.
func colorTranscript(th theme.Theme) tui.Model {
	m := tui.New(th)
	for _, e := range longTranscriptEvents() {
		m = m.Ingest(e)
	}
	return m
}

// TestColorAttachApprovalOverLongTranscript renders a long transcript ending
// in a pending permission request at a normal width (80) and a narrow one
// (24), plain and colored, and asserts the shared layout invariants. The
// narrow case proves lines clamp to width instead of wrap-exploding past it.
func TestColorAttachApprovalOverLongTranscript(t *testing.T) {
	for _, width := range []int{80, 24} {
		t.Run(fmt.Sprintf("width=%d", width), func(t *testing.T) {
			plain := testkit.Render(colorTranscript(theme.Test()), width, testkit.Height)
			colored := testkit.Render(colorTranscript(colorTheme()), width, testkit.Height)
			assertColorLayout(t, plain, colored, width)
		})
	}
}

// toolCallModel replays a started+finished bash call (empty seed, then the
// authoritative command on finish) with the given result and error flag,
// rendering through th. The first two rendered lines are the tool block's
// header and its single result line.
func toolCallModel(th theme.Theme, id, result string, isError bool) tui.Model {
	m := tui.New(th)
	for _, e := range []event.Event{
		event.NewToolCallStarted(sid, id, "bash", json.RawMessage(`{}`)),
		event.NewToolCallFinished(sid, id, json.RawMessage(`{"command":"go test ./..."}`), result, isError, nil),
	} {
		m = m.Ingest(e)
	}
	return m
}

// TestColorToolCallErrorStyling proves the softened error styling is actually
// applied, not merely structurally present (the Ascii golden can't see color):
// a failed tool call's header is rendered in the warn accent — deliberately
// softer than the DangerStyle red a fatal SessionError uses — and its result
// body is dimmed, while a clean call's header and body carry no styling at all.
func TestColorToolCallErrorStyling(t *testing.T) {
	th := colorTheme()

	failed := strings.Split(testkit.Render(toolCallModel(th, "call-1", "FAIL session 0.1s", true), testkit.Width, testkit.Height), "\n")
	clean := strings.Split(testkit.Render(toolCallModel(th, "call-2", "ok session 0.1s", false), testkit.Width, testkit.Height), "\n")
	failedHeader, failedBody := failed[0], failed[1]
	cleanHeader, cleanBody := clean[0], clean[1]

	// Same plain geometry — styling changes color, not the text or the glyph
	// positions; only the ok/error glyph differs.
	if got, want := ansi.Strip(failedHeader), "✗ bash(go test ./...)"; got != want {
		t.Fatalf("failed header (stripped) = %q, want %q", got, want)
	}
	if got, want := ansi.Strip(cleanHeader), "✓ bash(go test ./...)"; got != want {
		t.Fatalf("clean header (stripped) = %q, want %q", got, want)
	}

	// The failed header is exactly its plain text run through WarnStyle, and is
	// NOT the DangerStyle red — the whole point of the softer accent.
	if want := th.WarnStyle().Render(ansi.Strip(failedHeader)); failedHeader != want {
		t.Errorf("failed header not warn-styled:\n got %q\nwant %q", failedHeader, want)
	}
	if danger := th.DangerStyle().Render(ansi.Strip(failedHeader)); failedHeader == danger {
		t.Error("failed header is danger-styled; item wants the softer warn accent, not the SessionError red")
	}

	// The failed result body is dimmed (MutedStyle); the clean call carries no
	// styling on either line.
	if want := th.MutedStyle().Render(ansi.Strip(failedBody)); failedBody != want {
		t.Errorf("failed body not muted:\n got %q\nwant %q", failedBody, want)
	}
	if cleanHeader != ansi.Strip(cleanHeader) {
		t.Errorf("clean header should be unstyled, got %q", cleanHeader)
	}
	if cleanBody != ansi.Strip(cleanBody) {
		t.Errorf("clean body should be unstyled, got %q", cleanBody)
	}
}
