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
// until the tape quits it (Ctrl+C, TWICE — see the double-tap quit confirm,
// gofer#314) or the safety hold elapses.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/capability"
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
const scenarioHelp = "transcript-tool-call | transcript-approval | transcript-compacting | transcript-auto-compacting | transcript-overflow-recovery | roster-overview | roster-cwd-home | roster-cwd-missing | panel-status-overview | panel-status | panel-status-cwd-home | panel-config | panel-model | panel-model-empty | panel-model-daemon-refresh | panel-thinking | panel-usage | panel-stats | panel-help | panel-resume | panel-capabilities | panel-capabilities-unknown"

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
	case "roster-cwd-home":
		model = overviewCwdHomeScene()
	case "panel-status-overview", "panel-status", "panel-config", "panel-model",
		"panel-thinking", "panel-usage", "panel-stats", "panel-help", "panel-resume":
		// Every command-panel tab shares one canned App; the tape types the slash
		// command (and any navigation) that selects which tab is on screen. The
		// panel-resume scene additionally reads [vhsSupervisor.ListSessions].
		model = commandViewApp(cannedCommandEnv())
	case "panel-status-cwd-home":
		model = commandViewApp(cwdHomeCommandEnv())
	case "transcript-compacting":
		// An attach scene that nonetheless builds the real App, because the
		// state under capture is reached by DISPATCHING a slash command, not by
		// replaying an event stream — the tape types /compact itself.
		model = compactingApp()
	case "transcript-auto-compacting":
		// The same indicator as transcript-compacting, reached the OTHER way:
		// no slash command and no keystroke, just the event contract. See
		// autoCompactingApp.
		model = autoCompactingApp()
	case "transcript-overflow-recovery":
		// A settled sequence, not an in-flight state: the tape only attaches
		// and photographs the seeded backlog (see overflowRecoveryHistory).
		model = overflowRecoveryApp()
	case "roster-cwd-missing":
		// The same canned roster and env every panel-* scene renders — only the
		// Supervisor's behavior differs, exactly as transcript-compacting swaps
		// in a blocking Compact. Keeping the SESSION SET identical is what makes
		// this scene's frames comparable with the rest of the baseline, and what
		// keeps a new scenario from churning six other tapes' snapshots.
		model = cwdMissingApp()
	case "panel-model-empty":
		model = commandViewApp(emptyCommandEnv())
	case "panel-model-daemon-refresh":
		model = commandViewApp(daemonRefreshCommandEnv())
	case "panel-capabilities":
		// The /mcp + /skills scenes' POPULATED half: a backend that answered.
		model = commandViewApp(capabilitiesCommandEnv())
	case "panel-capabilities-unknown":
		// Their UNKNOWN half — a daemon-attached TUI whose backend cannot
		// report. panel-mcp.tape and panel-skills.tape each photograph BOTH
		// scenes, because one frame of a populated panel cannot show that the
		// unanswered one is different (CONTRIBUTING.md's before/after pair).
		model = commandViewApp(unknownCapabilitiesCommandEnv())
	default:
		fmt.Fprintf(os.Stderr, "harness: unknown scenario %q (want %s)\n", *scenario, scenarioHelp)
		os.Exit(2)
	}

	// tea.WithInput(os.Stdin) lets the tape's Ctrl+C reach handleKey, the same
	// key path a real attach uses — which now means the double-tap quit
	// confirm (gofer#314): a first Ctrl+C only arms, a second (immediately
	// following, with no other key between) quits. A tape that wants a
	// prompt frame in between (see roster-quit-confirm.tape) types Ctrl+C
	// once; one that just wants the harness to exit promptly types it twice.
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

