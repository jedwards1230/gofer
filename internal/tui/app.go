package tui

import (
	"context"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/tui/layout"
	"github.com/jedwards1230/gofer/internal/tui/theme"
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
	// active screen > global (see Update).
	panel *commandPanel

	// registry resolves a submitted "/name arg…" buffer's command token to
	// the [Command] that runs it.
	registry Registry

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

	// peekReply is the peek card's reply-input buffer. Peek carries no
	// transcript to own it, so the app root holds it and clears it on entering
	// peek, sending a reply, or leaving.
	peekReply string

	scr    screen
	width  int
	height int

	status string // transient error/status line, cleared on the next key press

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

// createdMsg carries the result of [Supervisor.Create].
type createdMsg struct {
	info SessionInfo
	err  error
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
	a.sessID = id
	a.sess = New(a.theme) // a fresh Model has no pending approval either
	a.sub = nil
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
		return a.handleWheel(msg), nil

	case rosterMsg:
		if msg.err != nil {
			a.status = msg.err.Error()
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
			a.status = msg.err.Error()
			return a, nil
		}
		a.sub = msg.sub
		return a, waitForEvent(msg.id, msg.sub)

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
			a.status = msg.err.Error()
			return a, nil
		}
		a.scr = screenAttach
		return a, a.switchSession(msg.info.ID)

	case opDoneMsg:
		if msg.err != nil {
			a.status = msg.err.Error()
		}
		return a, nil

	case tea.KeyPressMsg:
		a.status = ""
		// The command panel takes every key ahead of the approval overlay and
		// the per-screen handlers (dispatch precedence: panel > approval >
		// menu > active screen > global) — see handlePanelKey.
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
			return app.syncMenu(), cmd
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

// handleKey dispatches a key press to the current screen's handler.
func (a App) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
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
	case key.Mod.Contains(tea.ModCtrl) && key.Code == 'c':
		return a, tea.Quit

	case key.Code == tea.KeyUp:
		a.over = a.over.MoveUp()
		return a, nil

	case key.Code == tea.KeyDown:
		a.over = a.over.MoveDown()
		return a, nil

	case key.Code == tea.KeyTab:
		a.over = a.over.ToggleView()
		return a, nil

	case key.Code == tea.KeyRight && key.Mod == 0:
		// Bare (unmodified) Right only — a modified Right (Alt+Right, the
		// input keymap's word-move) is NOT the navigation contract's
		// attach-selected-session binding; it falls through to
		// applyInputKey below like any other editing key.
		id := a.over.SelectedID()
		if id == "" {
			return a, nil
		}
		a.scr = screenAttach
		a.scroll = 0
		cmd := a.enter(id)
		return a, cmd

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
			// Peek renders a roster-only card — it does not subscribe. The
			// subscription is established only if the user attaches from peek.
			a.scr = screenPeek
			a.scroll = 0
			a.peekReply = ""
			return a, nil
		}
		// A leading "/" is a command, not a prompt — dispatch it instead of
		// creating a session from the literal text. The intercept switches on
		// the first rune so "@" (file mention) / "!" (shell escape) can slot
		// in beside it later (docs/TUI.md); out of scope here.
		if strings.HasPrefix(a.over.input.String(), "/") {
			a.over = a.over.Submit()
			buf, _ := a.over.TakeSubmitted()
			return a.dispatchSlash(buf)
		}
		a.over = a.over.Submit()
		var cmd tea.Cmd
		if txt, ok := a.over.TakeSubmitted(); ok {
			cmd = a.doCreate(txt)
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
	}

	// Every key not already claimed by the navigation contract above falls
	// through to the shared input keymap (input_keymap.go) — movement,
	// insertion at the cursor, and deletion, the same keymap the attach
	// input uses. A bare Right never reaches here: it is already claimed by
	// the tea.KeyRight case above ("→ attaches the selected session" — see
	// applyInputKey's doc).
	if buf, ok := applyInputKey(a.over.input, key); ok {
		a.over.input = buf
	}
	return a, nil
}

