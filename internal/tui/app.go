package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/decision"
	"github.com/jedwards1230/gofer/internal/tui/layout"
	"github.com/jedwards1230/gofer/internal/tui/theme"
	"github.com/jedwards1230/gofer/internal/usercmd"
)

// screen selects which of the three navigation-contract surfaces [App] is
// currently rendering: the roster overview, the read-only peek split, or the
// full attach transcript+input.
type screen int

const (
	screenOverview screen = iota
	screenPeek
	screenAttach
)

// rosterInterval is how often App polls [Supervisor.Roster] to pick up
// status/summary changes from sessions it isn't currently subscribed to.
const rosterInterval = 1 * time.Second

// scrollPageLines and scrollWheelLines are the step sizes App.scroll moves by
// per PgUp/PgDn key press and per mouse-wheel notch, respectively — a page
// jump is deliberately larger than a wheel notch, matching how most terminal
// apps pace the two inputs differently. Both are plain line counts, not tied
// to the terminal's actual height: [Overview.body]/[Model.view] clamp the
// resulting offset to the content's real length via scrollTail, so an
// oversized step just clamps to the top of the content instead of
// overshooting.
//
// scrollWheelLines was tuned down from 3 to 2 for a smoother feel: every
// wheel notch this package receives is applied in full (handleWheel has no
// debounce/coalescing of its own, and bubbletea delivers one MouseWheelMsg
// per notch — see TestWheelNotchesAccumulateWithoutDrops), so the
// "jumpiness" a fixed per-notch step can produce is purely a function of the
// step size, not dropped input. 2 lines/notch stays inside the same 1-3 line
// band most terminal apps use for line-based (non-pixel) wheel scroll, just
// gentler than 3.
const (
	scrollPageLines  = 10
	scrollWheelLines = 2
)

// statusSeverity is what a transient status note MEANS, which is the only
// thing that decides its color (issue #161). Before this existed every note
// rendered in [theme.Theme.DangerStyle], so "Default model set." was visually
// identical to "openai: http 400" and a user reasonably read a success as a
// failure.
//
// The zero value is [sevDanger] deliberately: it is the historical behavior,
// so a status write that forgets a severity degrades to the pre-#161 rendering
// rather than silently claiming success. Clearing a.status resets it here too,
// so a stale severity can never outlive the note it described.
type statusSeverity int

const (
	// sevDanger is an operation that FAILED: a Supervisor op error, a
	// config read/write error, an unknown command. [opDoneMsg]'s error path
	// is the only route to danger for an op RESULT.
	sevDanger statusSeverity = iota
	// sevWarn is a caveat: the action did something, but not everything the
	// user might expect (a cross-provider pick that can't hot-swap, a
	// default the attached daemon is pinned against, a clipped paste).
	sevWarn
	// sevOK is an unqualified success.
	sevOK
)

// style resolves the severity to the theme style the status footer renders in.
func (s statusSeverity) style(th theme.Theme) lipgloss.Style {
	switch s {
	case sevOK:
		return th.OKStyle()
	case sevWarn:
		return th.WarnStyle()
	default:
		return th.DangerStyle()
	}
}

// App is gofer's TUI root: the bubbletea [tea.Model] that ties the
// overview/peek/attach screens together, enforces the navigation contract
// between them, and drives the daemon exclusively through [Supervisor] — the
// same Op surface any other ACP client uses (repo invariant: everything is a
// client). It holds at most one live [event.Subscription] at a time, for
// whichever session is currently peeked or attached.
type App struct {
	theme theme.Theme
	sup   Supervisor

	over Overview // roster screen
	sess Model    // transcript of the peeked/attached session

	// panel is the open command-panel overlay a slash command opens (see
	// command.go, panel.go); nil = none. It composes over whatever screen
	// scr currently shows rather than being a fourth [screen] — a stub tab
	// bar for M4 step 1, with the real /status, /config, and /model bodies
	// landing in follow-up PRs. Dispatch precedence: panel > approval >
	// decision > menu > active screen > global (see Update).
	panel *commandPanel

	// registry resolves a submitted "/name arg…" buffer's command token to
	// the [Command] that runs it.
	registry Registry

	// shellRuns is every `!` / `!!` shell escape this client has run, oldest
	// first (shell.go). It is the backing state for BOTH the transcript
	// rendering and the model-context fold, which is deliberate: one list, one
	// flag per run (shellRun.inContext), so what the user sees and what the
	// model sees can never be computed from different data. shellSeq is the
	// monotonic id a dispatched run is matched back by. Runs render into the
	// attach transcript via Model.WithShellRuns; unconsumed runs there are the
	// ones a subsequent prompt will fold in (see App.composePrompt), so what
	// shows and what will be sent stay one truth.
	shellRuns []shellRun
	shellSeq  int

	// shellQueue is the sticky reply-now/queue mode ctrl+r toggles (keymap.go).
	// false (the default) is reply-now: a finished `!` run on the attach screen
	// flushes everything pending through composePrompt and fires a turn at once
	// (see the shellDoneMsg handler). true is queue: a `!` run waits for the
	// user's next Enter, so they can stack more commands or a typed message
	// first. It is captured onto each run at dispatch ([shellRun.queued]) so a
	// later toggle never rewrites a run already in flight, and it governs `!`
	// only — `!!` is never sent regardless of the mode.
	shellQueue bool

	// files is the `@` file-mention completion's cached candidate list
	// (filemention.go), refreshed once per mention off the Update loop.
	files fileCandidates

	// menuToken records whether the last [App.syncMenu] found an active
	// command token in the live buffer. It exists only to detect the
	// closed→open EDGE, which is when the registry's markdown layer is
	// reloaded from disk ([App.loadUserCommandsCmd]) — once per "/" typed
	// rather than once per keystroke. It tracks the `/` token specifically
	// (see syncMenu): an `@` mention has no markdown layer to reload, and
	// latching this on one would eat the next `/`'s edge.
	menuToken bool

	// menu is the slash-command autocomplete popup (command_menu.go): closed
	// (zero value) whenever the overview dispatch bar / attach input's
	// buffer has no active command token, kept in sync by [App.syncMenu]
	// after every per-screen key handler runs (see Update). Composed above
	// the active screen's input rule in render.
	menu commandMenu

	// commandEnv is the read-only data source the command panel's views read
	// (version/cwd/store-root identity, lazy auth/config reads — see
	// env.go). Handed to the panel at open time by openPanel (command.go).
	commandEnv CommandEnv

	// cwd is this client's working directory (the same value the roster
	// header shows). The dispatch bar passes it as the new session's cwd so a
	// session created from the TUI carries the client's project directory —
	// not the daemon's launch dir — and is therefore visible to other clients
	// (e.g. a phone) filtering session/list by that cwd.
	cwd string

	sessID string // id `sess` is subscribed to ("" = none)
	sub    *event.Subscription

	// decSub is the SECOND stream an attach holds for the same session: its
	// open structured-decision requests (see [Supervisor.Decisions] and
	// internal/decision). It is separate from sub because the SDK's Event
	// union carries no decision kind, not because a decision is a different
	// KIND of thing to a client — so it is established, pumped, and torn down
	// in lockstep with sub (see the subReadyMsg case in Update, and
	// switchSession).
	decSub *decision.Subscription

	// peekReply is the peek card's reply-input buffer. Peek carries no
	// transcript to own it, so the app root holds it and clears it on entering
	// peek, sending a reply, or leaving.
	peekReply string

	scr    screen
	width  int
	height int

	status string // transient error/status line, cleared on the next key press

	// statusSev is how a.status is COLORED (issue #161). The status line is
	// the only feedback channel several actions have — notably /model — so
	// rendering a success in the same red as an HTTP 400 actively tells the
	// user their successful action failed. Every write to a.status goes
	// through [App.setStatus], which sets both fields together; the zero
	// value is [sevDanger] because a cleared status renders nothing at all
	// and because every pre-severity note was an error path.
	statusSev statusSeverity

	// scroll is the active screen's manual scroll-back offset into its
	// header+content region — 0 (the default) is tail-to-latest: the
	// overview follows the selection and the attach transcript tails new
	// messages, exactly as before this field existed. A mouse wheel
	// (handleWheel) or PgUp/PgDn (handleOverviewKey/handleAttachKey) moves
	// it; [Overview.body]/[Model.view] clamp it to the content's actual
	// length via scrollTail, so it is safe to grow unbounded here. Reset to
	// 0 wherever the screen or the attached/peeked session changes (see
	// switchSession and the a.scr assignments below) so navigating away and
	// back always lands back at the tail rather than a stale offset into
	// different content.
	scroll int

	// sel is the app-owned mouse click-drag text selection (mouse.go); nil
	// when nothing is selected. Set on tea.MouseClickMsg, extended on
	// tea.MouseMotionMsg while the left button stays held, and frozen (but
	// still shown/copyable) on tea.MouseReleaseMsg. Cleared on the next
	// click (handleMouseClick always installs a fresh one) or any key press
	// (see Update's tea.KeyPressMsg case) — never on scroll, so wheel/PgUp-
	// PgDn scrolling during or after a selection leaves it in place.
	sel *selectionState
}

