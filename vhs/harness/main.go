// Command harness drives the real gofer TUI (internal/tui) through fixed,
// canned data so charmbracelet VHS can capture true rendered frames — colors,
// spacing, glyphs — that the plain-text Ascii golden tests can't show. It is
// dev tooling, not part of the shipped gofer binary: the tapes under vhs/
// point VHS at it (see scripts/tui-vhs.sh and docs/TUI.md).
//
// It renders through [theme.Default] (real color profile). The attach scenes
// (tool-call, approval) feed a scripted [event.Event] sequence into a live
// bubbletea [tui.Program] via Program.Send, exactly as cmd/gofer's driveTUI
// forwards a session's events; the roster scene (overview) renders a static
// [tui.Overview] snapshot; the command-panel scenes (panel, status, config)
// build the real [tui.App] over a canned [tui.Supervisor]/[tui.CommandEnv]
// and let the tape drive it with real keystrokes (see command.go's
// dispatchSlash) — so in every case what VHS records is the same render path
// a real gofer session produces. Pick the scene with -scenario; the process
// holds the final frame until the tape quits it (Ctrl+C) or the safety hold
// elapses.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/config"
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
	scenario := flag.String("scenario", "tool-call", "scripted scene to play: tool-call | approval | overview | panel | status | config")
	flag.Parse()

	// The attach scenes drive tui.NewProgram (the transcript) with a scripted
	// event stream; the overview scene is a pure roster snapshot with no event
	// stream, so it runs a static model and leaves script nil. The
	// panel/status/config scenes drive the real [tui.App] instead — they have
	// no scripted event.Event stream of their own; the tape types the slash
	// command and any navigation keys directly into the running program's
	// stdin, the same path a real terminal's keystrokes take.
	var (
		model  tea.Model = tui.NewProgram(theme.Default())
		script []step
	)
	switch *scenario {
	case "tool-call":
		script = toolCallScene()
	case "approval":
		script = approvalScene()
	case "overview":
		model = overviewScene()
	case "panel", "status", "config":
		model = commandViewApp()
	default:
		fmt.Fprintf(os.Stderr, "harness: unknown scenario %q (want tool-call | approval | overview | panel | status | config)\n", *scenario)
		os.Exit(2)
	}

	// tea.WithInput(os.Stdin) lets the tape's Ctrl+C reach handleKey, which
	// quits the program; the same key path a real attach uses.
	p := tea.NewProgram(model, tea.WithInput(os.Stdin))

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

// overviewScene builds the roster screen over a mixed-state session set so VHS
// captures the ● status markers in color — the state the marker redesign moved
// out of glyph shape and into color alone, which the Ascii goldens are blind
// to: a working row (yellow ●), a permission-blocked row (yellow ●2, its live
// pending count), an awaiting-input row (yellow ●), and a finished row
// (green ●).
func overviewScene() tea.Model {
	now := time.Now()
	meta := tui.OverviewMeta{App: "gofer", Version: "0.3.0", Model: "fable-5", Cwd: "~/orchestration", Now: now}
	sessions := []tui.SessionInfo{
		{ID: "sess-1", Title: "wire the websocket ACP listener", Summary: "streaming the daemon handshake", Status: tui.StatusWorking, Updated: now.Add(-30 * time.Second)},
		{ID: "sess-2", Title: "explore three agent ecosystems", Summary: "blocked: approve Bash(kubectl delete pod)", Status: tui.StatusWorking, Pending: 2, Updated: now.Add(-2 * time.Minute)},
		{ID: "sess-3", Title: "keycloak path-b groundwork", Summary: "turn finished — awaiting the next prompt", Status: tui.StatusNeedsInput, Updated: now.Add(-5 * time.Minute)},
		{ID: "sess-4", Title: "authentik token exchange rfc 8693", Summary: "Keycloak Path-B foundation complete and verified", Status: tui.StatusFinished, Updated: now.Add(-time.Hour)},
	}
	return overviewModel{over: tui.NewOverview(theme.Default(), meta).WithSessions(sessions)}
}

// overviewModel wraps a static [tui.Overview] as a bubbletea model so VHS can
// capture the roster screen. Unlike the attach transcript, the roster carries
// no event stream — it just redraws its snapshot on resize and quits on
// Ctrl+C, the same alt-screen frame [tui.App] renders it through live.
type overviewModel struct {
	over          tui.Overview
	width, height int
}

func (m overviewModel) Init() tea.Cmd { return nil }