// handlePeekKey handles key presses on the peek card screen. up/down move the
// roster selection (the card follows; no subscription). The ❯ reply input owns
// text: enter with an empty reply opens/attaches the selected session, enter
// with text sends it as a reply (via the same Send path attach uses) and stays;
// space with an empty reply closes peek back to the overview, space with text
// types a space; ctrl+x deletes (kills a running session, archives a finished
// one); backspace edits the reply.
func (a App) handlePeekKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.Key()
	switch {
	case key.Mod.Contains(tea.ModCtrl) && key.Code == 'c':
		return a, tea.Quit

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
// submitting the input, esc to interrupt, and ← to back out to the overview
// when the input is empty.
func (a App) handleAttachKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.Key()
	switch {
	case key.Mod.Contains(tea.ModCtrl) && key.Code == 'c':
		return a, tea.Quit

	case key.Code == tea.KeyEscape:
		if a.sessID != "" {
			return a, a.doInterrupt(a.sessID)
		}
		return a, nil

	case key.Code == tea.KeyLeft && key.Mod == 0:
		// Bare (unmodified) Left only — a modified Left (Alt+Left, the input
		// keymap's word-move) falls through to applyInputKey below like any
		// other editing key. ← in an EMPTY input backs out to the overview
		// (the navigation contract); with text, it edits — moves the cursor
		// left one rune, the same as everywhere else Left means "move left"
		// — rather than the pre-cursor no-op this case used to fall through
		// to.
		if a.sess.InputEmpty() {
			a.scr = screenOverview
			a.scroll = 0
			return a, nil
		}
		a.sess = a.sess.MoveLeft()
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
		// A leading "/" is a command, not a prompt — same intercept as the
		// dispatch bar (handleOverviewKey), applied here too so /status,
		// /config, and /model work from the attach input as well.
		if strings.HasPrefix(a.sess.input.String(), "/") {
			a.sess = a.sess.Submit()
			buf, _ := a.sess.TakeSubmitted()
			return a.dispatchSlash(buf)
		}
		a.sess = a.sess.Submit()
		var cmd tea.Cmd
		if txt, ok := a.sess.TakeSubmitted(); ok && a.sessID != "" {
			// Sending a prompt is exactly the moment a scrolled-back reader
			// wants to see the reply as it streams in — snap back to the tail.
			a.scroll = 0
			cmd = a.doSend(a.sessID, txt)
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
// This is the pure core [App.View] wraps into a tea.View, kept separate so
// golden tests can assert on it directly without a bubbletea dependency.
func (a App) render() string {
	h := a.height - layout.TopPadding

	var footer string
	if a.status != "" {
		footer = truncate(a.theme.DangerStyle().Render(a.status), a.width)
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

	var body string
	switch a.scr {
	case screenPeek:
		body = NewPeek(a.theme, a.over, a.peekReply).View(a.width, h)
	case screenAttach:
		// attachHeaderLines is the same two-line "gofer v<version>" /
		// "<model> · <cwd>" identity chrome the overview's own header opens
		// with (see identityHeaderLines, overview_render.go) — the redesign's
		// global header, now topping the attach transcript, its approval
		// prompts, and its menu/panel overlays too, not just the overview.
		// Model.view joins it to the transcript as one scrollable region
		// (a.scroll), so it tails off the top for a long enough conversation
		// exactly like the oldest messages do.
		body = a.sess.ViewWithMenu(a.width, h, menuLines, attachHeaderLines(a.theme, a.over.meta, a.width), a.scroll)
	default:
		body = a.over.ViewWithMenu(a.width, h, menuLines, a.scroll, a.panel != nil)
	}

	if a.panel != nil {
		body += "\n" + a.panel.View(a.width, panelH)
	}

	if footer != "" {
		body += "\n" + footer
	}
	content := strings.Repeat("\n", layout.TopPadding) + body

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
// and enables cell-motion mouse reporting — bubbletea v2 moved mouse mode
// from a tea.NewProgram option onto the View itself (see the upgrade guide's
// "Mouse mode is now a View field") — so a terminal that supports it starts
// sending [tea.MouseWheelMsg] (see handleWheel) as soon as this frame draws;
// cmd/gofer's tui_app.go/attach.go build the [tea.Program] wrapping App and
// need no extra option for this.
func (a App) View() tea.View {
	v := tea.NewView(a.render())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
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
