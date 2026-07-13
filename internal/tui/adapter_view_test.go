package tui_test

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// TestGoldenProgramViewTopPadding covers the single-session TUI's own render
// path: cmd/gofer's driveTUI renders a [tui.Program] directly through
// bubbletea, bypassing App.render entirely (see [tui.Program.View]'s doc) —
// so it needs its own [layout.TopPadding] accounting, not just App's. This
// asserts the live frame carries that leading blank row, and that a
// user+agent turn renders with the user's prompt above the agent's reply,
// exactly as it does through App's attach screen.
func TestGoldenProgramViewTopPadding(t *testing.T) {
	var m tea.Model = tui.NewProgram(theme.Test())
	m, _ = m.Update(tea.WindowSizeMsg{Width: testkit.Width, Height: testkit.Height})
	for _, e := range []event.Event{
		event.NewMessageStarted(sid, event.MessageUser),
		event.NewMessageFinished(sid, event.MessageUser, "Ship the top-padding fix."),
		event.NewTurnStarted(sid),
		event.NewMessageStarted(sid, event.MessageText),
		event.NewMessageFinished(sid, event.MessageText, "Padding applied; nav contract untouched."),
		event.NewTurnFinished(sid, "end_turn", provider.Usage{InputTokens: 9, OutputTokens: 7}),
	} {
		m, _ = m.Update(tui.EventMsg{Event: e})
	}

	got := m.(tui.Program).View().Content
	testkit.AssertGolden(t, "program_view_top_padding", got)
}
