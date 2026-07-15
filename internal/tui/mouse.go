package tui

// mouse.go implements app-owned click-drag text selection and its OSC 52
// clipboard copy — the click/drag/release half of the mouse story
// (handleWheel, app.go, is the wheel half). Cell-motion mouse reporting
// (View's tea.MouseModeCellMotion) routes button clicks/drags to the
// program instead of the terminal's own native selection, so this
// reimplements selection in-app: track a screen-cell region from
// tea.MouseClickMsg through tea.MouseMotionMsg (while the left button stays
// held) to tea.MouseReleaseMsg, render it reverse-styled over whatever the
// frame shows (the overview roster or the attach transcript — selection
// operates on the fully composed screen App.render produces, not any one
// component's own content, since that's the only place both the header and
// scroll-adjusted body coexist as plain rows), and on release copy the
// selected text to the system clipboard via bubbletea's built-in OSC 52
// support (tea.SetClipboard — an OSC 52 "\x1b]52;c;<base64>\x07" sequence
// written straight to the program's output, no external clipboard
// dependency).

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// selectionState is the app-owned mouse selection: a screen-cell region from
// (startX,startY) through (curX,curY), both in the same absolute
// terminal-row/column coordinates tea.Mouse reports (0-based, top-left
// origin) and App.render's returned content already uses (it includes
// layout.TopPadding's leading blank rows, so no offset translation is
// needed between the two). dragging is true from the initiating click
// through release; it stays false afterward while the selection is still
// shown/copyable, until the next click (which always starts a fresh
// selectionState) or any key press (App.Update clears a.sel outright) — see
// docs/TUI.md's mouse/selection section.
type selectionState struct {
	dragging       bool
	startX, startY int
	curX, curY     int
}

// span normalizes the selection's start/current coordinates into
// reading-order (top-left, bottom-right) — a drag can move up-left as
// easily as down-right, so callers always want the pair in that order, not
// click-then-current chronological order.
func (s selectionState) span() (y0, x0, y1, x1 int) {
	y0, x0, y1, x1 = s.startY, s.startX, s.curY, s.curX
	if y1 < y0 || (y1 == y0 && x1 < x0) {
		return y1, x1, y0, x0
	}
	return y0, x0, y1, x1
}

// clampInt returns v clamped to [lo, hi].
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// mouseSelectable reports whether a's current screen participates in
// click-drag selection — the same overview+attach gate [App.handleWheel]
// uses (peek carries no scrollable content of its own; a command
// panel/menu/approval overlay composes OVER whichever screen is showing
// without stopping selection on the screen underneath it, matching how
// wheel scroll already behaves regardless of those overlays).
func (a App) mouseSelectable() bool {
	return a.scr == screenOverview || a.scr == screenAttach
}

// handleMouseClick starts a new selection at the clicked cell — a fresh
// click always overwrites any previous selection outright, satisfying
// "clear the selection on the next click" without any separate clear step.
// Only a plain left-button click starts one; a right/middle click, or a
// click while mouse capture wouldn't even be showing selectable content, is
// a no-op.
func (a App) handleMouseClick(msg tea.MouseClickMsg) App {
	if !a.mouseSelectable() {
		return a
	}
	m := msg.Mouse()
	if m.Button != tea.MouseLeft {
		return a
	}
	a.sel = &selectionState{dragging: true, startX: m.X, startY: m.Y, curX: m.X, curY: m.Y}
	return a
}

// handleMouseMotion extends an in-progress selection to the pointer's
// current cell. Cell-motion mouse mode (1002) only ever reports motion
// while a button is held, so every MouseMotionMsg this app receives is
// already mid-drag; the Button check further narrows it to the left button
// specifically, ignoring a right/middle-button drag while a selection is
// active.
func (a App) handleMouseMotion(msg tea.MouseMotionMsg) App {
	if a.sel == nil || !a.sel.dragging {
		return a
	}
	m := msg.Mouse()
	if m.Button != tea.MouseLeft {
		return a
	}
	sel := *a.sel
	sel.curX, sel.curY = m.X, m.Y
	a.sel = &sel
	return a
}