// NewApp returns an App rendering through th, driving sup, with its roster
// screen seeded from meta and its command panel's views reading env.
func NewApp(th theme.Theme, sup Supervisor, meta OverviewMeta, env CommandEnv) App {
	a := App{
		theme:      th,
		sup:        sup,
		over:       NewOverview(th, meta),
		sess:       New(th),
		scr:        screenOverview,
		cwd:        meta.Cwd,
		registry:   newBuiltinRegistry(),
		commandEnv: env,
	}
	// Seed the sticky shell reply-now/queue mode from config so a user who
	// always wants queue mode launches in it instead of re-pressing ctrl+r every
	// session (config.TUI.ShellReplyMode, default reply-now). A one-shot read at
	// construction, not a per-frame resolve like autoscroll: the ctrl+r toggle
	// owns the value for the rest of the session, so re-reading config would
	// fight it. A nil closure or a read error leaves the reply-now default.
	if env.Config != nil {
		if cfg, err := env.Config(); err == nil {
			a.shellQueue = cfg.TUI.ShellQueueDefault()
		}
	}
	// Markdown commands are a registry LAYER above the builtins (command.go's
	// CommandSource). This first load is synchronous BECAUSE it runs before
	// tea.NewProgram: there is no event loop to block and no frame to drop
	// yet, and loading eagerly means a command typed in the first keystrokes
	// resolves instead of racing the read. Every later refresh runs off the
	// loop — see App.loadUserCommandsCmd (usercmds.go) for the reload
	// contract.
	cmds, warns := usercmd.Load(a.commandEnv.Root, a.commandEnv.Cwd, a.userCommandOptions())
	a = a.applyUserCommands(userCommandsMsg{cmds: cmds, warns: warns})
	// `gofer attach <id>`: open straight into the session's attach screen and
	// pre-select it in the roster, so backing out with ← lands on it. The
	// subscription is kicked off in Init.
	if meta.AttachSessionID != "" {
		a.scr = screenAttach
		a.sessID = meta.AttachSessionID
		a.over.selectedID = meta.AttachSessionID
	}
	return a
}

// setStatus sets the transient status line and the severity it renders in
// together, so the two can never drift apart (issue #161: a note whose text
// says "set" and whose color says "failed" is worse than either alone). Every
// write to a.status goes through here.
func (a *App) setStatus(sev statusSeverity, note string) {
	a.status = note
	a.statusSev = sev
}

// clearStatus drops the transient status line, resetting the severity with it
// so a stale color can never outlive the note it described.
func (a *App) clearStatus() {
	a.status = ""
	a.statusSev = sevDanger
}

// rosterTickMsg fires [App.fetchRoster] again on the polling interval.
type rosterTickMsg struct{}

// rosterMsg carries the result of an [App.fetchRoster] call.
type rosterMsg struct {
	sessions []SessionInfo
	err      error
}

// subReadyMsg carries the result of subscribing to one session's event
// stream. id lets [App.Update] discard a stale resolve — one from a
// subscribe call issued for a session the user has since navigated away
// from.
type subReadyMsg struct {
	id  string
	sub *event.Subscription
	err error
}

// sessEventMsg carries one event read from a session's subscription. id
// guards the same staleness the way subReadyMsg does: a session can be
// switched away from while a blocking channel read is in flight.
type sessEventMsg struct {
	id  string
	ev  event.Event
	sub *event.Subscription
}

// sessClosedMsg reports a session's event.Subscription channel closing.
type sessClosedMsg struct{ id string }

// decisionSubReadyMsg carries the result of subscribing to one session's
// structured-decision stream — [subReadyMsg]'s twin for the second
// subscription an attach holds. id guards the same staleness, and for the
// same reason.
type decisionSubReadyMsg struct {
	id  string
	sub *decision.Subscription
	err error
}

// decisionMsg carries one update read from a session's decision
// subscription — [sessEventMsg]'s twin, carrying sub so [App.Update] can
// re-arm the read the same way.
type decisionMsg struct {
	id  string
	up  decision.Update
	sub *decision.Subscription
}

// decisionClosedMsg reports a session's decision.Subscription channel
// closing.
type decisionClosedMsg struct{ id string }

// createdMsg carries the result of [Supervisor.Create].
type createdMsg struct {
	info SessionInfo
	err  error
}

// resumedMsg carries the result of [Supervisor.Resume]: the session id that was
// asked for (so the attach below targets it — Resume returns no row) and the
// error, if any.
type resumedMsg struct {
	id  string
	err error
}

// opDoneMsg carries the error, if any, from a fire-and-forget Supervisor Op
// (Send/Interrupt/Kill/Archive).
type opDoneMsg struct{ err error }

// Init satisfies tea.Model: it kicks off the first roster fetch, plus — when
// the app opened straight into an attach (via OverviewMeta.AttachSessionID) —
// the subscription to that session so its transcript streams in immediately.
func (a App) Init() tea.Cmd {
	if a.sessID != "" {
		return tea.Batch(a.fetchRoster, a.subscribe(a.sessID))
	}
	return a.fetchRoster
}

// fetchRoster fetches a fresh roster snapshot from the Supervisor.
func (a App) fetchRoster() tea.Msg {
	sessions, err := a.sup.Roster(context.Background())
	return rosterMsg{sessions: sessions, err: err}
}

// subscribe subscribes to id's event stream via the Supervisor.
func (a App) subscribe(id string) tea.Cmd {
	return func() tea.Msg {
		sub, err := a.sup.Subscribe(context.Background(), id)
		return subReadyMsg{id: id, sub: sub, err: err}
	}
}

// waitForEvent blocks for the next event on sub, or reports the
// subscription closing.
func waitForEvent(id string, sub *event.Subscription) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-sub.C
		if !ok {
			return sessClosedMsg{id: id}
		}
		return sessEventMsg{id: id, ev: ev, sub: sub}
	}
}

// subscribeDecisions subscribes to id's open structured-decision requests via
// the Supervisor. It is armed off [subReadyMsg] rather than beside
// [App.subscribe] so both of an attach's streams are established from one
// place — and so a session that cannot be subscribed to at all never leaves a
// decision subscription dangling in a gate's subscriber set.
func (a App) subscribeDecisions(id string) tea.Cmd {
	return func() tea.Msg {
		sub, err := a.sup.Decisions(context.Background(), id)
		return decisionSubReadyMsg{id: id, sub: sub, err: err}
	}
}

