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

	"github.com/jedwards1230/gofer/internal/tui/layout"
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

// transcriptRegion returns the inclusive [top, bottom] row range — in
// a.render's own absolute row coordinates, the same space a.sel's
// coordinates and highlightSelection/selectedText's line indices live in —
// that belongs to the active screen's own scrollable content: the attach
// transcript (plus whatever of its identity header is still scrolled into
// view) or the overview roster body. This is deliberately narrower than
// "every row render() produces": it excludes layout.TopPadding's leading
// blank rows, the command menu/panel, the trailing status/usage footer, and
// (attach only) the input box and its framing rules — selection and its
// highlight both clamp to exactly this range so a drag that runs off the
// bottom of the transcript into the input/footer, or off the top into a
// scrolled-away header, never paints or copies those rows. ok is false when
// there is no selectable row at all (e.g. a terminal too short to show any
// content, or a screen selection doesn't apply to).
func (a App) transcriptRegion() (top, bottom int, ok bool) {
	if !a.mouseSelectable() {
		return 0, 0, false
	}
	fl := a.frameLayout()

	switch a.scr {
	case screenOverview:
		// Overview.render's own row order (see overview_render.go): a fixed
		// headerLines rows, then exactly bodyAvail roster rows (padded to
		// that height, whether or not the roster fills it), then the
		// command menu (if open), then the dispatchH-row dispatch bar. The
		// roster body is the only slice that's this screen's "transcript".
		bodyAvail := fl.h - headerLines - dispatchH - len(fl.menuLines)
		if bodyAvail <= 0 {
			return 0, 0, false
		}
		top = layout.TopPadding + headerLines
		bottom = top + bodyAvail - 1
		return top, bottom, true

	case screenAttach:
		// Model.view (model.go) treats the identity header and the
		// transcript as ONE scrollable list, windowed to `avail` rows via
		// scrollTail — so the header is only sometimes present in the
		// window (short conversations keep it pinned at the top; a long
		// enough one scrolls it up and out, same as the oldest messages).
		// Reproduce that same windowing here to find exactly which window
		// rows are transcript (as opposed to header, or blank fill below a
		// short conversation) rather than any fixed offset.
		header := headerLines // attachHeaderLines always pads to this many rows
		transcript := len(a.sess.transcriptLines(a.width))

		var footerLen int
		if prompt := a.sess.promptLines(a.width); prompt != nil {
			footerLen = len(prompt)
		} else {
			footerLen = len(fl.menuLines) + 3 // rule, input line, rule
			if a.sess.statusLine() != "" {
				footerLen++
			}
		}
		avail := fl.h - footerLen
		if avail <= 0 {
			return 0, 0, false
		}

		total := header + transcript
		start := 0
		if total > avail {
			maxOffset := total - avail
			offset := clampInt(a.scroll, 0, maxOffset)
			end := total - offset
			start = end - avail
		}
		// Deliberately single-sided clamps, not clampInt: topRow can
		// legitimately land at avail (one past the last row) when the
		// header alone fills the whole window (nothing left for the
		// transcript) — that's what signals "empty" below, and clamping it
		// down to avail-1 would wrongly claim the header's own last row as
		// a transcript row.
		topRow := header - start
		if topRow < 0 {
			topRow = 0
		}
		bottomRow := total - 1 - start
		if bottomRow > avail-1 {
			bottomRow = avail - 1
		}
		if topRow > bottomRow {
			return 0, 0, false
		}
		top = layout.TopPadding + topRow
		bottom = layout.TopPadding + bottomRow
		return top, bottom, true

	default:
		return 0, 0, false
	}
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
// terminal click-drag selection shape. The span is clamped to
// [App.transcriptRegion] first, so a drag that runs into the input box, the
// usage/status footer, the identity header, or an open panel never copies
// those rows — only the transcript/roster content itself. "" when nothing is
// selected or the (region-clamped) span covers no cells (e.g. a selection
// scrolled entirely out of the current frame, or entirely outside the
// transcript region).
func (a App) selectedText() string {
	if a.sel == nil {
		return ""
	}
	top, bottom, ok := a.transcriptRegion()
	if !ok {
		return ""
	}
	lines := strings.Split(ansi.Strip(a.render()), "\n")
	spanY0, x0, spanY1, x1 := a.sel.span()

	// The loop range is [spanY0, spanY1] intersected with the transcript
	// region — never the input/footer/header rows outside it
	// (transcriptRegion's doc). This MUST be a one-sided max/min on each
	// bound, not a symmetric clamp of both spanY0 and spanY1 into
	// [top, bottom]: a symmetric clamp would pull a span that lies entirely
	// outside the region (e.g. a click-drag that starts and ends inside the
	// input box, below the region) onto the region's near edge as a false
	// single-row overlap, instead of correctly yielding no selection at all.
	// When intersecting moves the range's start/end row inward, that row is
	// no longer the drag's real click/release row, so its column bound below
	// (which only fires on y == spanY0 / y == spanY1, the UNCLAMPED span
	// edges) correctly falls through to the row's full width instead of the
	// click/release column — the row is fully inside the selection, the
	// drag just continued past it into content that got clamped away.
	y0 := max(spanY0, top)
	y1 := min(spanY1, bottom)
	if y0 > y1 || y0 >= len(lines) {
		return ""
	}
	y1 = clampInt(y1, 0, len(lines)-1)

	out := make([]string, 0, y1-y0+1)
	for y := y0; y <= y1; y++ {
		line := lines[y]
		width := ansi.StringWidth(line)
		left, right := 0, width
		if y == spanY0 {
			left = clampInt(x0, 0, width)
		}
		if y == spanY1 {
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
// colored line's existing styling around the selection survives untouched
// OUTSIDE the selected run) and re-joining them. The selected run itself is
// stripped of any ANSI it already carries (ansi.Strip) before th's style
// wraps it — transcript rows built from more than one styled sub-render
// (e.g. [markerLine]'s `style.Render(glyph) + " " + rest`) embed their own
// SGR reset right after the glyph, and reverse-video is itself just another
// SGR: wrapping the raw run in style.Render without stripping first nests
// that reset INSIDE the reverse wrap, so it terminates the reverse video
// (and anything else style.Render opened) partway through the run instead
// of at its end — the marker glyph reverses but the text after it renders
// unstyled, on any row whose text trails a styling boundary. Selection is a
// uniform highlight, not a syntax-preserving one, so losing whatever inner
// styling a run had (glyph color, muted body) in exchange for a solid,
// fully-reversed block — immune to embedded resets — is the correct
// tradeoff; the unselected before/after runs keep their original styling
// untouched, only the selected run itself is affected. A span with no
// covered cells on a given line (e.g.
// every row outside [y0,y1]) leaves that line untouched. regionTop/
// regionBottom (App.transcriptRegion, inclusive, in the same absolute row
// coordinates as content) clamp the painted rows to the active screen's own
// scrollable content — never the input box, the usage/status footer, the
// identity header, or an open panel/menu — the same clamp [App.selectedText]
// applies so the highlight and the copied text always agree on what's
// selected. Content outside [regionTop, regionBottom] is never painted, full
// stop, even when sel's span extends past it: same reasoning as
// selectedText's raw-span column bounds (see its comment) — a row inside the
// region that the clamped range still covers is fully painted, not bounded
// by a click/release column that landed outside the region.
func highlightSelection(content string, sel selectionState, th theme.Theme, regionTop, regionBottom int) string {
	lines := strings.Split(content, "\n")
	spanY0, x0, spanY1, x1 := sel.span()
	// One-sided max/min, matching selectedText's intersection — see its
	// comment for why a symmetric clamp of both bounds is wrong here (it
	// would turn a span entirely outside the region into a false overlap on
	// the region's near edge).
	y0 := max(spanY0, regionTop)
	y1 := min(spanY1, regionBottom)
	if y0 > y1 || y0 >= len(lines) {
		return content
	}
	y1 = clampInt(y1, 0, len(lines)-1)

	style := th.SelectionStyle()
	for y := y0; y <= y1; y++ {
		line := lines[y]
		width := ansi.StringWidth(line)
		left, right := 0, width
		if y == spanY0 {
			left = clampInt(x0, 0, width)
		}
		if y == spanY1 {
			right = clampInt(x1+1, 0, width)
		}
		if left >= right {
			continue
		}
		lines[y] = ansi.Cut(line, 0, left) + style.Render(ansi.Strip(ansi.Cut(line, left, right))) + ansi.Cut(line, right, width)
	}
	return strings.Join(lines, "\n")
}
