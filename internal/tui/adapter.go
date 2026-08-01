package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/tui/layout"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// InterruptMsg is reserved for an interrupt Op: once this [Program] is wired
// to a live daemon session, esc will publish this instead of quitting
// outright, and a caller will send the corresponding interrupt Op on
// receiving it. This minimal in-process attach path has no such wiring, so
// esc quits the attach [tea.Program] directly, in ONE press (see handleKey) —
// driveTUI in cmd/gofer treats that quit as a cancellation of the in-flight
// run. ctrl-c reaches the same tea.Quit, but only on its SECOND press (see
// [Program.confirmQuit], gofer#314); esc staying single-press is deliberate,
// not an oversight this PR missed — esc is this minimal view's ONLY way to
// interrupt an in-flight turn, and there is no other screen or overlay to
// back out to first, so gating it behind a confirm would cost the one
// immediate cancel this surface has.
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

	// quitArmed is this Program's own ctrl+c double-tap arm state (gofer#314)
	// — [App.quitArmed]'s shape mirrored rather than shared, since Program is
	// architecturally a wholly separate tea.Model (see the type doc above) with
	// no App to call into. handleKey disarms it on every key that is not
	// itself ctrl+c, exactly as App.Update does for a.quitArmed, so a first
	// ctrl+c arms (see confirmQuit) and any OTHER key — never just a timeout —
	// cancels it.
	quitArmed bool
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
// the input buffer or quits the program (esc immediately, ctrl-c on its
// second press — see handleKey).
func (p Program) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		p.width, p.height = msg.Width, msg.Height
		return p, nil

	case EventMsg:
		p.inner.Ingest(msg.Event)
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
	isCtrlC := key.Mod.Contains(tea.ModCtrl) && key.Code == 'c'
	// Disarm on anything that is NOT ctrl+c, mirroring App.Update's
	// conditional clear of a.quitArmed (app.go) — see confirmQuit for the read
	// side and [Program.quitArmed]'s doc for why this is a separate copy
	// rather than a shared field.
	if !isCtrlC {
		p.quitArmed = false
	}
	switch {
	case isCtrlC:
		return p.confirmQuit()

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

// confirmQuit is the ctrl+c double-tap quit confirm (gofer#314) for this
// single-session view — [App.confirmQuit]'s shape mirrored, not shared (see
// [Program]'s type doc for why the two cannot share one method). The FIRST
// press arms: it sets p.quitArmed, which [Program.View] renders as
// [quitArmedNote] in place of App's status line — the same text, since the two
// implementations share the constant even though they render it through
// different paths. The SECOND press, while still armed, quits outright. Esc
// stays a single, un-confirmed quit throughout (see [InterruptMsg]'s doc for
// why that is deliberate), so this can never be the only way out.
func (p Program) confirmQuit() (tea.Model, tea.Cmd) {
	if p.quitArmed {
		return p, tea.Quit
	}
	p.quitArmed = true
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
//
// While p.quitArmed is set, one extra trailing line carries [quitArmedNote] —
// this Program's stand-in for App's status footer, which it has none of. The
// content budget shrinks by that one row (mirroring how App.frameLayout
// shrinks h for a.status) so the armed frame still totals p.height rows
// rather than overflowing it.
func (p Program) View() tea.View {
	h := p.height - layout.TopPadding
	var footer string
	if p.quitArmed {
		footer = "\n" + p.inner.theme.WarnStyle().Render(quitArmedNote)
		h--
	}
	body := strings.Repeat("\n", layout.TopPadding) + p.inner.View(p.width, h) + footer
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
