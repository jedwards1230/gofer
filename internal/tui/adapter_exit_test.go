package tui_test

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// TestProgramFinalTranscript drives a Program through the bubbletea message
// loop the way driveTUI does — a window-size message then forwarded events —
// and asserts FinalTranscript reflects the whole conversation at the tracked
// width. This is the headless stand-in for the exit-flush path (no PTY).
func TestProgramFinalTranscript(t *testing.T) {
	var m tea.Model = tui.NewProgram(theme.Test())
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	for _, e := range []event.Event{
		event.NewTurnStarted(sid),
		event.NewMessageStarted(sid, event.MessageText),
		event.NewMessageFinished(sid, event.MessageText, "All done — shipped it."),
		event.NewTurnFinished(sid, "end_turn", provider.Usage{InputTokens: 5, OutputTokens: 4}),
	} {
		m, _ = m.Update(tui.EventMsg{Event: e})
	}

	ft := m.(tui.Program).FinalTranscript()
	if !strings.Contains(ft, "All done — shipped it.") {
		t.Errorf("FinalTranscript missing the final message:\n%s", ft)
	}
	if strings.Contains(ft, "▏") {
		t.Errorf("FinalTranscript should omit the input line, got:\n%s", ft)
	}
}

// TestProgramFinalTranscriptEmpty verifies a Program that saw no events flushes
// nothing.
func TestProgramFinalTranscriptEmpty(t *testing.T) {
	if got := tui.NewProgram(theme.Test()).FinalTranscript(); got != "" {
		t.Errorf("empty FinalTranscript = %q; want empty string", got)
	}
}
