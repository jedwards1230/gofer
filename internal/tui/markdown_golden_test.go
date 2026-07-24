package tui_test

import (
	"testing"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"
)

// TestGoldenMarkdownRendered locks the plain (Ascii) rendering of a settled
// assistant reply that uses every markdown element the feature handles — a
// heading, bold/italic, an unordered list, inline code, a fenced code block,
// and a link. The golden is the structural oracle: glamour's colors are
// stripped under theme.Test's Ascii profile, so what remains is the wrapped,
// marker-aligned layout the transcript shows.
func TestGoldenMarkdownRendered(t *testing.T) {
	body := "# Release plan\n\n" +
		"Ship in **two** stages, *carefully*:\n\n" +
		"- cut the `v2` tag\n" +
		"- publish the notes\n\n" +
		"```go\nfunc main() {\n    run()\n}\n```\n\n" +
		"See the [runbook](https://example.com/runbook).\n"
	render(t, "markdown_rendered",
		event.NewMessageStarted(sid, event.MessageText),
		event.NewMessageFinished(sid, event.MessageText, body),
		event.NewTurnFinished(sid, "end_turn", provider.Usage{InputTokens: 20, OutputTokens: 40}),
	)
}

// TestGoldenMarkdownStreamingProgressive locks the incremental mid-stream frame:
// the message is STILL streaming (a MessageDelta with no MessageFinished), its
// two complete blocks (heading + bold paragraph) already glamoured — no "**"
// markers, the emphasis reflowed — while the trailing UNCLOSED ```go fence is
// held raw, delimiter and body verbatim, until it closes. Ascii-only, like the
// settled markdown_rendered golden: glamour's element colors aren't in the
// theme's marker palette, so a styled golden can't tag them (the streaming
// marker's warn color is pinned separately by mid_stream.styled).
func TestGoldenMarkdownStreamingProgressive(t *testing.T) {
	const streamed = "# Release plan\n\n" +
		"Ship in **two** stages, *carefully*.\n\n" +
		"```go\nfunc main() {\n    run()"
	render(t, "markdown_streaming_progressive",
		event.NewMessageStarted(sid, event.MessageText),
		event.NewMessageDelta(sid, event.MessageText, streamed),
	)
}