// waitForDecision blocks for the next decision update on sub, or reports the
// subscription closing — [waitForEvent]'s twin for the decision stream.
func waitForDecision(id string, sub *decision.Subscription) tea.Cmd {
	return func() tea.Msg {
		up, ok := <-sub.C
		if !ok {
			return decisionClosedMsg{id: id}
		}
		return decisionMsg{id: id, up: up, sub: sub}
	}
}

// doCreate starts a new session via the Supervisor. The dispatch bar passes
// this client's cwd (a.cwd, the roster header's value) so the new session
// carries the client's project directory rather than defaulting to the
// daemon's launch dir — otherwise a TUI-created session is invisible to a
// client (e.g. a phone) filtering session/list by that cwd. A
// credential-driven model is still the default; per-session model overrides
// arrive with the config/`-m` wiring in a later milestone.
func (a App) doCreate(prompt string) tea.Cmd {
	return func() tea.Msg {
		info, err := a.sup.Create(context.Background(), prompt, CreateOptions{Cwd: a.cwd})
		return createdMsg{info: info, err: err}
	}
}

// doResume brings an on-disk session back under live supervision via the
// Supervisor, in cwd, and reports the outcome as a [resumedMsg] so Update can
// attach into it — the same create-then-attach shape [App.doCreate]/[createdMsg]
// have, since "resume" and "new" differ only in where the journal comes from.
func (a App) doResume(id, cwd string) tea.Cmd {
	return func() tea.Msg {
		err := a.sup.Resume(context.Background(), id, cwd)
		return resumedMsg{id: id, err: err}
	}
}

// doSend submits prompt as id's next turn via the Supervisor.
func (a App) doSend(id, prompt string) tea.Cmd {
	return func() tea.Msg {
		err := a.sup.Send(context.Background(), id, prompt)
		return opDoneMsg{err: err}
	}
}

// doInterrupt stops id's in-flight turn via the Supervisor.
func (a App) doInterrupt(id string) tea.Cmd {
	return func() tea.Msg {
		err := a.sup.Interrupt(context.Background(), id)
		return opDoneMsg{err: err}
	}
}

// doKill interrupts and terminates id via the Supervisor.
func (a App) doKill(id string) tea.Cmd {
	return func() tea.Msg {
		err := a.sup.Kill(context.Background(), id)
		return opDoneMsg{err: err}
	}
}