// overviewCwdHomeScene builds the roster over sessions with absolute
// (not pre-tilde'd, unlike every other scene's fixture Cwd) working
// directories — and an absolute meta Cwd for the identity header — so both
// [tui.Overview]'s cwd group headers and the header's own `model · cwd` line
// exercise the REAL $HOME-contraction path (gofer#337) rather than rendering a
// literal "~" string that never touches the code under test.
//
// roster-cwd-home.tape runs this scenario TWICE, with HOME set to a
// different value each time — that env var, not a code difference, is the
// only thing that changes between the two captured frames, so the pair
// isolates exactly the feature: with HOME=/Users/justinother (matching
// none of these paths) every header renders its full absolute path, the
// pre-#337 appearance; with HOME=/Users/justin three of the four contract
// to "~"-relative headers while the fourth — /Users/justinother/notes, a
// SIBLING directory that merely shares "/Users/justin" as a text prefix —
// renders unchanged in the SAME frame, which is the path-boundary trap
// the issue exists to guard: a naive strings.HasPrefix would have
// contracted it into the nonsensical "~other/notes".
func overviewCwdHomeScene() tea.Model {
	now := fixedNow
	// An ABSOLUTE meta Cwd, so the identity header's `model · cwd` line rides
	// the same HOME swap as the group headers below it. It was previously
	// omitted entirely, which is why the tape could not show the header
	// spelling $HOME out while the group header two lines below contracted the
	// same path — see [identityHeaderLines].
	meta := tui.OverviewMeta{App: "gofer", Version: "0.4.0", Model: "fable-5", Cwd: "/Users/justin/orchestration/repos/gofer", Now: now}
	sessions := []tui.SessionInfo{
		{ID: "sess-1", Title: "wire the websocket ACP listener", Summary: "streaming the daemon handshake", Status: tui.StatusWorking, Cwd: "/Users/justin/orchestration/repos/gofer", Updated: now.Add(-30 * time.Second)},
		{ID: "sess-2", Title: "keycloak path-b groundwork", Summary: "turn finished — awaiting the next prompt", Status: tui.StatusNeedsInput, Cwd: "/Users/justin", Updated: now.Add(-5 * time.Minute)},
		{ID: "sess-3", Title: "authentik token exchange rfc 8693", Summary: "Keycloak Path-B foundation complete and verified", Status: tui.StatusFinished, Cwd: "/Users/justin/orchestration", Updated: now.Add(-time.Hour)},
		{ID: "sess-4", Title: "draft the Q3 planning notes", Summary: "sibling of $HOME — must NOT contract", Status: tui.StatusFinished, Cwd: "/Users/justinother/notes", Updated: now.Add(-2 * time.Hour)},
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
// compactingApp is the transcript-compacting scene: the same canned App every
// panel-* scene uses, but over a Supervisor whose Compact BLOCKS. The tape
// attaches to a session and dispatches /compact; the call is still in flight
// when the screenshot is taken, which is the only way the in-progress
// indicator exists to be captured at all.
//
// The hold is generous relative to the tape's own timeline: it must outlast
// the capture, and overshooting costs nothing because the tape quits with
// Ctrl+C long before it elapses (and Compact honors ctx regardless).
func compactingApp() tea.Model {
	sup := newVHSSupervisor(cannedSessions())
	sup.compactHold = 30 * time.Second
	sup.seed(compactableHistory()...)
	return commandViewAppOver(cannedCommandEnv(), sup)
}

// autoCompactDelay is how long autoCompactingApp waits before publishing the
// compaction start. It has to be long enough that the tape can attach and
// photograph the transcript BEFORE the indicator exists — the "before" half of
// the pair — with room for process startup jitter on either side.
const autoCompactDelay = 4 * time.Second

// autoCompactingApp is the AUTOMATIC-compaction scene (gofer#300), and the
// distinction from [compactingApp] above is the whole point: nothing is typed.
//
// compactingApp captures an indicator the user ASKED for — the tape types
// /compact, and the indicator lives as long as that call is held. Automatic
// compaction has no call and no keystroke: it is triggered supervisor-side, and
// before session.compaction_started existed the transcript simply froze for a
// minute with nothing on screen to explain it. So this scene publishes the
// start event on a timer with no input at all, which is the only honest mock of
// a compaction the client did not initiate.
//
// The delay is what makes a before/after PAIR possible. Seeding the start into
// the replay backlog instead would put the indicator on screen the instant the
// tape attaches, leaving no "before" frame — and a single frame of an indicator
// cannot show that it APPEARED on its own, which is the reviewable claim.
//
// No terminal event is ever published, so the indicator persists until the tape
// quits — the same reason compactingApp holds its Compact call.
func autoCompactingApp() tea.Model {
	sup := newVHSSupervisor(cannedSessions())
	sup.seed(compactableHistory()...)
	go func() {
		time.Sleep(autoCompactDelay)
		// ReplacesThrough and the message count match compactableHistory's two
		// seeded turns, so the figures on screen describe the transcript under
		// them rather than arbitrary numbers.
		sup.broker.Publish(event.NewSessionCompactionStarted("sess-1", "entry-482", 14))
	}()
	return commandViewAppOver(cannedCommandEnv(), sup)
}

// compactableHistory is the mocked conversation the compaction scene renders
// UNDER its indicator: two completed turns with a tool call in each.
//
// The history is the point, not set dressing. Compaction exists to replace a
// long context, so an indicator floating over an empty transcript shows the
// widget while misrepresenting the situation that produces it — the frame has
// to look like a session with something worth compacting. Two turns is the
// smallest history that reads as "a conversation in progress" rather than "one
// exchange."
//
// Seeded through the broker's retained backlog (see [vhsSupervisor.seed]), so
// it is already on screen when the tape attaches, with no publish-vs-subscribe
// race to make the captured frame timing-dependent. The ids are the attached
// session's ("sess-1"), matching the canned roster.
func compactableHistory() []event.Event {
	const s = "sess-1"
	return []event.Event{
		event.NewMessageStarted(s, event.MessageUser),
		event.NewMessageFinished(s, event.MessageUser, "Wire the websocket ACP listener and get the handshake streaming."),
		event.NewTurnStarted(s),
		event.NewMessageStarted(s, event.MessageText),
		event.NewMessageFinished(s, event.MessageText, "Reading the existing listener first."),
		event.NewToolCallStarted(s, "call-1", "read", json.RawMessage(`{}`)),
		event.NewToolCallFinished(s, "call-1", json.RawMessage(`{"path":"internal/daemon/listener.go"}`), "182 lines", false, nil),
		event.NewMessageStarted(s, event.MessageText),
		event.NewMessageFinished(s, event.MessageText, "The listener already accepts upgrades; it just never forwards the session events. I'll wire the fan-out."),
		event.NewTurnFinished(s, "end_turn", provider.Usage{InputTokens: 18420, OutputTokens: 260}),

		event.NewMessageStarted(s, event.MessageUser),
		event.NewMessageFinished(s, event.MessageUser, "Good. Add a test that proves the handshake replays to a late subscriber."),
		event.NewTurnStarted(s),
		event.NewMessageStarted(s, event.MessageText),
		event.NewMessageFinished(s, event.MessageText, "Adding it against the broker's retained backlog."),
		event.NewToolCallStarted(s, "call-2", "bash", json.RawMessage(`{}`)),
		event.NewToolCallFinished(s, "call-2", json.RawMessage(`{"command":"go test ./internal/daemon/ -run Handshake"}`), "ok  github.com/jedwards1230/gofer/internal/daemon  0.412s", false, nil),
		event.NewMessageStarted(s, event.MessageText),
		event.NewMessageFinished(s, event.MessageText, "Green. The late subscriber receives the full handshake."),
		event.NewTurnFinished(s, "end_turn", provider.Usage{InputTokens: 31775, OutputTokens: 415}),
	}
}

// overflowRecoveryApp is the transcript-overflow-recovery scene: the
// failure-triggered compaction sequence (jedwards1230/gofer#279) already
// settled on screen. Unlike the compaction scene next door, nothing here is
// in flight — the state under capture is a finished sequence of three
// transcript blocks, so the scene needs no blocking seam and the frame carries
// no live counter to churn the baseline (gofer#297).
func overflowRecoveryApp() tea.Model {
	sup := newVHSSupervisor(cannedSessions())
	sup.seed(overflowRecoveryHistory()...)
	return commandViewAppOver(cannedCommandEnv(), sup)
}

// overflowRecoveryHistory is the mocked stream the overflow-recovery scene
// renders: one completed turn for context, the prompt that overflowed, and
// then the three blocks the recovery produces — the notice, the compaction,
// and the answer to the re-issued turn.
//
// The GAP is the whole point of the scene, and it is why the events are seeded
// rather than described. A context-overflow rejection generates nothing: no
// text, no tool call, no usage. So between the user's prompt and the notice
// there is deliberately NO assistant output — the transcript really does jump
// from the prompt straight to "context window exceeded". A reader has to be
// able to see that the notice is the only thing standing between a prompt and
// an unexplained compaction; without it the frame would show a session
// silently skipping a beat, which is exactly the failure mode the notice
// exists to prevent.
//
// Both blocks are ordinary events (session.error, session.compacted) rendered
// by the ordinary items, so this scene adds no rendering path — it captures a
// SEQUENCE that did not exist before, not a new widget.
func overflowRecoveryHistory() []event.Event {
	const s = "sess-1"
	return []event.Event{
		event.NewMessageStarted(s, event.MessageUser),
		event.NewMessageFinished(s, event.MessageUser, "Wire the websocket ACP listener and get the handshake streaming."),
		event.NewTurnStarted(s),
		event.NewMessageStarted(s, event.MessageText),
		event.NewMessageFinished(s, event.MessageText, "Reading the existing listener first."),
		event.NewToolCallStarted(s, "call-1", "read", json.RawMessage(`{}`)),
		event.NewToolCallFinished(s, "call-1", json.RawMessage(`{"path":"internal/daemon/listener.go"}`), "182 lines", false, nil),
		event.NewMessageStarted(s, event.MessageText),
		event.NewMessageFinished(s, event.MessageText, "The listener already accepts upgrades; it just never forwards the session events."),
		event.NewTurnFinished(s, "end_turn", provider.Usage{InputTokens: 187420, OutputTokens: 260}),

		// The prompt whose call the provider REJECTED. Nothing follows it,
		// because a rejection produces nothing — see the doc above.
		event.NewMessageStarted(s, event.MessageUser),
		event.NewMessageFinished(s, event.MessageUser, "Now read every file under internal/daemon and summarize the fan-out."),

		event.NewSessionError(s, "context window exceeded — compacting the conversation and retrying this turn", false),
		event.NewSessionCompacted(s, "entry-482", 24, "claude-fable-5",
			provider.Usage{InputTokens: 188110, OutputTokens: 1240},
			// Kept inside the viewport width on purpose: blockRow text is not
			// width-wrapped (renderSessionCompactedLines splits on \n only), so
			// a longer summary hard-wraps to column 0 and the artifact would
			// draw the eye away from the sequence this frame exists to show.
			"Wired the websocket ACP listener; the handshake now replays to late subscribers."),

		event.NewTurnStarted(s),
		event.NewMessageStarted(s, event.MessageText),
		event.NewMessageFinished(s, event.MessageText, "The fan-out lives in listener.go: every accepted upgrade registers a subscription the router drains."),
		event.NewTurnFinished(s, "end_turn", provider.Usage{InputTokens: 24180, OutputTokens: 390}),
	}
}

func commandViewApp(env tui.CommandEnv) tea.Model {
	return commandViewAppOver(env, newVHSSupervisor(cannedSessions()))
}

// cannedSessions is the two-session roster every canned-App scene renders:
// one working, one awaiting input. Shared rather than inlined so the compaction
// scene's roster is the SAME roster as the panel scenes' — a scene that quietly
// rendered a different session set would make its frame incomparable with the
// rest of the baseline.
func cannedSessions() []tui.SessionInfo {
	now := fixedNow
	return []tui.SessionInfo{
		{ID: "sess-1", Title: "wire the websocket ACP listener", Summary: "streaming the daemon handshake", Status: tui.StatusWorking, Model: "claude-fable-5", Cwd: "~/orchestration", Updated: now.Add(-30 * time.Second)},
		{ID: "sess-2", Title: "keycloak path-b groundwork", Summary: "turn finished — awaiting the next prompt", Status: tui.StatusNeedsInput, Model: "claude-sonnet-5", Cwd: "~/orchestration", Updated: now.Add(-5 * time.Minute)},
	}
}

// cwdMissingSession / cwdMissingPath are which canned session the
// roster-cwd-missing scene reports as unattachable, and the recorded directory
// it reports as gone.
//
// sess-1 deliberately: it is the roster's FIRST row, so a bare Enter attaches
// it and the tape needs no navigation keys before the state it is capturing.
// The path is a plausible retired project — absolute, since a recorded cwd
// always is, and fixed, since these frames are committed and diffed.
const (
	cwdMissingSession = "sess-1"
	cwdMissingPath    = "/Users/justin/orchestration/repos/retired-service"
)

// cwdMissingApp is the roster-cwd-missing scene: the canned App, over a
// Supervisor that answers an attach of [cwdMissingSession] the way a real
// daemon does when that session's recorded directory has been deleted — with
// the typed cwd-missing signal (jedwards1230/gofer#326), which the TUI turns
// into its three-way prompt.
//
// The roster and CommandEnv are the shared canned ones, unchanged, so every
// other panel-*/roster-* frame in the baseline stays byte-identical.
func cwdMissingApp() tea.Model {
	sup := newVHSSupervisor(cannedSessions())
	sup.cwdMissingID = cwdMissingSession
	sup.cwdMissingDir = cwdMissingPath
	return commandViewAppOver(cannedCommandEnv(), sup)
}

// commandViewAppOver builds the canned App over a caller-supplied Supervisor,
// which is what lets the compaction scene swap in a blocking Compact without
// duplicating the roster or the meta.
func commandViewAppOver(env tui.CommandEnv, sup *vhsSupervisor) tea.Model {
	meta := tui.OverviewMeta{App: "gofer", Version: "0.4.0", Model: "claude-fable-5", Cwd: "~/orchestration", Now: fixedNow}
	return tui.NewApp(theme.Default(), sup, meta, env)
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

// cwdHomeCommandEnv is the [tui.CommandEnv] panel-status-cwd-home reads:
// cannedCommandEnv with an ABSOLUTE Cwd instead of the literal "~/orchestration"
// string every other panel-* scene uses. That literal never touches
// [displayHome] — it doesn't start with any real $HOME on any machine — so it
// can't demonstrate the Status tab's "Cwd: " row contraction (gofer#337); an
// absolute path can, the same way [overviewCwdHomeScene] does for the roster's
// group headers, and for the same reason: panel-status-cwd-home.tape runs it
// under two different HOME values so the ONLY variable between its two frames
// is whether the real $HOME matches this Cwd.
func cwdHomeCommandEnv() tui.CommandEnv {
	env := cannedCommandEnv()
	env.Cwd = "/Users/justin/orchestration"
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

// capabilitiesCommandEnv is the [tui.CommandEnv] the panel-capabilities scene
// reads: cannedCommandEnv plus a capability report chosen to put every state
// the /mcp and /skills tabs have a distinct WORD for on screen at once —
// connected, down, an unrecognized transport, disabled, a shadowed duplicate,
// a size-skipped candidate, a disabled skill, a truncated description.
//
// That breadth is the point of the tape. The Ascii goldens already pin the
// text; what only a real colour render can show is whether those states are
// also visually distinguishable (green/yellow/muted/red) at a glance — and
// whether any of the styling scatters or mis-measures at width.
//
// The closure is in-process, so this scene performs ZERO network IO and the
// frame is fully deterministic.
func capabilitiesCommandEnv() tui.CommandEnv {
	env := cannedCommandEnv()
	env.Capabilities = func(context.Context) (capability.Answer, error) {
		return capability.Answer{Known: true, Snapshot: capability.Snapshot{
			MCP: capability.MCP{
				Servers: []capability.Server{
					{Name: "github", ConfiguredTransport: "stdio", Enabled: true, Connected: true},
					{Name: "linear", ConfiguredTransport: "http", Enabled: true},
					{Name: "legacy-ws", Enabled: true},
					{Name: "scratch", ConfiguredTransport: "stdio"},
				},
				ConnectedTools: 7,
				SchemaMode:     "index",
				ResidentTools:  1,
				IndexOnlyTools: 6,
			},
			Skills: capability.Skills{
				Directories: []string{"~/orchestration/.gofer/skills", "~/.gofer/skills"},
				Loaded: []capability.Skill{
					{Name: "commit-msg", Description: "Write a conventional-commit message from a staged diff"},
					{Name: "deep-dive", Description: "Trace a symbol across packages before changing it", Truncated: true},
					{Name: "release", Description: "Cut a release and draft its notes", Disabled: true},
				},
				Diagnostics: []capability.Diagnostic{
					{Path: "~/.gofer/skills/commit-msg/SKILL.md", Detail: `skill: duplicate name "commit-msg"; the earlier directory's definition wins`, Shadowed: true},
					{Path: "~/.gofer/skills/whole-repo/SKILL.md", Detail: "skill: body exceeds 262144 bytes"},
				},
				Summary: `skills: skipped ~/.gofer/skills/commit-msg/SKILL.md: skill: duplicate name "commit-msg"; the earlier directory's definition wins (+1 more)`,
			},
		}}, nil
	}
	return env
}

// unknownCapabilitiesCommandEnv is the [tui.CommandEnv] the
// panel-capabilities-unknown scene reads: a DAEMON-BACKED env whose capability
// closure answers UNKNOWN, exactly as an attached `gofer daemon --workers`
// router (or a daemon predating gofer/capabilities) does.
//
// It is the "after" half of each tape's pair, and the frame that de-risks the
// whole feature: side by side with the populated one it shows that an
// unanswered panel looks nothing like an empty one, which is precisely the
// confusion a colourless golden cannot rule out on its own.
func unknownCapabilitiesCommandEnv() tui.CommandEnv {
	env := cannedCommandEnv()
	env.DaemonBacked = true
	env.Capabilities = func(context.Context) (capability.Answer, error) {
		return capability.Answer{}, nil
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
	// mu guards sessions. Roster is served from a tea.Cmd goroutine while
	// Kill/Archive mutate from another, so the two need a lock — a canned
	// roster that was only ever read did not.
	mu       sync.Mutex
	sessions []tui.SessionInfo
	broker   *event.Broker

	// compactHold is how long Compact blocks before returning. Zero (every
	// scene but transcript-compacting) returns immediately, as every other
	// write op does. The compaction scene needs the opposite: the in-progress
	// indicator lives exactly as long as the call does, so against an
	// instant-returning Compact it would appear and vanish inside one frame,
	// with nothing left to photograph. Holding the call is what makes the state
	// under capture actually exist.
	compactHold time.Duration

	// cwdMissingID / cwdMissingDir drive the roster-cwd-missing scene: when
	// [vhsSupervisor.Subscribe] is asked for cwdMissingID, it raises the typed
	// "recorded directory is gone" signal for cwdMissingDir instead of the
	// attach settling normally. Empty (every other scene) raises nothing.
	//
	// Implementing OnSessionCwdMissing at all is what makes this Supervisor
	// satisfy the notifier seam tui.App type-asserts against. The assertion, not
	// the interface, is the contract: tui.Supervisor does not require the method,
	// so a Supervisor that drops it still compiles and just never raises the
	// prompt (both backends gofer ships implement it — internal/daemonbridge and
	// internal/tuibridge).
	cwdMissingID  string
	cwdMissingDir string
	// cwdMissing is the handler tui.App registers at construction. It is read
	// from the Subscribe goroutine and written from the App's, so it is guarded
	// by mu like sessions.
	cwdMissing func(sessionID, cwd string)
}

// OnSessionCwdMissing satisfies the seam tui.App type-asserts its Supervisor
// against (see internal/tui/cwdprompt.go's cwdMissingNotifier). The real
// implementation is daemonbridge.Supervisor's; this is the canned one.
func (s *vhsSupervisor) OnSessionCwdMissing(fn func(sessionID, cwd string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cwdMissing = fn
}

// raiseCwdMissingFor reports the signal for id, if that is the session this
// scene is configured to fail, on a BACKGROUND goroutine after a short delay.
//
// Both details mirror production. The signal really does arrive on a background
// goroutine (the reconstruction core's per-session load), and it really does
// arrive AFTER the attach screen has opened — the load is asynchronous — so a
// tape that fired it synchronously would capture a state sequence real users
// never see. The delay is comfortably shorter than any tape's frame spacing, so
// it stays deterministic rather than racing the screenshot.
func (s *vhsSupervisor) raiseCwdMissingFor(id string) {
	s.mu.Lock()
	fn, want, dir := s.cwdMissing, s.cwdMissingID, s.cwdMissingDir
	s.mu.Unlock()
	if fn == nil || want == "" || id != want {
		return
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		fn(id, dir)
	}()
}

func newVHSSupervisor(sessions []tui.SessionInfo) *vhsSupervisor {
	// WithReplay retains a must-deliver backlog, which is what lets [seed] mock
	// a session's prior conversation BEFORE anything attaches: Subscribe replays
	// it, so the history is there the instant the tape opens the session. Doing
	// it through retention rather than a timed publish is deliberate — a
	// publish racing the subscribe would make the captured frame depend on which
	// won, and these frames are committed and diffed.
	//
	// Only stream deltas are lossy (event.TierOf), so every event a scripted
	// conversation is built from — message started/finished, tool call
	// started/finished, turn started/finished — is retained.
	return &vhsSupervisor{sessions: sessions, broker: event.NewBroker(event.WithReplay(replayCap))}
}

// replayCap bounds the mocked backlog. It also bounds what a scene can seed:
// [vhsSupervisor.seed] refuses to exceed it rather than silently dropping the
// OLDEST events, which would truncate a scripted conversation from the top and
// look like a rendering bug.
const replayCap = 64

// seed mocks a session's prior conversation by publishing it into the broker
// before any client attaches. The events replay in publish order on Subscribe.
func (s *vhsSupervisor) seed(events ...event.Event) {
	if len(events) > replayCap {
		panic(fmt.Sprintf("harness: seeded %d events, replay backlog holds %d — "+
			"the oldest would be dropped and the transcript would render truncated",
			len(events), replayCap))
	}
	for _, e := range events {
		s.broker.Publish(e)
	}
}

func (s *vhsSupervisor) Roster(context.Context) ([]tui.SessionInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]tui.SessionInfo(nil), s.sessions...), nil
}

// Subscribe hands back a real subscription off the private broker, replaying
// whatever [vhsSupervisor.seed] mocked. The buffer is sized to the replay cap:
// a subscription too small to hold the backlog would stall the replay, so the
// two bounds are tied together rather than picked independently.
//
// It is also where the roster-cwd-missing scene's failure originates, because
// that is where it originates in production: attaching a session is what makes
// the daemon load it, and loading is what discovers the recorded directory is
// gone. Every other scene passes [vhsSupervisor.raiseCwdMissingFor] straight
// through (its cwdMissingID is empty), so the subscribe path is unchanged for
// them.
func (s *vhsSupervisor) Subscribe(_ context.Context, id string) (*event.Subscription, error) {
	s.raiseCwdMissingFor(id)
	return s.broker.Subscribe(event.FilterAll, replayCap), nil
}

func (s *vhsSupervisor) Create(context.Context, string, tui.CreateOptions) (tui.SessionInfo, error) {
	return tui.SessionInfo{}, nil
}

// ListSessions backs the panel-resume scene's picker: a small frozen listing
// (an offline session no longer in the roster, plus a live one) so /resume
// renders a real list rather than an empty state. Updated times derive from
// [fixedNow] so the resume snapshot is reproducible across renders.
func (s *vhsSupervisor) ListSessions(context.Context) ([]tui.SessionRef, error) {
	return []tui.SessionRef{
		{ID: "0192a0c4-off0-7000-8000-000000000009", Title: "an offline session", Cwd: "/home/j/elsewhere", Updated: fixedNow.Add(-time.Hour)},
		{ID: "sess-1", Title: "wire the websocket ACP listener", Cwd: "~/orchestration", Updated: fixedNow.Add(-2 * time.Minute)},
	}, nil
}

func (s *vhsSupervisor) Resume(context.Context, string, string) error { return nil }

func (s *vhsSupervisor) Send(context.Context, string, string) error { return nil }

func (s *vhsSupervisor) Interrupt(context.Context, string) error { return nil }

// Kill marks id terminal rather than returning a bare nil, because the state
// roster-delete-lands.tape photographs IS the roster changing: gofer#322 made a
// roster-mutating op refetch immediately instead of leaving the stale row on
// screen until the next 1s poll, and against a Supervisor whose roster never
// changes there is nothing for that refetch to reveal — the tape would render
// two identical frames and read as decoration.
//
// StatusFinished, not removal: a kill terminates the session and KEEPS its
// journal (repo invariant #4), so the row stays on the roster as a terminal
// one. Removal is what Archive does.
func (s *vhsSupervisor) Kill(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.sessions {
		if s.sessions[i].ID == id {
			s.sessions[i].Status = tui.StatusFinished
			s.sessions[i].Summary = "killed"
		}
	}
	return nil
}

// Archive drops id from the roster — see [vhsSupervisor.Kill] for why these
// two mutate at all.
func (s *vhsSupervisor) Archive(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.sessions[:0]
	for _, sess := range s.sessions {
		if sess.ID != id {
			out = append(out, sess)
		}
	}
	s.sessions = out
	return nil
}

func (s *vhsSupervisor) SetModel(context.Context, string, string) error { return nil }

func (s *vhsSupervisor) SetEffort(context.Context, string, string) error { return nil }

// Compact holds for [vhsSupervisor.compactHold] before succeeding, so the
// transcript-compacting scene can capture an in-progress compaction. It honors
// ctx so the tape's Ctrl+C still exits promptly rather than waiting out the
// hold.
func (s *vhsSupervisor) Compact(ctx context.Context, _, _ string) error {
	if s.compactHold == 0 {
		return nil
	}
	select {
	case <-time.After(s.compactHold):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *vhsSupervisor) Reply(context.Context, string, string, tui.PermissionDecision) error {
	return nil
}

// ExplainPermission answers with a canned rationale: no tape drives ctrl+e
// (these scenes are about the command panel), and a scene that later does
// should render an explained prompt rather than an error banner.
func (s *vhsSupervisor) ExplainPermission(context.Context, string, string) (acp.PermissionRationale, error) {
	return acp.PermissionRationale{
		Reason: "No permission rule matched this call, so gofer is asking before it runs.",
		Policy: "unmatched",
		Trace:  []string{"rule: unmatched"},
	}, nil
}

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

func (s *vhsSupervisor) RestartDaemon(context.Context) error { return nil }

// DaemonVersion answers "": no tape drives the stale-daemon banner's restart.
func (s *vhsSupervisor) DaemonVersion() string { return "" }
