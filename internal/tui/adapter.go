package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// InterruptMsg reports that the user pressed esc in the attach surface. A
// caller wiring [Program] into a live session should send the
// corresponding interrupt Op on receiving it; M1 has no daemon to send it
// to, so the message only ever reaches [tea.Program.Run]'s caller.
type InterruptMsg struct{}

// EventMsg wraps a session event.Event so it can ride the bubbletea message
// loop. A caller subscribes to a session's *event.Subscription and forwards
// each event.Event from sub.C into the running [tea.Program] via
// [tea.Program.Send](EventMsg{Event: e}); [Program.Update] unwraps it into
// [Model.Ingest].
type EventMsg struct{ Event event.Event }

// Program adapts [Model] to bubbletea's tea.Model interface. It is the only
// type in this package that imports bubbletea, so [Model] itself stays a
// plain value any test can drive without a terminal.
type Program struct {
	inner  Model
	width  int
	height int
}

// NewProgram returns a bubbletea-ready Program wrapping a fresh [Model]
// rendered through th.
func NewProgram(th theme.Theme) Program {
	return Program{inner: New(th)}
}

// Init satisfies tea.Model. The attach surface has nothing to do on start.
func (p Program) Init() tea.Cmd { return nil }

// Update satisfies tea.Model: it resizes on [tea.WindowSizeMsg], ingests
// forwarded session events on [EventMsg], and edits the input buffer or
// emits [InterruptMsg] on key presses.
func (p Program) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		p.width, p.height = msg.Width, msg.Height
		return p, nil

	case EventMsg:
		p.inner = p.inner.Ingest(msg.Event)
		return p, nil

	case tea.KeyPressMsg:
		return p.handleKey(msg)
	}
	return p, nil
}

// handleKey translates one key press into the corresponding edit on the
// pure Model, keeping all buffer-editing logic in Model itself (see
// TypeRune/Backspace/Submit) so it stays headlessly testable.
func (p Program) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.Key()
	switch {
	case key.Mod.Contains(tea.ModCtrl) && key.Code == 'c':
		return p, tea.Quit

	case key.Code == tea.KeyEscape:
		return p, func() tea.Msg { return InterruptMsg{} }

	case key.Code == tea.KeyEnter:
		p.inner = p.inner.Submit()
		return p, nil

	case key.Code == tea.KeyBackspace:
		p.inner = p.inner.Backspace()
		return p, nil

	case key.Text != "":
		for _, r := range key.Text {
			p.inner = p.inner.TypeRune(r)
		}
		return p, nil
	}
	return p, nil
}

// View satisfies tea.Model, rendering the wrapped Model at the last known
// terminal size.
func (p Program) View() tea.View {
	return tea.NewView(p.inner.View(p.width, p.height))
}