func (m overviewModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyPressMsg:
		if key := msg.Key(); key.Mod.Contains(tea.ModCtrl) && key.Code == 'c' {
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m overviewModel) View() tea.View {
	v := tea.NewView(m.over.View(m.width, m.height))
	v.AltScreen = true
	return v
}

// commandViewApp builds the real [tui.App] the panel/status/config scenes
// share: a canned two-session roster (so /status has a session to describe
// once the tape attaches into one) plus a canned [tui.CommandEnv]. Unlike
// the attach scenes, these have no scripted event.Event stream of their
// own — the tape drives the app directly, typing the slash command (and any
// navigation keys) into the running program's stdin, the same path a real
// terminal's keystrokes take (see command.go's dispatchSlash).
func commandViewApp() tea.Model {
	now := time.Now()
	sessions := []tui.SessionInfo{
		{ID: "sess-1", Title: "wire the websocket ACP listener", Summary: "streaming the daemon handshake", Status: tui.StatusWorking, Model: "fable-5", Cwd: "~/orchestration", Updated: now.Add(-30 * time.Second)},
		{ID: "sess-2", Title: "keycloak path-b groundwork", Summary: "turn finished — awaiting the next prompt", Status: tui.StatusNeedsInput, Model: "sonnet-5", Cwd: "~/orchestration", Updated: now.Add(-5 * time.Minute)},
	}
	meta := tui.OverviewMeta{App: "gofer", Version: "0.4.0", Model: "fable-5", Cwd: "~/orchestration", Now: now}
	return tui.NewApp(theme.Default(), newVHSSupervisor(sessions), meta, cannedCommandEnv())
}

// cannedCommandEnv is the [tui.CommandEnv] the panel/status/config scenes
// read: a fixed version/cwd/root plus two representative authenticated
// providers — an Anthropic OAuth token with a real expiry and an OpenAI API
// key, exercising both [tui.AuthKind]s and their color states on the Status
// tab — and the zero-value [config.Config] (gofer's own unconfigured
// defaults) so the Config tab's settings list renders real rows. SaveConfig
// is a no-op: none of the three tapes commits an edit.
func cannedCommandEnv() tui.CommandEnv {
	return tui.CommandEnv{
		Version: "0.4.0",
		Cwd:     "~/orchestration",
		Root:    "~/.gofer",
		Auth: func() ([]tui.ProviderAuth, error) {
			return []tui.ProviderAuth{
				{Provider: "anthropic", Kind: tui.KindOAuth, Expires: time.Now().Add(90 * 24 * time.Hour)},
				{Provider: "openai", Kind: tui.KindAPIKey},
			}, nil
		},
		Config:     func() (config.Config, error) { return config.Config{}, nil },
		SaveConfig: func(config.Config) error { return nil },
	}
}

// vhsSupervisor is the canned [tui.Supervisor] the panel/status/config scenes
// drive: Roster answers with the fixed session set [commandViewApp] seeds,
// and Subscribe hands back a real (empty) [event.Subscription] off a private
// broker so attaching into a session doesn't error — nothing publishes to it,
// so the transcript underneath the panel stays empty, which is fine: these
// scenes are about the command panel, not the transcript. The write ops
// (Create/Send/Interrupt/Kill/Archive/Reply) are no-ops; none of the three
// tapes exercises them.
type vhsSupervisor struct {
	sessions []tui.SessionInfo
	broker   *event.Broker
}

func newVHSSupervisor(sessions []tui.SessionInfo) *vhsSupervisor {
	return &vhsSupervisor{sessions: sessions, broker: event.NewBroker()}
}

func (s *vhsSupervisor) Roster(context.Context) ([]tui.SessionInfo, error) {
	return s.sessions, nil
}

func (s *vhsSupervisor) Subscribe(context.Context, string) (*event.Subscription, error) {
	return s.broker.Subscribe(event.FilterAll, 8), nil
}

func (s *vhsSupervisor) Create(context.Context, string, tui.CreateOptions) (tui.SessionInfo, error) {
	return tui.SessionInfo{}, nil
}

func (s *vhsSupervisor) Send(context.Context, string, string) error { return nil }

func (s *vhsSupervisor) Interrupt(context.Context, string) error { return nil }

func (s *vhsSupervisor) Kill(context.Context, string) error { return nil }

func (s *vhsSupervisor) Archive(context.Context, string) error { return nil }

func (s *vhsSupervisor) Reply(context.Context, string, string, bool, bool) error { return nil }
