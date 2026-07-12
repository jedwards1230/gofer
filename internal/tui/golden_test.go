package tui_test

import (
	"testing"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

const sid = "0192a1b2-c3d4-7e5f-8a90-000000000001"

// ingest builds a Model from a fixed theme and replays events onto it in
// order, the pattern every golden test in this package shares.
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
// It establishes the testkit harness end to end before the rest of the
// attach model (reasoning, tool calls, errors, approvals, input editing)
// is built out against it.
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