// doKillTree kills every id in order via the Supervisor — one Kill per
// session, the same Op ctrl-x issues for a single row, because "stop this
// session's agents" is not a new capability, just a fan-out of an existing
// one. Journals are never deleted (repo invariant #4): Kill interrupts and
// terminates, and that is all this does.
//
// A failing Kill does NOT abort the sweep: a bulk stop that gives up halfway
// leaves the operator with a partly-stopped tree and no way to tell which half.
// Every id is attempted and the FIRST error is what surfaces on the status
// line, which is the one an operator can act on (the later ones are usually
// the same cause repeated).
func (a App) doKillTree(ids []string) tea.Cmd {
	return func() tea.Msg {
		var firstErr error
		for _, id := range ids {
			if err := a.sup.Kill(context.Background(), id); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return opDoneMsg{err: firstErr}
	}
}

// doArchive drops id from the roster via the Supervisor.
func (a App) doArchive(id string) tea.Cmd {
	return func() tea.Msg {
		err := a.sup.Archive(context.Background(), id)
		return opDoneMsg{err: err}
	}
}

// switchSession points the peek/attach transcript at a different session: it
// closes the old subscription (so the broker stops buffering into a stream no
// one reads), resets sess to empty, and subscribes to id. Callers use it (via
// [App.enter]) whenever they navigate to a session other than the one already
// subscribed. Closing the old subscription unblocks its in-flight
// waitForEvent, which then reports sessClosedMsg for the now-former id and is
// ignored.
func (a *App) switchSession(id string) tea.Cmd {
	if a.sub != nil {
		a.sub.Close()
	}
	// The decision stream is torn down with the event stream it sits beside:
	// a gate publishes to every subscriber it holds, so a forgotten
	// subscription would keep a buffer alive (and, once full, count drops)
	// for a session nobody is watching.
	if a.decSub != nil {
		a.decSub.Close()
	}
	a.sessID = id
	a.sess = New(a.theme) // a fresh Model has no pending approval or decision either
	a.sub = nil
	a.decSub = nil
	a.scroll = 0 // a different session's transcript starts back at the tail
	return a.subscribe(id)
}

// ingestAttach applies ev to a.sess in place, honoring the tui.autoscroll
// setting (settings.go/config.TUI.AutoscrollEnabled — default true): with
// autoscroll enabled (the default), an event is ingested exactly like
// before this setting existed — a.scroll is left untouched, so at its
// default of 0 the transcript keeps tailing to the latest content, per
// [scrollTail]'s offset-0-is-the-tail contract. With autoscroll explicitly
// disabled, ev must not be allowed to silently pull an already-rendered
// view down toward the tail: this bumps a.scroll by however many transcript
// lines ev just added, so the window of content actually on screen — same
// start/end line indices into the (now longer) transcript — stays exactly
// where the operator left it. Gated to the attach screen only: peek and the
// overview never hold a live subscription this fires for. A pointer
// receiver (like switchSession/enter) because it must mutate a.scroll, not
// just a.sess.
func (a *App) ingestAttach(ev event.Event) {
	if a.scr != screenAttach || a.autoscrollEnabled() {
		a.sess = a.sess.Ingest(ev)
		return
	}
	before := len(a.sess.transcriptLines(a.width))
	a.sess = a.sess.Ingest(ev)
	if delta := len(a.sess.transcriptLines(a.width)) - before; delta > 0 {
		a.scroll += delta
	}
}

// autoscrollEnabled reports the effective tui.autoscroll setting
// (config.TUI.AutoscrollEnabled — default true), read directly off
// a.commandEnv.Config() on every call rather than cached: the same
// "always current, never a stale snapshot" contract every other CommandEnv
// read follows (see env.go's doc) — an edit from the /config panel, or from
// config.json changing under a different attached client, takes effect on
// the very next streamed event, not just the next process start. A nil
// Config closure (the zero CommandEnv — e.g. a test that doesn't wire one)
// or a read error both fall through to the same default: true.
func (a App) autoscrollEnabled() bool {
	if a.commandEnv.Config == nil {
		return true
	}
	cfg, err := a.commandEnv.Config()
	if err != nil {
		return true
	}
	return cfg.TUI.AutoscrollEnabled()
}

// mouseEnabled reports the effective tui.mouse setting
// (config.TUI.MouseEnabled — default true), read directly off
// a.commandEnv.Config() on every call, the same "always current" contract
// autoscrollEnabled follows. Gates both View's mouse-capture enable
// (tea.MouseModeCellMotion vs tea.MouseModeNone) and, defensively, every
// mouse-message case in Update — so even a message a misbehaving terminal
// sends despite MouseModeNone (or a synthetic one from a non-terminal
// client) is a no-op while mouse is configured off, not just uncaptured at
// the protocol level.
func (a App) mouseEnabled() bool {
	if a.commandEnv.Config == nil {
		return true
	}
	cfg, err := a.commandEnv.Config()
	if err != nil {
		return true
	}
	return cfg.TUI.MouseEnabled()
}

// approvalBodyLines reports the effective tui.approval_body_lines setting
// (config.TUI.ApprovalBodyLineLimit — default
// config.DefaultApprovalBodyLines), read off a.commandEnv.Config() on every
// call, the same "always current, never a stale snapshot" contract
// autoscrollEnabled/mouseEnabled/pasteLimitBytes follow. It caps the inline
// approval prompt's command-body rows (see renderApprovalPrompt). A nil
// Config closure or a read error both fall through to the default.
func (a App) approvalBodyLines() int {
	if a.commandEnv.Config == nil {
		return config.DefaultApprovalBodyLines
	}
	cfg, err := a.commandEnv.Config()
	if err != nil {
		return config.DefaultApprovalBodyLines
	}
	return cfg.TUI.ApprovalBodyLineLimit()
}

// approvalMinTranscriptRows reports the effective
// tui.approval_min_transcript_rows setting
// (config.TUI.ApprovalMinTranscriptRowFloor — default
// config.DefaultApprovalMinTranscriptRows), read off a.commandEnv.Config() on
// every call, the same "always current, never a stale snapshot" contract
// approvalBodyLines follows. It is the transcript budget the inline approval
// prompt collapses its rationale to protect (see [Model.promptLines]). A nil
// Config closure or a read error both fall through to the default.
func (a App) approvalMinTranscriptRows() int {
	if a.commandEnv.Config == nil {
		return config.DefaultApprovalMinTranscriptRows
	}
	cfg, err := a.commandEnv.Config()
	if err != nil {
		return config.DefaultApprovalMinTranscriptRows
	}
	return cfg.TUI.ApprovalMinTranscriptRowFloor()
}

// promptModel is a.sess with the approval prompt's two config knobs plumbed
// in from the always-current config read — the model BOTH row-arithmetic
// consumers must use.
//
// It exists because [App.render] and [App.transcriptRegion] compute the same
// footer length independently (render to lay the frame out, transcriptRegion
// to find which rows a mouse selection may paint), and both now depend on
// settings that change the prompt's height. Reading them in one place is what
// keeps the two in lockstep: if transcriptRegion measured a default-configured
// prompt while render drew a collapsed one, every click-drag highlight on the
// attach screen would land on the wrong rows.
func (a App) promptModel() Model {
	return a.sess.
		WithApprovalBodyLines(a.approvalBodyLines()).
		WithApprovalMinTranscriptRows(a.approvalMinTranscriptRows())
}

// attachModel is the FULLY composed attach model — the config-plumbed
// [App.promptModel] plus the render-local blocks the attach screen appends to
// the transcript (the background-agents roster fact, the `!`/`!!` shell runs,
// and the turn-in-flight thinking indicator) and the shell-mode queue label. It
// is the single definition both [App.render] (to draw the frame) and
// [App.transcriptRegion] (to measure which rows a mouse selection may paint) go
// through, so the two can never disagree about how many rows the transcript has:
// before this helper, render drew the shell/background blocks but
// transcriptRegion measured the bare a.sess without them, so those tail blocks
// rendered below the computed selectable region and could not be selected or
// copied. Measuring and drawing through one model is what closes that gap.
//
// WithThinking is appended LAST so the indicator sits below the shell/background
// blocks at the very tail, and so both consumers count the same extra row.
func (a App) attachModel() Model {
	return a.promptModel().
		WithBackgroundAgents(a.over.Children(a.sessID)).
		WithShellRuns(a.shellRuns).
		WithShellQueue(a.shellQueue).
		WithThinking()
}

// currentSessionInfo returns the roster snapshot for whichever session is
// currently peeked or attached, or nil on the overview — there is no active
// session for a command-panel view (e.g. /status's Session name/ID/Model
// rows) to describe. The roster keeps a.over.selectedID pointed at that
// session through peek/attach (see handleOverviewKey's → / Enter cases), so
// this is just a lookup, not new state.
func (a App) currentSessionInfo() *SessionInfo {
	if a.scr == screenOverview {
		return nil
	}
	if s, ok := a.over.Selected(); ok {
		return &s
	}
	return nil
}

// parentOf resolves the roster row that spawned id, for the attach screen's
// drill-out (← from a child returns to its parent — see handleAttachKey). It
// reports false for a root session, for a session the polled roster snapshot
// doesn't hold, and for one whose ParentID names a row that isn't on screen:
// all three are "there is no parent session to return to", and the caller falls
// back to the overview rather than navigating to a session it cannot render.
func (a App) parentOf(id string) (SessionInfo, bool) {
	s, ok := a.over.SessionByID(id)
	if !ok || s.ParentID == "" || s.ParentID == s.ID {
		return SessionInfo{}, false
	}
	return a.over.SessionByID(s.ParentID)
}

// enter ensures sess is subscribed to id, re-subscribing via
// [App.switchSession] only when id differs from the currently subscribed
// session (or no subscription is live yet). Peeking or attaching the
// already-subscribed session is a no-op — its transcript and subscription
// carry over unchanged.
func (a *App) enter(id string) tea.Cmd {
	if id == "" {
		return nil
	}
	if id == a.sessID && a.sub != nil {
		return nil
	}
	return a.switchSession(id)
}

// Update satisfies tea.Model.
func (a App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
		return a, nil

	case tea.MouseWheelMsg:
		if !a.mouseEnabled() {
			return a, nil
		}
		return a.handleWheel(msg), nil

	case tea.MouseClickMsg:
		if !a.mouseEnabled() {
			return a, nil
		}
		return a.handleMouseClick(msg), nil

	case tea.MouseMotionMsg:
		if !a.mouseEnabled() {
			return a, nil
		}
		return a.handleMouseMotion(msg), nil

	case tea.MouseReleaseMsg:
		if !a.mouseEnabled() {
			return a, nil
		}
		return a.handleMouseRelease(msg)

	case rosterMsg:
		if msg.err != nil {
			a.setStatus(sevDanger, msg.err.Error())
		} else {
			a.over = a.over.WithSessions(msg.sessions)
		}
		return a, tea.Tick(rosterInterval, func(time.Time) tea.Msg { return rosterTickMsg{} })

	case rosterTickMsg:
		return a, a.fetchRoster

	case subReadyMsg:
		if msg.id != a.sessID {
			return a, nil // user moved on before the subscribe resolved
		}
		if msg.err != nil {
			a.setStatus(sevDanger, msg.err.Error())
			return a, nil
		}
		a.sub = msg.sub
		// Arm the decision stream alongside the event stream — see
		// subscribeDecisions for why it hangs off this message rather than
		// off switchSession.
		return a, tea.Batch(waitForEvent(msg.id, msg.sub), a.subscribeDecisions(msg.id))

	case decisionSubReadyMsg:
		if msg.id != a.sessID {
			// The user moved on before the subscribe resolved. Close it rather
			// than drop it: an abandoned subscription stays in the gate's
			// subscriber set forever, and Gate.Request treats "has a
			// subscriber" as "a client can see this", so a leaked one would
			// let a question be asked that nobody is rendering.
			if msg.sub != nil {
				msg.sub.Close()
			}
			return a, nil
		}
		if msg.err != nil {
			a.setStatus(sevDanger, msg.err.Error())
			return a, nil
		}
		// Close whatever we already hold before adopting: two subscribes for
		// the same session can be in flight at once (Init subscribes, then an
		// enter before subReadyMsg lands subscribes again), and an overwritten
		// subscription would stay in the gate's subscriber set forever with its
		// waitForDecision goroutine parked on a channel nothing publishes to.
		// Worse than a plain leak: Gate.Request decides ErrNoClient from
		// "are there subscribers", so one orphan makes that fail-fast
		// permanently unreachable for this session — the same reason the stale
		// branch above closes rather than drops.
		if a.decSub != nil {
			a.decSub.Close()
		}
		a.decSub = msg.sub
		return a, waitForDecision(msg.id, msg.sub)

	case decisionMsg:
		if msg.id != a.sessID {
			return a, nil // stale: from a session we've since left, drop it
		}
		// Straight to the Model, not through ingestAttach: a decision update
		// adds no transcript lines, so there is no autoscroll accounting to do
		// (see Model.IngestDecision). Model.IngestDecision owns the
		// pending-decision bookkeeping — set on UpdateRequested, cleared on a
		// matching UpdateResolved — see decision.go.
		a.sess = a.sess.IngestDecision(msg.up)
		return a, waitForDecision(msg.id, msg.sub)

	case decisionClosedMsg:
		if msg.id == a.sessID {
			a.decSub = nil
		}
		return a, nil

	case sessEventMsg:
		if msg.id != a.sessID {
			return a, nil // stale: from a session we've since left, drop it
		}
		// Ingest owns the pending-approval bookkeeping (set on
		// PermissionRequested, cleared on a matching PermissionResolved) —
		// see Model.Ingest and approval.go.
		a.ingestAttach(msg.ev)
		return a, waitForEvent(msg.id, msg.sub)

	case sessClosedMsg:
		if msg.id == a.sessID {
			a.sub = nil
		}
		return a, nil

	case createdMsg:
		if msg.err != nil {
			a.setStatus(sevDanger, msg.err.Error())
			return a, nil
		}
		a.scr = screenAttach
		return a, a.switchSession(msg.info.ID)

	case resumedMsg:
		if msg.err != nil {
			a.setStatus(sevDanger, msg.err.Error())
			return a, nil
		}
		// Same landing as createdMsg: the session is live now, so show it.
		a.scr = screenAttach
		return a, a.switchSession(msg.id)

	case sessionsListedMsg:
		return a.applySessionsListed(msg), nil

	case opDoneMsg:
		if msg.err != nil {
			a.setStatus(sevDanger, msg.err.Error())
		}
		return a, nil

	case permissionExplainedMsg:
		// A ctrl+e explain landing (dialog.go). It never resolves or dismisses
		// the pending request — see applyPermissionExplained, which also drops
		// a result whose request is no longer the one on screen.
		return a.applyPermissionExplained(msg), nil

	case daemonDefaultProbedMsg:
		// The attached daemon's answer to "what is your default model NOW",
		// read back after a /model write (panel.go). It refreshes the header
		// and upgrades the hedged status note to a definitive one — the whole
		// point being that neither needs a restart to become true (issue #162).
		return a.applyDaemonDefault(msg), nil

	case shellDoneMsg:
		// A `!` / `!!` escape finishing (shell.go). On the attach screen the
		// run renders as a transcript block (Model.WithShellRuns) carrying its
		// exit code, note, and truncation marker, so a status line there would
		// only talk over what the reader is looking at — and a reply-now `!` run
		// additionally fires a turn immediately (see below). On a screen with no
		// transcript (the overview's dispatch bar, peek) there is nowhere for
		// the block to land, so post a one-line acknowledgement instead — a `!`
		// typed at the roster still needs to show it ran and where its output
		// went (see shellRun.shellRunStatus).
		a = a.applyShellDone(msg)
		if a.scr != screenAttach {
			if r, ok := a.shellRunBySeq(msg.seq); ok {
				a.setStatus(r.shellRunStatus())
			}
			return a, nil
		}
		// On the attach screen a reply-now `!` run (inContext && !queued) means
		// "go now": flush everything pending through composePrompt and fire a
		// turn so the agent replies immediately. A queued `!` run and any `!!`
		// run do NOT — the queued one waits for the user's next Enter (the
		// existing composePrompt-on-submit path), and a `!!` run is never sent
		// regardless (inContext gates it out here exactly as composePrompt does).
		// composePrompt sweeps ALL finished-unconsumed inContext runs, so a
		// queued run that finished earlier rides along on this flush — that is
		// what reply-now means. composePrompt is on its own statement because it
		// has a pointer receiver and marks the folded runs consumed; a statement
		// orders that mutation before doSend rather than resting on operand
		// evaluation order (matching the Enter handlers).
		if a.sessID != "" {
			if r, ok := a.shellRunBySeq(msg.seq); ok && r.inContext && !r.queued {
				prompt := a.composePrompt("")
				if prompt != "" {
					a.scroll = 0
					return a, a.doSend(a.sessID, prompt)
				}
			}
		}
		return a, nil

	case filesLoadedMsg:
		// The `@` mention's background cwd enumeration landing
		// (filemention.go). Resyncing the menu is what opens the popup — the
		// load was dispatched precisely because an `@` token was active.
		return a.applyFilesLoaded(msg)

	case modelsLoadedMsg:
		// The /model picker's background catalog load landing (panel.go). It
		// never touches a.status: a silent in-place list upgrade is the whole
		// design, and reporting it would talk over whatever note the user is
		// actually reading.
		return a.applyModelsLoaded(msg), nil

	case userCommandsMsg:
		// A markdown-command load landing off the loop (usercmds.go). The
		// popup that triggered it opened on the registry as it stood, so
		// re-sync it here — the new layer may add, drop, or re-summarize rows.
		//
		// syncMenu's Cmd is PROPAGATED, not discarded. It cannot be another
		// markdown reload — a.menuToken is already true whenever a load is in
		// flight, so the closed→open edge it gates on has passed — but it can
		// be the `@` half's cwd enumeration, and dropping THAT would strand
		// a.files.loading true forever ([App.syncFileCandidates] latches it
		// before returning the Cmd, and only a filesLoadedMsg clears it),
		// killing file mentions for the rest of the session. No caller of a
		// syncMenu that can reach the `@` branch may swallow its Cmd.
		return a.applyUserCommands(msg).syncMenu()

	case tea.PasteMsg:
		// Bracketed paste arrives as ONE message carrying the whole payload,
		// handled entirely outside the key handlers below so no character in
		// it can be read as a binding — see paste.go.
		return a.handlePaste(msg)

	case tea.KeyPressMsg:
		a.clearStatus()
		// Any key press clears an active/frozen mouse selection — docs/TUI.md's
		// "clear the selection on the next click / a key press" contract (a
		// fresh click already clears it via handleMouseClick installing a new
		// selectionState outright, so this is the key-press half).
		a.sel = nil
		// The command panel takes every key ahead of the approval overlay and
		// the per-screen handlers (dispatch precedence: panel > approval >
		// decision > menu > active screen > global) — see handlePanelKey.
		if a.panel != nil {
			return a.handlePanelKey(msg)
		}
		// Approval keys apply on the attach screen only — that is the sole
		// screen backed by a live session transcript (a.sess). Peek renders a
		// roster-only card with its own reply input and never subscribes, so a
		// stale a.sess approval from a prior attach must not hijack its keys.
		if a.scr == screenAttach && a.sess.HasPendingApproval() {
			return a.handleApprovalKey(msg)
		}
		// A pending structured decision commandeers the footer the same way,
		// one rung below the approval (see Model.promptLines for the
		// ordering's rationale) and scoped to the attach screen for the same
		// reason: it is the only screen backed by a live a.sess.
		if a.scr == screenAttach && a.sess.HasPendingDecision() {
			return a.handleDecisionKey(msg)
		}
		// The open command-autocomplete menu (command_menu.go) claims
		// ↓/↑/Tab/Enter/Esc ahead of the per-screen handlers, same overlay
		// precedence as the panel/approval checks above; any other key
		// (ordinary typing, backspace, ctrl-c) is the per-screen handler's as
		// usual.
		if a.menu.open() {
			if next, cmd, handled := a.handleMenuKey(msg); handled {
				return next, cmd
			}
		}
		next, cmd := a.handleKey(msg)
		if app, ok := next.(App); ok {
			// tea.Batch collapses to the single non-nil Cmd, so this is
			// exactly `cmd` on every key press except the two that open a
			// token: `/` dispatches the markdown-command reload, `@` the cwd
			// enumeration (see syncMenu).
			synced, syncCmd := app.syncMenu()
			return synced, tea.Batch(cmd, syncCmd)
		}
		return next, cmd
	}
	return a, nil
}

