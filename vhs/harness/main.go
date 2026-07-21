// Command harness drives the real gofer TUI (internal/tui) through fixed,
// canned data so charmbracelet VHS can capture true rendered frames — colors,
// spacing, glyphs — that the plain-text Ascii golden tests can't show. It is
// dev tooling, not part of the shipped gofer binary: the tapes under vhs/
// point VHS at it (see scripts/tui-vhs.sh and docs/TUI.md).
//
// It renders through [theme.Default] (real color profile). The transcript-*
// scenes feed a scripted [event.Event] sequence into a live bubbletea
// [tui.Program] via Program.Send, exactly as cmd/gofer's driveTUI forwards a
// session's events; the roster-* scene renders a static [tui.Overview]
// snapshot; the panel-* scenes build the real [tui.App] over a canned
// [tui.Supervisor]/[tui.CommandEnv] and let the tape drive it with real
// keystrokes (see command.go's dispatchSlash) — so in every case what VHS
// records is the same render path a real gofer session produces. Pick the
// scene with -scenario (see [scenarioHelp] for the slug list — every slug
// follows `<area>-<view>[-<state>]`); the process holds the final frame
// until the tape quits it (Ctrl+C) or the safety hold elapses.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/decision"
	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// sid is a fixed session id; the scripted stream is single-session.
const sid = "0192a1b2-c3d4-7e5f-8a90-000000000001"

// fixedNow is a frozen wall clock so VHS frames render identically on every
// run — a prerequisite for committing them as golden images and diffing them
// cleanly in PRs. Any absolute timestamp the TUI derives from it (e.g. an
// OAuth token expiry) is then stable across renders.
var fixedNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// step is one scripted event plus how long to hold before sending the next, so
// VHS records the intermediate streaming frames a live turn produces (a running
// tool header, a delta-by-delta message) rather than only the settled state.
type step struct {
	ev    event.Event
	pause time.Duration
}

// scenarioHelp is both the -scenario flag's usage text and the unknown-
// scenario error's "want" list — the single place the harness's slug
// vocabulary is spelled out, so the two never drift apart. Slugs follow
// `<area>-<view>[-<state>]`, kebab-case: transcript-* (the attach scenes),
// roster-* (the overview scene), panel-* (the command-panel scenes).
const scenarioHelp = "transcript-tool-call | transcript-approval | roster-overview | panel-status-overview | panel-status | panel-config | panel-model | panel-model-empty | panel-model-daemon-refresh"

func main() {
	scenario := flag.String("scenario", "transcript-tool-call", "scripted scene to play: "+scenarioHelp)
	flag.Parse()

	// The attach scenes drive tui.NewProgram (the transcript) with a scripted
	// event stream; the roster scene is a pure snapshot with no event stream,
	// so it runs a static model and leaves script nil. The panel scenes drive
	// the real [tui.App] instead — they have no scripted event.Event stream of
	// their own; the tape types the slash command and any navigation keys
	// directly into the running program's stdin, the same path a real
	// terminal's keystrokes take.
	var (
		model  tea.Model = tui.NewProgram(theme.Default())
		script []step
	)
	switch *scenario {
	case "transcript-tool-call":
		script = toolCallScene()
	case "transcript-approval":
		script = approvalScene()
	case "roster-overview":
		model = overviewScene()
	case "panel-status-overview", "panel-status", "panel-config", "panel-model":
		model = commandViewApp(cannedCommandEnv())
	case "panel-model-empty":
		model = commandViewApp(emptyCommandEnv())
	case "panel-model-daemon-refresh":
		model = commandViewApp(daemonRefreshCommandEnv())
	default:
		fmt.Fprintf(os.Stderr, "harness: unknown scenario %q (want %s)\n", *scenario, scenarioHelp)
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
		// The trace is the exact two-entry shape loop.RuleGuard emits for an
		// unmatched, un-sandboxable call — the prompt DERIVES its rationale
		// paragraphs from it (see internal/tui's rationaleLines), so a made-up
		// trace string would record a demo of the "could not determine why"
		// fallback rather than of the feature.
		{event.NewPermissionRequested(sid, "perm-1", "bash", map[string]any{"command": "rm -rf /tmp/session-fixtures"},
			[]string{"rule: unmatched", "containable: false (no container configured)"}), beat},
	}
}