// handleMouseRelease ends the drag (the selection stays shown/copyable
// afterward — dragging flips to false, the region itself is untouched) and,
// when it covers real content, copies the selected text to the system
// clipboard via OSC 52 (tea.SetClipboard). No release-worthy selection in
// progress is a no-op with no Cmd.
func (a App) handleMouseRelease(msg tea.MouseReleaseMsg) (App, tea.Cmd) {
	if a.sel == nil || !a.sel.dragging {
		return a, nil
	}
	sel := *a.sel
	sel.dragging = false
	m := msg.Mouse()
	sel.curX, sel.curY = m.X, m.Y
	a.sel = &sel

	text := a.selectedText()
	if text == "" {
		return a, nil
	}
	return a, tea.SetClipboard(text)
}

// selectedText extracts the plain (ANSI-stripped) text a.sel currently
// covers from a.render()'s own output — the same fully composed frame the
// terminal actually shows, so the mapping automatically accounts for the
// active scroll offset and the identity header (both already baked into
// render()'s returned lines; there is no separate coordinate space to
// translate between). Multi-row spans take the clicked line from its start
// column to the line's end, every full line in between whole, and the
// release line from its own start to the release column — the standard
// terminal click-drag selection shape. "" when nothing is selected or the
// span covers no cells (e.g. a selection scrolled entirely out of the
// current frame).
func (a App) selectedText() string {
	if a.sel == nil {
		return ""
	}
	lines := strings.Split(ansi.Strip(a.render()), "\n")
	y0, x0, y1, x1 := a.sel.span()
	if y0 >= len(lines) || y1 < 0 {
		return ""
	}
	y0 = clampInt(y0, 0, len(lines)-1)
	y1 = clampInt(y1, 0, len(lines)-1)

	out := make([]string, 0, y1-y0+1)
	for y := y0; y <= y1; y++ {
		line := lines[y]
		width := ansi.StringWidth(line)
		left, right := 0, width
		if y == y0 {
			left = clampInt(x0, 0, width)
		}
		if y == y1 {
			right = clampInt(x1+1, 0, width) // the released-over cell is included
		}
		if left >= right {
			out = append(out, "")
			continue
		}
		out = append(out, ansi.Cut(line, left, right))
	}
	return strings.Join(out, "\n")
}

// highlightSelection overlays sel's span on content (App.render's output,
// already including layout.TopPadding) with th's reverse-video selection
// style, cutting each covered line into its unselected-before/
// selected/unselected-after runs via ansi.Cut (grapheme/ANSI-aware, so a
// colored line's existing styling around the selection survives untouched)
// and re-joining them. A span with no covered cells on a given line (e.g.
// every row outside [y0,y1]) leaves that line untouched.
func highlightSelection(content string, sel selectionState, th theme.Theme) string {
	lines := strings.Split(content, "\n")
	y0, x0, y1, x1 := sel.span()
	if y0 >= len(lines) || y1 < 0 {
		return content
	}
	y0 = clampInt(y0, 0, len(lines)-1)
	y1 = clampInt(y1, 0, len(lines)-1)

	style := th.SelectionStyle()
	for y := y0; y <= y1; y++ {
		line := lines[y]
		width := ansi.StringWidth(line)
		left, right := 0, width
		if y == y0 {
			left = clampInt(x0, 0, width)
		}
		if y == y1 {
			right = clampInt(x1+1, 0, width)
		}
		if left >= right {
			continue
		}
		lines[y] = ansi.Cut(line, 0, left) + style.Render(ansi.Cut(line, left, right)) + ansi.Cut(line, right, width)
	}
	return strings.Join(lines, "\n")
}
