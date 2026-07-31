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
	"time"
	"unicode"

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

	// wordSelect is true when the selection was opened by a double-click: the
	// initial click snapped to a whole word (the ANCHOR, anchorY/anchorX0..
	// anchorX1 below), and a subsequent drag extends the selection
	// WORD-BY-WORD — the moving end snaps to whole-word boundaries so the
	// selection always covers complete words, like a native terminal/editor.
	// A plain (single-click) drag leaves this false and selects per-character.
	wordSelect bool
	anchorY    int // the double-clicked row
	anchorX0   int // the anchor word's start column (inclusive)
	anchorX1   int // the anchor word's end column (exclusive)
}

// doubleClickWindow is the maximum gap between two clicks on the same cell for
// the pair to count as a double-click (which promotes the selection to
// whole-word mode). bubbletea v2's Mouse event carries no click count, so the
// app reconstructs a double-click from click timing + position. 500ms is the
// common desktop default; a documented default rather than a bare literal so it
// can graduate to a config knob if a user ever needs to tune it.
const doubleClickWindow = 500 * time.Millisecond

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
		// Measure through the SAME fully composed model App.render draws from
		// (background-agents + shell-run blocks appended to the transcript), not
		// the bare a.sess: those tail blocks are real transcript rows, so
		// counting a.sess alone here would place the selectable region above
		// them and a drag over a `$ ls` shell block (or a background-agents
		// line) would select nothing. See [App.attachModel].
		am := a.attachModel()
		transcript := len(am.transcriptLines(a.width))

		// promptLines takes the same width AND height Model.view hands it
		// (fl.h), through the same composed model: the prompt collapses its
		// rationale when the frame is short, so measuring it at a different
		// height — or with a different configured floor — would give a footer
		// length the render never produced, and every highlight below it would
		// be off by the difference. (The shell/background blocks are transcript
		// items, not footer, so the footer length is unchanged by them —
		// measuring through the same model just keeps the two call sites reading
		// one source.)
		var footerLen int
		if prompt := am.promptLines(a.width, fl.h); prompt != nil {
			footerLen = len(prompt)
		} else {
			footerLen = 1 + len(fl.menuLines) + 3 // spacer, menu, then rule/input/rule
			if am.statusLine() != "" {
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

// selectableRegion returns the inclusive [top, bottom] row range selection and
// its highlight actually operate on: EVERY row the composed frame renders, in
// the same absolute row coordinates a.sel, highlightSelection, and
// selectedText already use.
//
// This is deliberately wider than [App.transcriptRegion]. Selection used to
// clamp to that narrower range — the active screen's own scrollable content —
// which meant the identity header (version, model, cwd, the awaiting/working/
// idle counts), the usage/status footer, the dispatch bar, and any open panel
// or menu could be neither highlighted nor copied. Every one of those rows is
// rendered text a user can read on screen, so every one of them is text they
// can reasonably expect to select and copy; excluding them made "select the
// screen" silently return a fragment (issue #307).
//
// [App.transcriptRegion] is left in place but now has NO non-test caller: it
// answers a real and different question (which rows are the active screen's
// own scrollable body) and its frame-layout arithmetic is covered by tests
// worth keeping, but nothing in the selection path consults it any more.
// Deleting it, or repointing those tests at [App.frameLayout] directly, is a
// deliberate follow-up rather than something to do silently in a selection
// change.
//
// It takes the frame's row count rather than re-rendering, so a caller that
// already holds the composed frame (all three of them do) never renders twice
// to ask which of its own rows are selectable.
//
// ok is false only when the frame has no rows at all.
func selectableRegion(lineCount int) (top, bottom int, ok bool) {
	if lineCount <= 0 {
		return 0, 0, false
	}
	return 0, lineCount - 1, true
}

// mouseSelectable reports whether a's current screen participates in
// click-drag selection. Every screen does: a screen that renders text renders
// text a user can want to copy, and the previous overview+attach-only gate
// refused peek outright (issue #307).
//
// Note what this gate did NOT cause, since #307 attributes it here: a command
// panel (/config, /status, /help) is an OVERLAY — `a.scr` stays screenOverview
// while it is open — so a panel always passed even the old gate. What made a
// panel unselectable was the region clamp, [App.transcriptRegion], whose doc
// excludes "the command menu/panel" in as many words; [selectableRegion]
// replaced it. TestSelectionOnAPanelAndPeekScreens pins one half against each,
// because a test covering only one goes green against the other regression.
//
// It stays a named predicate rather than being inlined away because the
// question it answers ("can this screen be selected?") is a real one that a
// future screen with genuinely nothing selectable could answer differently —
// and because two call sites (handleMouseClick and App.render's highlight
// overlay) must agree on the answer.
func (a App) mouseSelectable() bool {
	return true
}

// inputEmpty reports whether the text-entry surface of the current screen has
// no pending text — the overview's dispatch bar, or the peek/attach input.
// It is what gates ctrl+a between "select the whole screen" and the input
// keymap's "move to line start" (see [keyBinding.enabled]).
func (a App) inputEmpty() bool {
	if a.scr == screenOverview {
		return a.over.InputEmpty()
	}
	return a.sess.InputEmpty()
}

// selectAll selects every rendered row of the current frame and copies the
// result to the system clipboard, the keyboard counterpart to dragging over
// the whole screen. The selection it installs is an ordinary [selectionState],
// so it highlights like any other, is copyable again, and clears on the next
// key press or click exactly like a dragged one.
//
// The end column is the frame's widest row rather than that row's own width:
// [App.selectedText] clamps the end bound per row (`clampInt(x1+1, 0, width)`),
// so one over-wide value selects each row to its own end, whatever its length.
//
// No rows (an unrendered or zero-height frame) or a frame that normalizes to
// nothing is a no-op with no Cmd — never an empty clipboard write, which would
// silently destroy whatever the user had copied before.
func (a App) selectAll() (App, tea.Cmd) {
	lines := strings.Split(ansi.Strip(a.render()), "\n")
	top, bottom, ok := selectableRegion(len(lines))
	if !ok {
		return a, nil
	}
	widest := 0
	for _, line := range lines {
		widest = max(widest, ansi.StringWidth(line))
	}
	a.sel = &selectionState{startX: 0, startY: top, curX: widest, curY: bottom}

	text := a.selectedText()
	if text == "" {
		return a, nil
	}
	return a, tea.SetClipboard(text)
}

// copyTranscript copies the WHOLE transcript — every item, regardless of what
// the viewport currently shows — to the system clipboard.
//
// It exists because selection is viewport-only: [App.selectAll] spans the
// composed FRAME, so even selecting everything on screen cannot reach content
// that has scrolled off (gofer#312, from live use: "i can only select whats
// already on screen"). For the stated need — put a whole session on the
// clipboard — a dedicated action is strictly better than dragging through
// thousands of rows: deterministic, instant, and testable without a mouse.
//
// It reads [Model.transcriptLines], which renders every item and is already
// independent of scroll (it is what [App.ingestAttach] measures to hold the
// viewport still), so no new rendering plumbing is needed. That deliberately
// covers the transcript ONLY — not the identity header, input box, or usage
// footer. Those are frame chrome, not session content, and "copy the
// transcript" that silently included the input box would be a different
// action; ctrl+a remains the way to copy what is on screen INCLUDING chrome.
//
// Normalization is [normalizeCopy], unchanged and shared with select-all, so
// the two actions produce the same shape of text: every row right-trimmed, no
// leading or trailing blank rows, interior runs collapsed to one. That
// behavior was confirmed against a live roster by hand and is not re-litigated
// here.
//
// An empty transcript (or one that normalizes to nothing) is a no-op with no
// Cmd — never an empty clipboard write, which would silently destroy whatever
// the user had copied before. Same rule [App.selectAll] follows.
func (a App) copyTranscript() (App, tea.Cmd) {
	text := normalizeCopy(a.sess.transcriptLines(a.width))
	if text == "" {
		return a, nil
	}
	return a, tea.SetClipboard(text)
}

// handleMouseClick starts a new selection at the clicked cell — a fresh
// click always overwrites any previous selection outright, satisfying
// "clear the selection on the next click" without any separate clear step.
// Only a plain left-button click starts one; a right/middle click, or a
// click while mouse capture wouldn't even be showing selectable content, is
// a no-op.
// linkOpenMods are the modifier keys that turn a left click on a rendered link
// into an open-in-browser (rather than starting a selection): Ctrl / Alt / Cmd
// (Super) / Meta. Shift is deliberately excluded — it conventionally EXTENDS a
// selection. Requiring a modifier keeps a plain click/drag and a double-click
// purely selection, so link-open composes with both.
const linkOpenMods = tea.ModCtrl | tea.ModAlt | tea.ModSuper | tea.ModMeta

func (a App) handleMouseClick(msg tea.MouseClickMsg) (App, tea.Cmd) {
	if !a.mouseSelectable() {
		return a, nil
	}
	m := msg.Mouse()
	if m.Button != tea.MouseLeft {
		return a, nil
	}

	// A modifier+click on a rendered hyperlink opens it in the browser instead
	// of starting a selection (the URL is recovered from the OSC 8 the render
	// emitted). Terminals that honor OSC 8 under mouse capture handle this
	// themselves and never deliver the event here; this is the fallback for
	// those that don't.
	if m.Mod&linkOpenMods != 0 {
		if url, ok := linkAt(a.render(), m.X, m.Y); ok {
			return a, openURLCmd(url)
		}
	}

	// Double-click (two left clicks on the same cell within doubleClickWindow)
	// promotes the selection to whole-word mode: snap it to the word under the
	// cursor. bubbletea v2 reports no click count, so it is reconstructed from
	// the previous click's time + cell (see [isDoubleClick]).
	now := time.Now()
	double := isDoubleClick(a.lastClickAt, now, a.lastClickX, a.lastClickY, m.X, m.Y)
	a.lastClickAt, a.lastClickX, a.lastClickY = now, m.X, m.Y

	if double {
		if x0, x1, ok := a.wordBoundsAt(m.X, m.Y); ok {
			a.sel = &selectionState{
				dragging:   true,
				wordSelect: true,
				startX:     x0, startY: m.Y,
				curX: x1 - 1, curY: m.Y,
				anchorY: m.Y, anchorX0: x0, anchorX1: x1,
			}
			return a, nil
		}
	}

	a.sel = &selectionState{dragging: true, startX: m.X, startY: m.Y, curX: m.X, curY: m.Y}
	return a, nil
}

// isDoubleClick reports whether a click at (x,y) at time now is the second of a
// double-click, given the previous click at (px,py) at time prev: same cell,
// within [doubleClickWindow]. A zero prev time (no prior click) is never a
// double-click.
func isDoubleClick(prev, now time.Time, px, py, x, y int) bool {
	if prev.IsZero() {
		return false
	}
	return x == px && y == py && now.Sub(prev) <= doubleClickWindow
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
	if sel.wordSelect {
		// Word-by-word extension: snap the moving end to the word under the
		// cursor and keep the anchor word wholly covered, so a drag after a
		// double-click always spans complete words (native editor behavior).
		sel.extendWord(a, m.X, m.Y)
	} else {
		sel.curX, sel.curY = m.X, m.Y
	}
	a.sel = &sel
	return a
}

// extendWord grows a word-mode selection to include the whole word under the
// cursor (cx,cy) while always covering the anchor word. It picks whichever of
// the two outer corners lands the cursor's word on the correct side: dragging
// past the anchor extends the END to the cursor word's end; dragging before it
// extends the START to the cursor word's start; a cursor within the anchor word
// collapses back to the anchor alone.
func (s *selectionState) extendWord(a App, cx, cy int) {
	cs, ce, ok := a.wordBoundsAt(cx, cy)
	if !ok {
		// Off any selectable word (e.g. into the footer): fall back to the raw
		// cell so the drag still tracks, without corrupting the anchor.
		s.curX, s.curY = cx, cy
		return
	}
	// Is the cursor word before the anchor word in reading order?
	before := cy < s.anchorY || (cy == s.anchorY && ce <= s.anchorX0)
	if before {
		s.startX, s.startY = cs, cy
		s.curX, s.curY = s.anchorX1-1, s.anchorY
		return
	}
	s.startX, s.startY = s.anchorX0, s.anchorY
	s.curX, s.curY = ce-1, cy
}

// wordBoundsAt returns the [start, end) visible-CELL span of the word under
// cell (x,y) on the current frame, clamped to the transcript region. ok is
// false when (x,y) is outside the selectable region or past the row's content.
// It reads the same ANSI-stripped frame [App.selectedText] does, so the columns
// it returns line up with the cell columns the selection/highlight math uses.
func (a App) wordBoundsAt(x, y int) (start, end int, ok bool) {
	lines := strings.Split(ansi.Strip(a.render()), "\n")
	top, bottom, regionOK := selectableRegion(len(lines))
	if !regionOK || y < top || y > bottom {
		return 0, 0, false
	}
	return wordBoundsCells(lines[y], x)
}

// wordBoundsCells returns the [start, end) CELL span of the word at cell column
// x in the (plain, ANSI-free) line. A "word" is a maximal run of cells whose
// runes share the class of the rune at x — non-space (a token/URL/identifier)
// or whitespace — so a double-click on any glyph selects the contiguous token
// it belongs to. Cell-accurate (not rune-index) so a wide rune earlier in the
// line doesn't shift the bounds relative to the mouse column. ok is false when x
// is past the line's visible width.
func wordBoundsCells(line string, x int) (start, end int, ok bool) {
	runes := []rune(line)
	cellStart := make([]int, len(runes)+1)
	for i, r := range runes {
		cellStart[i+1] = cellStart[i] + ansi.StringWidth(string(r))
	}
	if x < 0 || x >= cellStart[len(runes)] {
		return 0, 0, false
	}
	// The rune index whose cell range [cellStart[ri], cellStart[ri+1]) covers x.
	ri := 0
	for ri < len(runes) && cellStart[ri+1] <= x {
		ri++
	}
	wordy := !unicode.IsSpace(runes[ri])
	lo, hi := ri, ri+1
	for lo > 0 && (!unicode.IsSpace(runes[lo-1]) == wordy) {
		lo--
	}
	for hi < len(runes) && (!unicode.IsSpace(runes[hi]) == wordy) {
		hi++
	}
	return cellStart[lo], cellStart[hi], true
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
// [selectableRegion] — every rendered row — so a drag that runs into the
// input box, the usage/status footer, the identity header, or an open panel
// copies those rows too (#307). The result is then run through
// [normalizeCopy], so the clipboard never receives the frame's right-hand
// padding or its runs of blank filler rows.
//
// "" when nothing is selected, the span covers no cells (e.g. a selection
// scrolled entirely out of the current frame), or everything it covers is
// whitespace.
func (a App) selectedText() string {
	if a.sel == nil {
		return ""
	}
	lines := strings.Split(ansi.Strip(a.render()), "\n")
	top, bottom, ok := selectableRegion(len(lines))
	if !ok {
		return ""
	}
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
	return normalizeCopy(out)
}

// normalizeCopy turns the raw per-row cuts of a selection into the text that
// actually reaches the clipboard: every row right-trimmed, no leading or
// trailing blank rows, and any interior run of blank rows collapsed to one.
//
// It exists because the frame is a fixed grid and the clipboard is not. Rows
// are padded out to the frame width to paint their background, and the screen
// below the roster is padded with blank rows to the terminal's height — so a
// selection copied verbatim carries a block of trailing spaces on every line
// plus a long tail of empty ones. That is invisible on screen and very visible
// the moment it is pasted anywhere.
//
// Interior blank rows collapse rather than vanish because they carry real
// structure in a transcript (the gap between two messages); a run of them is
// grid padding, but one of them is a paragraph break. Trimming happens on the
// COPY path only — [highlightSelection] still paints exactly the cells the
// drag covered, so what the user sees selected is unchanged.
func normalizeCopy(rows []string) string {
	trimmed := make([]string, 0, len(rows))
	blank := 0
	for _, row := range rows {
		row = strings.TrimRight(row, " \t")
		if row == "" {
			blank++
			continue
		}
		// Emit at most one blank row for the run that preceded this one, and
		// none at all if nothing has been emitted yet (leading blanks).
		if blank > 0 && len(trimmed) > 0 {
			trimmed = append(trimmed, "")
		}
		blank = 0
		trimmed = append(trimmed, row)
	}
	// A trailing run is deliberately dropped: `blank` is never flushed after
	// the loop.
	return strings.Join(trimmed, "\n")
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
// regionBottom ([selectableRegion], inclusive, in the same absolute row
// coordinates as content) bound the painted rows to the rows the frame
// actually has — the SAME range [App.selectedText] uses, which is the
// invariant that matters here: the highlight and the copied text must always
// agree on what is selected, so these two must never be handed different
// regions. Content outside [regionTop, regionBottom] is never painted, full
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
