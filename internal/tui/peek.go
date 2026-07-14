package tui

import (
	"strings"

	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// cardChrome is the fixed number of rows the peek card occupies below the
// roster rail: a rule, the selected-session title, its waiting line, the reply
// input, a closing rule, and the footer hint.
const cardChrome = 6

// Peek is the roster rail plus a summary CARD for the selected session: its
// title, a one-line waiting/status line, and a reply input. Unlike the earlier
// read-along transcript tail, peek carries no event stream — it is a pure
// projection of the roster snapshot plus the in-progress reply buffer, so its
// selection can move without re-subscribing. The reply routes to the same
// Supervisor.Send path attach uses; the app root owns the buffer text and
// passes it in.
type Peek struct {
	theme theme.Theme
	over  Overview // roster rail; its selection is the peeked session
	reply string   // the in-progress reply-buffer text, rendered in the ❯ input
}

// NewPeek returns a peek screen over the given roster rail, rendering reply as
// the ❯ input's live buffer.
func NewPeek(th theme.Theme, over Overview, reply string) Peek {
	return Peek{theme: th, over: over, reply: reply}
}

// WithOverview returns a copy with an updated roster rail — a moved selection
// or a refreshed roster snapshot.
func (p Peek) WithOverview(o Overview) Peek {
	p.over = o
	return p
}

// NextSession moves the roster selection to the next session; the card follows.
func (p Peek) NextSession() Peek {
	p.over = p.over.MoveDown()
	return p
}

// PrevSession moves the roster selection to the previous session.
func (p Peek) PrevSession() Peek {
	p.over = p.over.MoveUp()
	return p
}

// SelectedID is the peeked session's id.
func (p Peek) SelectedID() string { return p.over.SelectedID() }

// View renders the roster rail above a summary card for the selected session:
// a rule, the session title, a waiting/status line, the ❯ reply input, a
// closing rule, and the footer hint. The card is a fixed-height band pinned to
// the bottom; the rail fills the rest.
func (p Peek) View(width, height int) string {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}

	railH := height - cardChrome
	if railH < 0 {
		railH = 0
	}

	rule := strings.Repeat("─", width)

	var title, waiting string
	if s, ok := p.over.Selected(); ok {
		title = truncate(s.Title, width)
		verb := statusVerb(effectiveStatus(s))
		dur := humanDuration(p.over.meta.Now.Sub(s.Updated))
		waiting = "  " + p.theme.MutedStyle().Render(verb+" "+dur)
	}

	var replyLine string
	if p.reply == "" {
		replyLine = "❯ " + p.theme.MutedStyle().Render("reply")
	} else {
		replyLine = "❯ " + p.reply + "▏"
	}

	footer := truncate(p.theme.MutedStyle().Render(
		"enter to open · space to close · ctrl+x to delete"), width)

	out := strings.Split(p.over.Rail(width, railH), "\n")
	out = append(out, rule, title, truncate(waiting, width), truncate(replyLine, width), rule, footer)

	// Clip defensively so the card never overruns its allotted height — the
	// same end-clip [Overview.View] uses, so a too-short terminal loses the
	// bottom of the card (footer/rule first) rather than the roster rail.
	if len(out) > height {
		out = out[:height]
	}
	return strings.Join(out, "\n")
}

// statusVerb is the peek card's waiting-line verb for an effective status:
// "waiting" while it needs input, "working" while a turn is in flight,
// "finished" once terminal.
func statusVerb(st SessionStatus) string {
	switch st {
	case StatusWorking:
		return "working"
	case StatusFinished:
		return "finished"
	default:
		return "waiting"
	}
}
