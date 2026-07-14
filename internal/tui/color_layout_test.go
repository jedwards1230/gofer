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

// colorPeekTail builds a small, populated read-only transcript rendering
// through th — a themed twin of peek_test.go's peekTail.
func colorPeekTail(th theme.Theme) tui.Model {
	m := tui.New(th)
	for _, e := range []event.Event{
		event.NewTurnStarted(sid),
		event.NewMessageStarted(sid, event.MessageReasoning),
		event.NewMessageFinished(sid, event.MessageReasoning, "Checking the ACP handshake path."),
		event.NewToolCallStarted(sid, "call-1", "bash", json.RawMessage(`{"cmd":"go test ./acp"}`)),
		event.NewToolCallFinished(sid, "call-1", "ok  acp  0.4s", false, nil),
		event.NewMessageStarted(sid, event.MessageText),
		event.NewMessageFinished(sid, event.MessageText, "Tests pass. The listener is wired."),
		event.NewTurnFinished(sid, "end_turn", provider.Usage{InputTokens: 40, OutputTokens: 18}),
	} {
		m = m.Ingest(e)
	}
	return m
}

// colorPeek builds a Peek over colorOverviewFixture and colorPeekTail,
// rendering through th.
func colorPeek(th theme.Theme) tui.Peek {
	return tui.NewPeek(th, colorOverview(th), colorPeekTail(th))
}

// TestColorPeekHorizontalDividerPlumb renders the peek screen's side-by-side
// split (width 120, at layout.PeekHorizontalMinWidth) plain and colored, and
// asserts — beyond the shared layout invariants — that the " │ " column
// divider sits at the same display column on every row. This is the direct
// reproduction of the "scattered │" defect: under rune-counting, a styled
// left pane measured wider than its display width, so JoinColumns padded it
// inconsistently row to row.
func TestColorPeekHorizontalDividerPlumb(t *testing.T) {
	const width = 120

	plain := testkit.Render(colorPeek(theme.Test()), width, testkit.Height)
	colored := testkit.Render(colorPeek(colorTheme()), width, testkit.Height)
	assertColorLayout(t, plain, colored, width)

	dividerCol := -1
	found := false
	for i, line := range strings.Split(colored, "\n") {
		idx := strings.Index(line, " │ ")
		if idx < 0 {
			continue // the trailing hint line carries no divider
		}
		found = true
		col := ansi.StringWidth(line[:idx])
		if dividerCol == -1 {
			dividerCol = col
			continue
		}
		if col != dividerCol {
			t.Errorf("line %d: divider at display column %d, want %d (established by an earlier row): %q", i, col, dividerCol, line)
		}
	}
	if !found {
		t.Fatal("no ` │ ` divider found in colored peek render; expected a horizontal split at width 120")
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
		event.NewToolCallFinished(sid, "call-1", "package authmw\n\nfunc Handler() {}\n", false, nil),
		event.NewMessageStarted(sid, event.MessageText),
		event.NewMessageFinished(sid, event.MessageText, "Middleware looks fine; running the test suite next."),
		event.NewToolCallStarted(sid, "call-2", "bash", json.RawMessage(`{"cmd":"go test ./..."}`)),
		event.NewToolCallFinished(sid, "call-2", "ok  authmw  1.2s\nok  handlers 0.8s\nFAIL session 0.1s", true, nil),
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