// overviewScene builds the roster screen over a mixed-state session set so VHS
// captures the ● status markers in color — the state the marker redesign moved
// out of glyph shape and into color alone, which the Ascii goldens are blind
// to: a working row (yellow ●), a permission-blocked row (yellow ●2, its live
// pending count), an awaiting-input row (yellow ●), and a finished row
// (green ●).
func overviewScene() tea.Model {
	now := fixedNow
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

// commandViewApp builds the real [tui.App] every panel-* scene shares: a
// canned two-session roster (so panel-status has a session to describe once
// the tape attaches into one) plus env, the [tui.CommandEnv] the caller
// supplies — cannedCommandEnv for the panel-status/panel-config/panel-model
// scenes, emptyCommandEnv for panel-model-empty. Unlike the transcript-*
// scenes, these have no scripted event.Event stream of their own — the tape
// drives the app directly, typing the slash command (and any navigation
// keys) into the running program's stdin, the same path a real terminal's
// keystrokes take (see command.go's dispatchSlash). Model fields use the
// SDK catalog's real ids (provider.Models()), not display names — the
// panel-model scene's ✓ active mark is [modelPickerView.activeModel] matching
// a row's id verbatim, so a display-name shorthand here would silently mark
// nothing.
func commandViewApp(env tui.CommandEnv) tea.Model {
	now := fixedNow
	sessions := []tui.SessionInfo{
		{ID: "sess-1", Title: "wire the websocket ACP listener", Summary: "streaming the daemon handshake", Status: tui.StatusWorking, Model: "claude-fable-5", Cwd: "~/orchestration", Updated: now.Add(-30 * time.Second)},
		{ID: "sess-2", Title: "keycloak path-b groundwork", Summary: "turn finished — awaiting the next prompt", Status: tui.StatusNeedsInput, Model: "claude-sonnet-5", Cwd: "~/orchestration", Updated: now.Add(-5 * time.Minute)},
	}
	meta := tui.OverviewMeta{App: "gofer", Version: "0.4.0", Model: "claude-fable-5", Cwd: "~/orchestration", Now: now}
	return tui.NewApp(theme.Default(), newVHSSupervisor(sessions), meta, env)
}

// cannedCommandEnv is the [tui.CommandEnv] most panel-* scenes read: a fixed
// version/cwd/root plus two representative authenticated providers — an
// Anthropic OAuth token with a real expiry and an OpenAI API key, exercising
// both [tui.AuthKind]s and their color states on the Status tab, and (once
// the Model tab reads them the same way) a non-empty picker list with an
// active-model checkmark — and the zero-value [config.Config] (gofer's own
// unconfigured defaults) so the Config tab's settings list renders real
// rows. SaveConfig is a no-op: none of these tapes commits an edit.
func cannedCommandEnv() tui.CommandEnv {
	return tui.CommandEnv{
		Version: "0.4.0",
		Cwd:     "~/orchestration",
		Root:    "~/.gofer",
		Auth: func() ([]tui.ProviderAuth, error) {
			return []tui.ProviderAuth{
				{Provider: "anthropic", Kind: tui.KindOAuth, Expires: fixedNow.Add(90 * 24 * time.Hour)},
				{Provider: "openai", Kind: tui.KindAPIKey},
			}, nil
		},
		Config:     func() (config.Config, error) { return config.Config{}, nil },
		SaveConfig: func(config.Config) error { return nil },
	}
}

// emptyCommandEnv is the [tui.CommandEnv] panel-model-empty reads: identical
// to cannedCommandEnv but with zero authenticated providers, so the Model
// tab renders its no-credentials empty state instead of a picker list.
func emptyCommandEnv() tui.CommandEnv {
	env := cannedCommandEnv()
	env.Auth = func() ([]tui.ProviderAuth, error) { return nil, nil }
	return env
}

// daemonRefreshCommandEnv is the [tui.CommandEnv] panel-model-daemon-refresh
// reads: cannedCommandEnv marked DAEMON-BACKED, with a stub gofer/hello probe
// standing in for a reachable, UNPINNED `gofer daemon`.
//
// It is what makes issue #162 visually demonstrable in a LIVE process. The tape
// screenshots the header, types a /model change, and screenshots the header
// again — one continuous run, no restart — and the two frames must differ. The
// probe answers with whatever id it is asked about, which is exactly how an
// unpinned daemon behaves: it re-reads its default per session/new, so asked
// straight after the write it reports the value just written.
//
// The probe is an in-process closure, so this scene performs ZERO network IO by
// construction — no daemon is dialed and no credential is read.
func daemonRefreshCommandEnv() tui.CommandEnv {
	env := cannedCommandEnv()
	env.DaemonBacked = true
	var adopted atomic.Pointer[string]
	env.SaveConfig = func(c config.Config) error {
		model := c.Session.Model
		adopted.Store(&model)
		return nil
	}
	env.DaemonDefaultModel = func(context.Context) (string, error) {
		if m := adopted.Load(); m != nil {
			return *m, nil
		}
		return "claude-fable-5", nil // the daemon's pre-change default
	}
	return env
}

// vhsSupervisor is the canned [tui.Supervisor] every panel-* scene drives:
// Roster answers with the fixed session set [commandViewApp] seeds, and
// Subscribe hands back a real (empty) [event.Subscription] off a private
// broker so attaching into a session doesn't error — nothing publishes to it,
// so the transcript underneath the panel stays empty, which is fine: these
// scenes are about the command panel, not the transcript. The write ops
// (Create/Send/Interrupt/Kill/Archive/SetModel/SetEffort/Reply/AnswerDecision)
// are no-ops; none of these tapes exercises them.
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

func (s *vhsSupervisor) SetModel(context.Context, string, string) error { return nil }

func (s *vhsSupervisor) SetEffort(context.Context, string, string) error { return nil }

func (s *vhsSupervisor) Reply(context.Context, string, string, bool, bool) error { return nil }

// Decisions hands back an already-closed subscription — no tape drives a
// structured decision, and a closed stream keeps the app's decision pump idle.
func (s *vhsSupervisor) Decisions(context.Context, string) (*decision.Subscription, error) {
	sub := decision.NewGate("").Subscribe(0)
	sub.Close()
	return sub, nil
}

func (s *vhsSupervisor) AnswerDecision(context.Context, string, string, []acp.DecisionAnswer) error {
	return nil
}