// handleWheel applies a mouse-wheel notch to a.scroll — the roster on the
// overview, or the header+transcript on attach (see [Overview.body]/
// [Model.view]); wheel-up scrolls back into history, wheel-down scrolls
// toward the tail, clamped at 0 (peek has no scrollable content of its own,
// and a command panel takes over the bottom of whichever screen is showing
// but doesn't stop the screen underneath it from scrolling, so this only
// gates on the screen, not a.panel). Content-length clamping happens at
// render time (scrollTail), so this never needs to know the actual
// viewport/content size.
func (a App) handleWheel(msg tea.MouseWheelMsg) App {
	if a.scr != screenOverview && a.scr != screenAttach {
		return a
	}
	switch msg.Mouse().Button {
	case tea.MouseWheelUp:
		a.scroll += scrollWheelLines
	case tea.MouseWheelDown:
		a.scroll -= scrollWheelLines
		if a.scroll < 0 {
			a.scroll = 0
		}
	}
	return a
}

// handleKey dispatches a key press: first the global keymap (keymap.go's
// table, which is where ctrl+c and ctrl+y are DEFINED as well as documented —
// each screen's switch used to carry its own ctrl+c copy), then the current
// screen's handler for everything else. The order matches what those copies
// had: ctrl+c was the first case in every screen's switch.
func (a App) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if next, cmd, handled := dispatchGlobalKey(a, msg.Key()); handled {
		return next, cmd
	}
	switch a.scr {
	case screenPeek:
		return a.handlePeekKey(msg)
	case screenAttach:
		return a.handleAttachKey(msg)
	default:
		return a.handleOverviewKey(msg)
	}
}

