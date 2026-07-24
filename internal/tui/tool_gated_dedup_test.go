package tui_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
)

// gatedAllowLifecycle is the exact event stream a live daemon fans out for one
// bash tool call that the permission guard gated and the user allowed. The
// ordering is load-bearing: the SDK emits ToolCallStarted while the provider
// streams the tool_use block, THEN the loop gates the call (loop.runTools runs
// after the turn's stream drains), so PermissionRequested arrives AFTER the tool
// item already exists and BEFORE ToolCallFinished. See the PermissionRequested
// case in Model.Ingest.
func gatedAllowLifecycle() []event.Event {
	return []event.Event{
		event.NewTurnStarted(sid),
		// Streamed tool_use: bare seed, the running `● bash` block.
		event.NewToolCallStarted(sid, "call-1", "bash", json.RawMessage(`{}`)),
		// The guard gates the call; the user allows it.
		event.NewPermissionRequested(sid, "call-1", "bash",
			map[string]any{"cmd": "echo hi"}, tui.GoldenTrace()),
		event.NewPermissionResolved(sid, "call-1", event.VerdictAllow, ""),
		// The call runs: authoritative args + output fill the SAME block.
		event.NewToolCallFinished(sid, "call-1", json.RawMessage(`{"cmd":"echo hi"}`), "hi", false, nil),
		event.NewTurnFinished(sid, "end_turn", provider.Usage{InputTokens: 3, OutputTokens: 1}),
	}
}

// TestGoldenToolCallGatedAllow locks the settled frame for a gated-then-allowed
// bash call: a SINGLE `● bash(echo hi)` block with its output, no stray empty
// `● bash` bullet beside it. The golden is the visual record; the count
// assertion below is the mutation guard that a golden alone cannot give.
func TestGoldenToolCallGatedAllow(t *testing.T) {
	render(t, "tool_call_gated_allow", gatedAllowLifecycle()...)
}

// TestToolCallGatedAllowRendersOneBlock is the mutation guard for the
// tool-call DOUBLING fix: a gated-then-allowed call must render EXACTLY ONE
// tool block. Before the fix the PermissionRequested case appended a separate
// itemApproval badge that outlived the resolve, so the allowed call rendered
// twice — the real `● bash(echo hi)` block AND an empty `● bash` bullet (one
// stray bullet per gated call). Neutralize the badgeIdx-reuse (append a fresh
// itemApproval instead of pointing at the tool item) and the empty bullet
// returns, flipping this test red.
func TestToolCallGatedAllowRendersOneBlock(t *testing.T) {
	got := testkit.Render(ingest(gatedAllowLifecycle()...), testkit.Width, testkit.Height)

	var headers, emptyBullets int
	for _, line := range strings.Split(got, "\n") {
		trimmed := strings.TrimRight(line, " ")
		switch {
		case trimmed == "● bash":
			// The empty badge bullet: the doubling artifact this fix removes.
			emptyBullets++
		case strings.HasPrefix(trimmed, "● bash"):
			// The real tool block header, e.g. "● bash(echo hi)".
			headers++
		}
	}

	if emptyBullets != 0 {
		t.Errorf("gated-allow tool call rendered %d empty `● bash` bullet(s) — the doubling regressed:\n%s", emptyBullets, got)
	}
	if headers != 1 {
		t.Errorf("gated-allow tool call rendered %d `● bash(...)` header(s), want exactly 1:\n%s", headers, got)
	}
	if !strings.Contains(got, "● bash(echo hi)") {
		t.Errorf("gated-allow tool block missing its authoritative args `● bash(echo hi)`:\n%s", got)
	}
	if !strings.Contains(got, "└ hi") {
		t.Errorf("gated-allow tool block missing its output `└ hi`:\n%s", got)
	}
}
