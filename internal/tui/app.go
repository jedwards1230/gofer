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
	return a.subscribe(id)
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
		a.sess = a.sess.Ingest(msg.ev)
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
		// active screen > global) — see handlePanelKey.
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
		return a.handleKey(msg)
	}
	return a, nil
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

	case key.Code == tea.KeyRight:
		id := a.over.SelectedID()
		if id == "" {
			return a, nil
		}
		a.scr = screenAttach
		cmd := a.enter(id)
		return a, cmd

	case key.Code == tea.KeyEnter:
		if a.over.InputEmpty() {
			id := a.over.SelectedID()
			if id == "" {
				return a, nil
			}
			// Peek renders a roster-only card — it does not subscribe. The
			// subscription is established only if the user attaches from peek.
			a.scr = screenPeek
			a.peekReply = ""
			return a, nil
		}
		// A leading "/" is a command, not a prompt — dispatch it instead of
		// creating a session from the literal text. The intercept switches on
		// the first rune so "@" (file mention) / "!" (shell escape) can slot
		// in beside it later (docs/TUI.md); out of scope here.
		if strings.HasPrefix(a.over.input, "/") {
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
		for !a.over.InputEmpty() {
			a.over = a.over.Backspace()
		}
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
		a.over = a.over.Backspace()
		return a, nil

	case key.Text != "":
		for _, r := range key.Text {
			a.over = a.over.TypeRune(r)
		}
		return a, nil
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

	case key.Code == tea.KeyLeft:
		if a.sess.InputEmpty() {
			a.scr = screenOverview
		}
		return a, nil

	case key.Code == tea.KeyEnter:
		// A leading "/" is a command, not a prompt — same intercept as the
		// dispatch bar (handleOverviewKey), applied here too so /status,
		// /config, and /model work from the attach input as well.
		if strings.HasPrefix(a.sess.input, "/") {
			a.sess = a.sess.Submit()
			buf, _ := a.sess.TakeSubmitted()
			return a.dispatchSlash(buf)
		}
		a.sess = a.sess.Submit()
		var cmd tea.Cmd
		if txt, ok := a.sess.TakeSubmitted(); ok && a.sessID != "" {
			cmd = a.doSend(a.sessID, txt)
		}
		return a, cmd

	case key.Code == tea.KeyBackspace:
		a.sess = a.sess.Backspace()
		return a, nil

	case key.Text != "":
		for _, r := range key.Text {
			a.sess = a.sess.TypeRune(r)
		}
		return a, nil
	}
	return a, nil
}

// render builds the current screen's output as a plain string: this is the
// single point every screen (overview, peek, attach) flows through, so it is
// also where [layout.TopPadding] blank leading rows go — one insertion point
// rather than three. The content budget is shrunk by the same amount so the
// padded frame still totals a.height rows (status/input lines stay on
// screen instead of being pushed off the bottom). The trailing status line
// is included when set. This is the pure core [App.View] wraps into a
// tea.View, kept separate so golden tests can assert on it directly without
// a bubbletea dependency.
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

	var body string
	switch a.scr {
	case screenPeek:
		body = NewPeek(a.theme, a.over, a.peekReply).View(a.width, h)
	case screenAttach:
		body = a.sess.View(a.width, h)
	default:
		body = a.over.View(a.width, h)
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
// terminal size. It requests the alternate screen, matching [Program.View].
func (a App) View() tea.View {
	v := tea.NewView(a.render())
	v.AltScreen = true
	return v
}
