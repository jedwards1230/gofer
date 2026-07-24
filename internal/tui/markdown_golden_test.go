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