// handleOverviewKey handles key presses on the roster screen: navigation,
// the dispatch bar, and the kill/archive shortcut. The dispatch bar is
// always typeable.
func (a App) handleOverviewKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.Key()
	switch {
	case key.Code == tea.KeyUp:
		a.over = a.over.MoveUp()
		return a, nil

	case key.Code == tea.KeyDown:
		a.over = a.over.MoveDown()
		return a, nil

	case key.Code == tea.KeyTab:
		a.over = a.over.ToggleView()
		return a, nil

	case (key.Code == tea.KeySpace || key.Text == " ") && a.over.InputEmpty():
		// Peek the selected session: a roster-only card that does NOT
		// subscribe (enter opens the full, subscribed session). Conditional on
		// an EMPTY dispatch bar — with text, space is an ordinary character and
		// falls through to the shared input keymap below, exactly like the bare
		// "?" help key. Peek closes back here with space or esc.
		id := a.over.SelectedID()
		if id == "" {
			return a, nil
		}
		a.scr = screenPeek
		a.scroll = 0
		a.peekReply = ""
		return a, nil

	case key.Code == tea.KeyPgUp:
		a.scroll += scrollPageLines
		return a, nil

	case key.Code == tea.KeyPgDown:
		a.scroll -= scrollPageLines
		if a.scroll < 0 {
			a.scroll = 0
		}
		return a, nil

	case key.Code == tea.KeyEnter:
		if a.over.InputEmpty() {
			id := a.over.SelectedID()
			if id == "" {
				return a, nil
			}
			// Open the whole session: attach and subscribe now. `space` is the
			// lighter verb — it peeks the roster-only card that does NOT
			// subscribe (see the KeySpace case below).
			a.scr = screenAttach
			a.scroll = 0
			cmd := a.enter(id)
			return a, cmd
		}
		// A leading sigil is a command or a shell escape, not a prompt —
		// dispatch it instead of creating a session from the literal text.
		// [hasInputPrefix]/[App.dispatchInput] (shell.go) are the single
		// first-rune switch both this and the attach input route through, so
		// a prefix can never mean one thing here and another there. "@" is
		// deliberately absent: a file mention is part of a prompt, not a
		// replacement for one, so it is handled by the completion popup and
		// submitted as ordinary text.
		if hasInputPrefix(a.over.input.String()) {
			a.over = a.over.Submit()
			buf, _ := a.over.TakeSubmitted()
			return a.dispatchInput(buf)
		}
		a.over = a.over.Submit()
		var cmd tea.Cmd
		if txt, ok := a.over.TakeSubmitted(); ok {
			// composePrompt folds any pending `!` shell output in ahead of
			// the user's text — see shell.go for why `!!` cannot reach here.
			// On its own line, not inlined into the doCreate call: it mutates
			// a (marking the folded runs consumed), and a statement makes that
			// mutation ordered with respect to the call rather than resting on
			// operand-evaluation order.
			prompt := a.composePrompt(txt)
			cmd = a.doCreate(prompt)
		}
		return a, cmd

	case key.Code == tea.KeyEscape:
		// Clears the WHOLE buffer regardless of the cursor's position — a
		// repeated Backspace loop would only clear the text before the
		// cursor and could stall forever once the cursor (but not the
		// buffer) reaches 0.
		a.over = a.over.SetInput("")
		return a, nil

	case key.Mod.Contains(tea.ModCtrl) && key.Code == 'x':
		if s, ok := a.over.Selected(); ok {
			if s.Status == StatusFinished {
				return a, a.doArchive(s.ID)
			}
			return a, a.doKill(s.ID)
		}
		return a, nil

	case key.Mod.Contains(tea.ModCtrl) && key.Code == 't':
		// Bulk stop: kill every subagent BELOW the selected row, leaving the
		// selected session itself running — ctrl-x is still the way to stop one
		// session, including this one. The two read as a pair: ctrl-x kills the
		// row, ctrl-t stops what the row fanned out.
		//
		// The status note is the whole feedback channel here: the roster is a
		// polled snapshot, so the killed rows keep rendering as Working for up to
		// rosterInterval, and a bulk destructive key that looks like it did
		// nothing invites a second press.
		ids := a.over.Descendants(a.over.SelectedID())
		if len(ids) == 0 {
			a.setStatus(sevWarn, "No subagents under this session.")
			return a, nil
		}
		a.setStatus(sevOK, fmt.Sprintf("Stopping %s.", plural(len(ids), "subagent")))
		return a, a.doKillTree(ids)

	case key.Text == "?" && a.over.InputEmpty():
		// The roster footer has advertised "? shortcuts" since M2 with nothing
		// bound behind it; /help is what it was promising. Conditional on an
		// EMPTY dispatch bar, exactly like the bare → above: with text typed,
		// "?" is an ordinary character and falls through to the input keymap,
		// so it never interrupts a prompt mid-sentence.
		app, cmd := openPanel(panelHelp)(a, nil)
		return app, cmd
	}

	// Every key not already claimed by the navigation contract above falls
	// through to the shared input keymap (input_keymap.go) — movement,
	// insertion at the cursor, and deletion, the same keymap the attach
	// input uses. Bare Right is a plain cursor-move here (it reaches
	// applyInputKey's KeyRight case); space reaches applyInputKey only with
	// text in the bar — an empty bar's space peeks (the KeySpace case above).
	if buf, ok := applyInputKey(a.over.input, key); ok {
		a.over.input = buf
	}
	return a, nil
}

