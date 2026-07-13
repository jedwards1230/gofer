package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/tui/layout"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// InterruptMsg is reserved for the daemon-era interrupt Op: once gofer has a
// daemon to send it to, esc will publish this instead of quitting outright,
// and a caller wiring [Program] into a live session will send the
// corresponding interrupt Op on receiving it. M1 has no daemon, so esc quits
// the attach [tea.Program] directly (see handleKey) — driveTUI in
// cmd/gofer treats that quit as a cancellation of the in-flight run, the
// same as ctrl-c.
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
// forwarded session events on [EventMsg], and on key presses either edits
// the input buffer or quits the program (ctrl-c, esc — see handleKey).
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
		return p, tea.Quit

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
// terminal size. It requests the alternate screen so the live, height-clipped
// frames never touch the normal buffer; bubbletea exits the alt screen on
// quit, and driveTUI then flushes the full transcript to the scrollback (see
// [Program.FinalTranscript]).
//
// It prepends [layout.TopPadding] blank rows and shrinks the content height
// budget by the same amount, mirroring [App.render]'s accounting — some
// terminal emulators (observed on a macOS beta running fullscreen) clip the
// top row of the alt-screen frame, and this is the single-session TUI's
// render path (it renders [Model] directly, bypassing App.render, so it
// needs its own copy of the same padding).
func (p Program) View() tea.View {
	h := p.height - layout.TopPadding
	body := strings.Repeat("\n", layout.TopPadding) + p.inner.View(p.width, h)
	v := tea.NewView(body)
	v.AltScreen = true
	return v
}

// FinalTranscript renders the wrapped Model's full transcript at the last known
// terminal width, for flushing to the scrollback on exit. A caller running the
// program in the alternate screen (so the live, height-clipped frames leave no
// residue) prints this to stdout after [tea.Program.Run] returns, giving the
// user the whole conversation instead of the clipped final frame. Width
// defaults to 80 when no [tea.WindowSizeMsg] has been seen.
func (p Program) FinalTranscript() string {
	width := p.width
	if width < 1 {
		width = 80
	}
	return p.inner.FullTranscript(width)
}
