// Command harness drives the real attach TUI (internal/tui) through a fixed,
// scripted event stream so charmbracelet VHS can capture true rendered frames —
// colors, spacing, glyphs — that the plain-text Ascii golden tests can't show.
// It is dev tooling, not part of the shipped gofer binary: the tapes under
// vhs/ point VHS at it (see scripts/tui-vhs.sh and docs/TUI.md).
//
// It renders through [theme.Default] (real color profile) and feeds a canned
// [event.Event] sequence into a live bubbletea [tui.Program] via Program.Send,
// exactly as cmd/gofer's driveTUI forwards a session's events — so what VHS
// records is the same render path a real attach produces. Pick the scene with
// -scenario; the process holds the final frame until the tape quits it (Ctrl+C)
// or the safety hold elapses.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// sid is a fixed session id; the scripted stream is single-session.
const sid = "0192a1b2-c3d4-7e5f-8a90-000000000001"

// step is one scripted event plus how long to hold before sending the next, so
// VHS records the intermediate streaming frames a live turn produces (a running
// tool header, a delta-by-delta message) rather than only the settled state.
type step struct {
	ev    event.Event
	pause time.Duration
}

func main() {
	scenario := flag.String("scenario", "tool-call", "scripted scene to play: tool-call | approval")
	flag.Parse()

	var script []step
	switch *scenario {
	case "tool-call":
		script = toolCallScene()
	case "approval":
		script = approvalScene()
	default:
		fmt.Fprintf(os.Stderr, "harness: unknown scenario %q (want tool-call | approval)\n", *scenario)
		os.Exit(2)
	}

	// tea.WithInput(os.Stdin) lets the tape's Ctrl+C reach handleKey, which
	// quits the program; the same key path a real attach uses.
	p := tea.NewProgram(tui.NewProgram(theme.Default()), tea.WithInput(os.Stdin))

	go func() {
		time.Sleep(600 * time.Millisecond) // let the alt screen settle before the first frame
		for _, s := range script {
			p.Send(tui.EventMsg{Event: s.ev})
			time.Sleep(s.pause)
		}
		time.Sleep(30 * time.Second) // safety hold; the tape normally quits sooner via Ctrl+C
		p.Quit()
	}()

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "harness: %v\n", err)
		os.Exit(1)
	}
}

// toolCallScene is a clean turn ending in a successful bash call: it shows the
// running header (bare tool name from the empty start-of-call seed), then the
// real command decoded from ToolCallFinished.Input, plus the blank-line rhythm
// between blocks.
func toolCallScene() []step {
	const beat = 350 * time.Millisecond
	return []step{
		{event.NewMessageStarted(sid, event.MessageUser), 0},
		{event.NewMessageFinished(sid, event.MessageUser, "Count the Go files in this repo."), beat},
		{event.NewTurnStarted(sid), beat},
		{event.NewMessageStarted(sid, event.MessageReasoning), 0},
		{event.NewMessageDelta(sid, event.MessageReasoning, "I'll count the .go files "), beat},
		{event.NewMessageDelta(sid, event.MessageReasoning, "with find piped to wc."), beat},
		{event.NewMessageFinished(sid, event.MessageReasoning, "I'll count the .go files with find piped to wc."), beat},
		{event.NewMessageStarted(sid, event.MessageText), 0},
		{event.NewMessageFinished(sid, event.MessageText, "Counting the Go files now."), beat},
		// Empty "{}" seed: the running header shows the bare tool name.
		{event.NewToolCallStarted(sid, "call-1", "bash", json.RawMessage(`{}`)), 900 * time.Millisecond},
		// The authoritative command arrives on finish and renders in the header.
		{event.NewToolCallFinished(sid, "call-1", json.RawMessage(`{"command":"find . -type f -name '*.go' | wc -l"}`), "421", false, nil), beat},
		{event.NewMessageStarted(sid, event.MessageText), 0},
		{event.NewMessageFinished(sid, event.MessageText, "There are 421 Go files."), beat},
		{event.NewTurnFinished(sid, "end_turn", provider.Usage{InputTokens: 34, OutputTokens: 12}), beat},
	}
}

// approvalScene ends in a pending permission request (the inline approval
// prompt commandeering the input line). Along the way it runs a failing test
// command, so the softened error styling — a warn-accented failed-call header
// with a dimmed result body — is on screen above the prompt.
func approvalScene() []step {
	const beat = 350 * time.Millisecond
	return []step{
		{event.NewMessageStarted(sid, event.MessageUser), 0},
		{event.NewMessageFinished(sid, event.MessageUser, "Refactor the auth middleware and run the tests."), beat},
		{event.NewTurnStarted(sid), beat},
		{event.NewMessageStarted(sid, event.MessageText), 0},
		{event.NewMessageFinished(sid, event.MessageText, "Running the test suite first."), beat},
		{event.NewToolCallStarted(sid, "call-1", "bash", json.RawMessage(`{}`)), 700 * time.Millisecond},
		{event.NewToolCallFinished(sid, "call-1", json.RawMessage(`{"command":"go test ./..."}`), "ok    authmw   1.2s\nok    handlers 0.8s\nFAIL  session  0.1s", true, nil), beat},
		{event.NewMessageStarted(sid, event.MessageText), 0},
		{event.NewMessageFinished(sid, event.MessageText, "One package failed. I need to remove a stale fixture before re-running."), beat},
		{event.NewTurnFinished(sid, "end_turn", provider.Usage{InputTokens: 88, OutputTokens: 41}), beat},
		{event.NewPermissionRequested(sid, "perm-1", "bash", map[string]any{"command": "rm -rf /tmp/session-fixtures"}, []string{"no rule"}), beat},
	}
}
