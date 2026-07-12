package tui

import (
	"strings"

	"github.com/jedwards1230/gofer/internal/tui/layout"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// Peek is the read-only split screen: the roster rail plus a live tail of the
// selected session's transcript. It steals no input — j/k move the roster
// selection (the app root swaps the tail to the newly selected session). The
// two panes stack vertically (roster above the tail) by default and split
// side-by-side once the terminal is wide enough
// ([layout.PeekHorizontalMinWidth]).
type Peek struct {
	theme theme.Theme
	over  Overview // roster rail; its selection is the peeked session
	tail  Model    // read-only transcript of the selected session
}

// NewPeek returns a peek screen over the given roster rail and tail.
func NewPeek(th theme.Theme, over Overview, tail Model) Peek {
	return Peek{theme: th, over: over, tail: tail}
}

// WithOverview returns a copy with an updated roster rail — a moved selection
// or a refreshed roster snapshot.
func (p Peek) WithOverview(o Overview) Peek {
	p.over = o
	return p
}

// WithTail returns a copy showing tail as the peeked session's transcript. The
// app root calls this after re-subscribing to the newly selected session.
func (p Peek) WithTail(m Model) Peek {
	p.tail = m
	return p
}

// NextSession moves the roster selection to the next session (j). The caller
// re-subscribes the tail to [Peek.SelectedID].
func (p Peek) NextSession() Peek {
	p.over = p.over.MoveDown()
	return p
}

// PrevSession moves the roster selection to the previous session (k).
func (p Peek) PrevSession() Peek {
	p.over = p.over.MoveUp()
	return p
}

// SelectedID is the peeked session's id.
func (p Peek) SelectedID() string { return p.over.SelectedID() }

// View renders the split: the roster rail and the read-only tail, arranged by
// terminal width, above a one-line shortcut hint.
func (p Peek) View(width, height int) string {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}

	hint := truncate(p.theme.MutedStyle().Render(
		"j/k switch · →/enter attach · esc back · ctrl-c quit"), width)
	bodyH := height - 1
	if bodyH < 1 {
		bodyH = 1
	}

	var body string
	if layout.PeekOrientation(width) == layout.Horizontal {
		lw, rw := layout.SplitWidth(width)
		body = layout.JoinColumns(p.over.Rail(lw, bodyH), p.tail.TailView(rw, bodyH))
	} else {
		top, bottom := layout.SplitHeight(bodyH)
		divider := strings.Repeat("─", width)
		body = p.over.Rail(width, top) + "\n" + divider + "\n" + p.tail.TailView(width, bottom)
	}

	return body + "\n" + hint
}
