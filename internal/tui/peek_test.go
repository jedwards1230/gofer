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

// peekTail builds a read-only transcript for the peeked session: a reasoning
// note, a settled tool call, final text, and a finished turn with usage.
func peekTail() tui.Model {
	m := tui.New(theme.Test())
	for _, e := range []event.Event{
		event.NewTurnStarted(sid),
		event.NewMessageStarted(sid, event.MessageReasoning),
		event.NewMessageFinished(sid, event.MessageReasoning, "Checking the ACP handshake path."),
		event.NewToolCallStarted(sid, "call-1", "bash", json.RawMessage(`{"cmd":"go test ./acp"}`)),
		event.NewToolCallFinished(sid, "call-1", json.RawMessage(`{"cmd":"go test ./acp"}`), "ok  acp  0.4s", false, nil),
		event.NewMessageStarted(sid, event.MessageText),
		event.NewMessageFinished(sid, event.MessageText, "Tests pass. The listener is wired."),
		event.NewTurnFinished(sid, "end_turn", provider.Usage{InputTokens: 40, OutputTokens: 18}),
	} {
		m = m.Ingest(e)
	}
	return m
}

func newPeek() tui.Peek {
	over := newOverview().WithSessions(rosterFixture())
	return tui.NewPeek(theme.Test(), over, peekTail())
}

// TestGoldenPeekVertical renders the default stacked split (roster above the
// tail) at the standard 80-column width, below the horizontal breakpoint.
func TestGoldenPeekVertical(t *testing.T) {
	testkit.AssertGolden(t, "peek_vertical", testkit.Render(newPeek(), testkit.Width, testkit.Height))
}

// TestGoldenPeekHorizontal renders the side-by-side split (roster left, tail
// right) at 130 columns, at or above the horizontal breakpoint.
func TestGoldenPeekHorizontal(t *testing.T) {
	testkit.AssertGolden(t, "peek_horizontal", testkit.Render(newPeek(), 130, testkit.Height))
}

// TestGoldenPeekNextSession renders the peek after j moves the roster
// selection to the next session.
func TestGoldenPeekNextSession(t *testing.T) {
	testkit.AssertGolden(t, "peek_next_session", testkit.Render(newPeek().NextSession(), testkit.Width, testkit.Height))
}

// TestPeekSessionSwitch verifies j/k move the peeked session and clamp at the
// roster ends.
func TestPeekSessionSwitch(t *testing.T) {
	p := newPeek()
	first := p.SelectedID()
	if got := p.PrevSession().SelectedID(); got != first {
		t.Errorf("PrevSession at top moved selection: got %q want %q", got, first)
	}
	next := p.NextSession().SelectedID()
	if next == first {
		t.Error("NextSession did not move the selection")
	}
}