// handlePeekKey handles key presses on the peek card screen. up/down move the
// roster selection (the card follows; no subscription). The ❯ reply input owns
// text: enter with an empty reply opens/attaches the selected session, enter
// with text sends it as a reply (via the same Send path attach uses) and stays;
// esc closes peek back to the overview, and so does space with an empty reply
// (space is the toggle partner of the overview's space-to-peek); space with
// text types a space; ctrl+x deletes (kills a running session, archives a
// finished one); backspace edits the reply.
func (a App) handlePeekKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.Key()
	switch {
	case key.Code == tea.KeyEscape:
		// The universal back-out: esc always closes peek, whatever the reply
		// buffer holds (an in-progress reply is discarded — the overview's own
		// esc likewise clears its dispatch bar). space does the same, but only
		// with an empty reply, since it also types a space.
		a.scr = screenOverview
		a.scroll = 0
		return a, nil

	case key.Code == tea.KeyUp:
		a.over = a.over.MoveUp()
		return a, nil

	case key.Code == tea.KeyDown:
		a.over = a.over.MoveDown()
		return a, nil

	case key.Code == tea.KeyEnter:
		if a.peekReply == "" {
			// Open: attach the peeked session, subscribing now (peek did not).
			a.scr = screenAttach
			a.scroll = 0
			return a, a.enter(a.over.SelectedID())
		}
		id := a.over.SelectedID()
		text := a.peekReply
		a.peekReply = ""
		if id == "" {
			return a, nil
		}
		return a, a.doSend(id, text)

	case key.Code == tea.KeySpace || key.Text == " ":
		if a.peekReply == "" {
			a.scr = screenOverview
			a.scroll = 0
			return a, nil
		}
		a.peekReply += " "
		return a, nil

	case key.Mod.Contains(tea.ModCtrl) && key.Code == 'x':
		if s, ok := a.over.Selected(); ok {
			if s.Status == StatusFinished {
				return a, a.doArchive(s.ID)
			}
			return a, a.doKill(s.ID)
		}
		return a, nil

	case key.Code == tea.KeyBackspace:
		if a.peekReply != "" {
			r := []rune(a.peekReply)
			a.peekReply = string(r[:len(r)-1])
		}
		return a, nil

	case key.Text != "":
		a.peekReply += key.Text
		return a, nil
	}
	return a, nil
}

// handleAttachKey handles key presses on the full attach screen: typing and
// submitting the input, esc to interrupt, and — with an empty input — the two
// back-out keys: ← to this session's parent (or the overview when it has none)
// and ↓ to the overview with its first spawned child selected.
func (a App) handleAttachKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.Key()
	switch {
	case key.Code == tea.KeyEscape:
		if a.sessID != "" {
			return a, a.doInterrupt(a.sessID)
		}
		return a, nil

	case key.Code == tea.KeyLeft && key.Mod == 0:
		// Bare (unmodified) Left only — a modified Left (Alt+Left, the input
		// keymap's word-move) falls through to applyInputKey below like any
		// other editing key. ← in an EMPTY input backs out (the navigation
		// contract); with text, it edits — moves the cursor left one rune, the
		// same as everywhere else Left means "move left" — rather than the
		// pre-cursor no-op this case used to fall through to.
		//
		// Backing out of a SUBAGENT session lands on its PARENT's session, not
		// on the overview: drilling into a child (↑/↓ then enter) and pressing ←
		// walks back up the tree one level at a time, so a supervisor can read a
		// child's whole history and return to the context it came from — the
		// tree shows who is working, entering a node shows what they did (see
		// docs/TUI.md § "Subagent sessions"). A ROOT session — or a child whose
		// parent is absent from the polled roster snapshot, the same orphan case
		// [byTree] renders as a root — keeps returning to the overview exactly as
		// before.
		if a.sess.InputEmpty() {
			if parent, ok := a.parentOf(a.sessID); ok {
				// Keep the roster's selection in step with the drill-out, the way
				// every other navigation into a session does: the header, the
				// command panel's session views, and a subsequent ← all read
				// a.over's selection rather than a.sessID.
				a.over.selectedID = parent.ID
				a.scroll = 0
				return a, a.enter(parent.ID)
			}
			a.scr = screenOverview
			a.scroll = 0
			return a, nil
		}
		a.sess = a.sess.MoveLeft()
		return a, nil

	case key.Code == tea.KeyDown && key.Mod == 0 && a.sess.InputEmpty():
		// The other half of the drill-out pair, and the key the transcript's
		// background-agents block advertises: "N background agents launched (↓ to
		// manage)". ← goes UP the tree to the parent; children are managed on the
		// ROSTER (peek, attach, ctrl-x, ctrl-t all live there), so ↓ returns to
		// the overview with this session's FIRST child already selected — the
		// caption names a key that really does land on the agents it counted.
		//
		// The empty-input guard rides the case expression rather than the body,
		// unlike the bare-← case above: ← has an editing meaning of its own to
		// fall back on (move the cursor), ↓ has none, so a non-empty input must
		// leave the key to the shared input keymap below instead of claiming it
		// here and swallowing whatever that keymap grows to do with it.
		children := a.over.Children(a.sessID)
		if len(children) == 0 {
			// Nothing was ever advertised for a childless session (the block only
			// renders when there ARE children), so there is nothing to honor —
			// navigating anyway would be a surprise, not a shortcut.
			return a, nil
		}
		a.over.selectedID = children[0].ID
		a.scr = screenOverview
		a.scroll = 0
		return a, nil

	case key.Code == tea.KeyPgUp:
		a.scroll += scrollPageLines
		return a, nil

	case key.Code == tea.KeyPgDown:
		a.scroll -= scrollPageLines
		if a.scroll < 0 {
			a.scroll = 0
		}
		return a, nil

	case key.Code == tea.KeyEnter:
		// A leading sigil is a command or a shell escape, not a prompt — the
		// same [hasInputPrefix]/[App.dispatchInput] intercept the dispatch
		// bar uses (handleOverviewKey), applied here too so every prefix
		// behaves identically wherever it is typed.
		if hasInputPrefix(a.sess.input.String()) {
			a.sess = a.sess.Submit()
			buf, _ := a.sess.TakeSubmitted()
			return a.dispatchInput(buf)
		}
		a.sess = a.sess.Submit()
		var cmd tea.Cmd
		if txt, ok := a.sess.TakeSubmitted(); ok && a.sessID != "" {
			// Sending a prompt is exactly the moment a scrolled-back reader
			// wants to see the reply as it streams in — snap back to the tail.
			a.scroll = 0
			// composePrompt folds any pending `!` shell output in ahead of
			// the user's text — see shell.go for why `!!` cannot reach here,
			// and handleOverviewKey for why this is its own statement.
			prompt := a.composePrompt(txt)
			cmd = a.doSend(a.sessID, prompt)
		}
		return a, cmd
	}

	// Every key not already claimed by the navigation contract above falls
	// through to the shared input keymap (input_keymap.go) — the same
	// movement/insertion/deletion keymap the overview dispatch bar uses.
	// Bare Left never reaches here: it is already claimed by the tea.KeyLeft
	// case above (conditionally — back out to the overview, or edit — see
	// its own comment).
	if buf, ok := applyInputKey(a.sess.input, key); ok {
		a.sess.input = buf
	}
	return a, nil
}

// render builds the current screen's output as a plain string: this is the
// single point every screen (overview, peek, attach) flows through, so it is
// also where [layout.TopPadding] blank leading rows go — one insertion point
// rather than three. The content budget is shrunk by the same amount so the
// padded frame still totals a.height rows (status/input lines stay on
// screen instead of being pushed off the bottom). The trailing status line
// is included when set.
//
// The overview/attach screens each bottom-anchor their own input block
// (dispatch bar / input rule + menu + footer) within the h budget they're
// handed here — Overview.render pads its roster rows and Model.view pads
// its transcript with blank filler up to h before appending the pinned
// block, so the block always lands on h's last row and a short
// transcript/roster leaves blank rows ABOVE it (chat-style bottom
// anchoring) instead of trailing directly beneath the content. TopPadding
// above is unrelated — a fixed workaround for a terminal that clips the
// frame's very first row, not part of this bottom-anchoring math.
//
// frameLayout is the row-budget arithmetic render and [App.transcriptRegion]
// both need — the status-footer/command-panel/command-menu carve-out of
// a.height that every screen's own body renders within. Computed once behind
// this method so the two call sites can never drift apart (they used to be
// one copy of this math inline in render; transcriptRegion needs the exact
// same numbers to find the active screen's selectable rows within the frame
// render produces).
type frameLayout struct {
	h         int      // content budget handed to the active screen's own render
	footer    string   // trailing status line, "" when a.status is unset
	panelH    int      // command-panel row count, 0 when a.panel is nil
	menuLines []string // pre-rendered command-menu rows, nil when closed/not applicable
}

