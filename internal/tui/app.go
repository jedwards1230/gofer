package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/event"

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

	sessID string // id `sess` is subscribed to ("" = none)
	sub    *event.Subscription

	scr    screen
	width  int
	height int

	status string // transient error/status line, cleared on the next key press
}

// NewApp returns an App rendering through th, driving sup, with its roster
// screen seeded from meta.
func NewApp(th theme.Theme, sup Supervisor, meta OverviewMeta) App {
	return App{
		theme: th,
		sup:   sup,
		over:  NewOverview(th, meta),
		sess:  New(th),
		scr:   screenOverview,
	}
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

// Init satisfies tea.Model: it kicks off the first roster fetch.
func (a App) Init() tea.Cmd {
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

// doCreate starts a new session via the Supervisor.
func (a App) doCreate(prompt string) tea.Cmd {
	return func() tea.Msg {
		info, err := a.sup.Create(context.Background(), prompt)
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
	a.sess = New(a.theme)
	a.sub = nil
	return a.subscribe(id)
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
			a.scr = screenPeek
			cmd := a.enter(id)
			return a, cmd
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

// handlePeekKey handles key presses on the read-only peek screen: j/k move
// the roster selection (re-subscribing the tail), →/enter attach the peeked
// session, ← backs out to the overview, and esc interrupts a running peeked
// session. Peek steals no other input.
func (a App) handlePeekKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.Key()
	switch {
	case key.Mod.Contains(tea.ModCtrl) && key.Code == 'c':
		return a, tea.Quit

	case key.Text == "j":
		a.over = a.over.MoveDown()
		cmd := a.enter(a.over.SelectedID())
		return a, cmd

	case key.Text == "k":
		a.over = a.over.MoveUp()
		cmd := a.enter(a.over.SelectedID())
		return a, cmd

	case key.Code == tea.KeyRight || key.Code == tea.KeyEnter:
		// The peeked session is already subscribed; attach reuses it as-is.
		a.scr = screenAttach
		return a, nil

	case key.Code == tea.KeyLeft:
		a.scr = screenOverview
		return a, nil

	case key.Code == tea.KeyEscape:
		if s, ok := a.over.Selected(); ok && s.Status == StatusWorking {
			return a, a.doInterrupt(s.ID)
		}
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

// render builds the current screen's output as a plain string, including
// the trailing status line when set. It is the pure core [App.View] wraps
// into a tea.View, kept separate so golden tests can assert on it directly
// without a bubbletea dependency.
func (a App) render() string {
	h := a.height

	var footer string
	if a.status != "" {
		footer = truncate(a.theme.DangerStyle().Render(a.status), a.width)
		h--
	}

	var body string
	switch a.scr {
	case screenPeek:
		body = NewPeek(a.theme, a.over, a.sess).View(a.width, h)
	case screenAttach:
		body = a.sess.View(a.width, h)
	default:
		body = a.over.View(a.width, h)
	}

	if footer != "" {
		body += "\n" + footer
	}
	return body
}

// View satisfies tea.Model, rendering the current screen at the last known
// terminal size. It requests the alternate screen, matching [Program.View].
func (a App) View() tea.View {
	v := tea.NewView(a.render())
	v.AltScreen = true
	return v
}