func (a App) frameLayout() frameLayout {
	h := a.height - layout.TopPadding

	var footer string
	if a.status != "" {
		footer = truncate(a.statusSev.style(a.theme).Render(a.status), a.width)
		h--
	}

	// A command panel takes a slice out of the bottom of the content budget —
	// sized to what the active tab actually renders ([commandPanel.Height]),
	// not always the worst-case max — the screen above it shrinks to fit, the
	// same way the status footer already does.
	panelH := 0
	if a.panel != nil {
		panelH = a.panel.Height(a.width)
		if panelH > h {
			panelH = h
		}
		h -= panelH
	}

	// The command-autocomplete menu (command_menu.go) is part of the
	// bottom-anchored block Overview/Model's *WithMenu variant composes
	// above the active screen's input rule — see the h-budget note below.
	// It only applies to the overview and attach screens — the two
	// text-entry surfaces the command-token grammar covers, see
	// App.syncMenu — and is mutually exclusive with the panel (a.menu is
	// always closed while a.panel != nil).
	var menuLines []string
	// Guard on h > 0: the first render happens before the terminal-size
	// message arrives (a.height == 0), so the content budget can be negative
	// after the padding/footer slices. With no room there is no menu to show,
	// and skipping avoids a menuLines[:h] slice with a negative bound (panic).
	if (a.scr == screenOverview || a.scr == screenAttach) && h > 0 {
		menuLines = a.menu.Lines(a.width)
		if len(menuLines) > h {
			menuLines = menuLines[:h]
		}
		// h is NOT reduced by len(menuLines) here: unlike the panel/footer
		// above (which sit outside the screen's own render budget),
		// Overview.render and Model.view already carve the menu's rows out
		// of the height they're handed — see their own bottom-block/filler
		// math — so subtracting it again here would double-count it and
		// shrink the bottom-anchored frame short of a.height.
	}

	return frameLayout{h: h, footer: footer, panelH: panelH, menuLines: menuLines}
}

// This is the pure core [App.View] wraps into a tea.View, kept separate so
// golden tests can assert on it directly without a bubbletea dependency.
func (a App) render() string {
	fl := a.frameLayout()

	var body string
	switch a.scr {
	case screenPeek:
		body = NewPeek(a.theme, a.over, a.peekReply).View(a.width, fl.h)
	case screenAttach:
		// attachHeaderLines is the same two-line "gofer v<version>" /
		// "<model> · <cwd>" identity chrome the overview's own header opens
		// with (see identityHeaderLines, overview_render.go) — the redesign's
		// global header, now topping the attach transcript, its approval
		// prompts, and its menu/panel overlays too, not just the overview.
		// Model.view joins it to the transcript as one scrollable region
		// (a.scroll), so it tails off the top for a long enough conversation
		// exactly like the oldest messages do.
		//
		// attachModel is the fully composed attach model: the config-plumbed
		// prompt model (tui.approval_body_lines / approval_min_transcript_rows,
		// read fresh every frame so a /config edit applies to the next render),
		// plus the two render-local transcript blocks — the background-agents
		// roster fact and the `!`/`!!` shell runs, each composed per frame off
		// live App state rather than ingested once and left to go stale — plus
		// the shell-mode queue label. render has a value receiver, so all of
		// this lands on a LOCAL copy and never touches a.sess. [App.transcriptRegion]
		// measures through the SAME helper so the frame it draws and the rows it
		// lets a selection paint can never drift apart — see attachModel's doc.
		body = a.attachModel().
			ViewWithMenu(a.width, fl.h, fl.menuLines, attachHeaderLines(a.theme, a.over.meta, a.width), a.scroll)
	default:
		// The dispatch bar's shell-mode rule labels the live reply-now/queue
		// mode; a.over is a local copy here (render's value receiver), so this
		// tracks a ctrl+r toggle without mutating App's own overview state.
		a.over.shellQueue = a.shellQueue
		body = a.over.ViewWithMenu(a.width, fl.h, fl.menuLines, a.scroll, a.panel != nil)
	}

	if a.panel != nil {
		body += "\n" + a.panel.View(a.width, fl.panelH)
	}

	if fl.footer != "" {
		body += "\n" + fl.footer
	}
	content := strings.Repeat("\n", layout.TopPadding) + body

	// An active/frozen mouse selection (mouse.go) overlays its reverse-video
	// highlight on top of the fully composed frame — after every other
	// overlay (panel/footer) so the highlight always draws over whatever it
	// covers, on whichever screen actually participates in selection. The
	// highlight is clamped to [App.transcriptRegion] — the active screen's
	// own scrollable content — so a drag that extends into the input box,
	// the usage/status footer, the identity header, or an open panel never
	// paints those rows (see transcriptRegion's doc for why: they sit
	// outside the row range highlightSelection is handed).
	if a.sel != nil && a.mouseSelectable() {
		top, bottom, ok := a.transcriptRegion()
		if ok {
			content = highlightSelection(content, *a.sel, a.theme, top, bottom)
		}
	}

	// A pending approval, when there is one, is already rendered inline by
	// a.sess.View/Peek.View above — the overview screen shows only the
	// roster's colored status word (see overview_render.go's statusColorFor), never the
	// prompt itself, even though a.sess can stay pointed at a pending
	// approval while backed out to it (so re-entering the same session's
	// peek/attach redisplays it).
	return content
}

// View satisfies tea.Model, rendering the current screen at the last known
// terminal size. It requests the alternate screen, matching [Program.View],
// and enables cell-motion mouse reporting when tui.mouse is on (the
// default; see mouseEnabled) — bubbletea v2 moved mouse mode from a
// tea.NewProgram option onto the View itself (see the upgrade guide's
// "Mouse mode is now a View field") — so a terminal that supports it starts
// sending [tea.MouseWheelMsg]/click/drag/release (see handleWheel and
// mouse.go) as soon as this frame draws; cmd/gofer's tui_app.go/attach.go
// build the [tea.Program] wrapping App and need no extra option for this.
// tui.mouse explicitly off sets tea.MouseModeNone instead, handing mouse
// reporting back to the terminal entirely — its own native click-to-select
// and scrollback return, and gofer's own wheel/selection handling goes
// quiet (also defensively gated in Update — see mouseEnabled's doc).
func (a App) View() tea.View {
	v := tea.NewView(a.render())
	v.AltScreen = true
	if a.mouseEnabled() {
		v.MouseMode = tea.MouseModeCellMotion
	} else {
		v.MouseMode = tea.MouseModeNone
	}
	return v
}

// attachHeaderLines renders the attach screen's identity header: the same
// title/context lines [Overview.header] shows (identityHeaderLines,
// overview_render.go), padded to headerLines rows with blank lines in place
// of the overview's status-count line — a global roster tally has no meaning
// once attached to one session. Model.view treats these as the top of the
// scrollable header+transcript region (see App.render's screenAttach case).
func attachHeaderLines(th theme.Theme, meta OverviewMeta, width int) []string {
	return pad(identityHeaderLines(th, meta, width), headerLines)
}
