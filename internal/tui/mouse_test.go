package tui

// mouse_test.go covers app-owned click-drag text selection (mouse.go)
// against App's internal state: the cell→text mapping (including a scroll
// offset and the identity header, both baked into App.render's own output),
// selectionState.span's reading-order normalization, and highlightSelection.
// The OSC 52 clipboard byte sequence, captured off a real tea.Program (like
// the existing mouse-enable test), lives in mouse_runtime_test.go
// (package tui_test) alongside it.

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// TestSelectionSpanNormalizesReadingOrder covers span()'s reading-order
// normalization: a drag that moves up-left of its start still returns
// (top-left, bottom-right), not click-then-current chronological order.
func TestSelectionSpanNormalizesReadingOrder(t *testing.T) {
	cases := []struct {
		name           string
		sel            selectionState
		wantY0, wantX0 int
		wantY1, wantX1 int
	}{
		{"forward drag (down-right) is already in order", selectionState{startX: 2, startY: 1, curX: 8, curY: 3}, 1, 2, 3, 8},
		{"same-row drag right", selectionState{startX: 2, startY: 1, curX: 8, curY: 1}, 1, 2, 1, 8},
		{"same-row drag left needs swapping", selectionState{startX: 8, startY: 1, curX: 2, curY: 1}, 1, 2, 1, 8},
		{"backward drag (up-left) needs swapping", selectionState{startX: 8, startY: 3, curX: 2, curY: 1}, 1, 2, 3, 8},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			y0, x0, y1, x1 := tc.sel.span()
			if y0 != tc.wantY0 || x0 != tc.wantX0 || y1 != tc.wantY1 || x1 != tc.wantX1 {
				t.Errorf("span() = (y0=%d x0=%d y1=%d x1=%d), want (y0=%d x0=%d y1=%d x1=%d)",
					y0, x0, y1, x1, tc.wantY0, tc.wantX0, tc.wantY1, tc.wantX1)
			}
		})
	}
}

// TestSelectedTextSingleLine covers a plain same-row selection: the
// substring between the clicked and released columns, inclusive of the
// released-over cell. The row selected is inside the roster body (the
// overview screen's transcript-region equivalent — see
// TestSelectionHighlightAndCopyExcludeChrome for why a header row wouldn't
// even be selectable).
func TestSelectedTextSingleLine(t *testing.T) {
	a := newAppForGolden(t, newInternalFakeSup(GoldenRoster()))
	// Row 6 of the rendered content (testdata/app_overview.golden, 0-indexed)
	// is "▸ wire the app root …" — the first roster row, inside the roster
	// body. Columns 0-1 are the caret+space prefix; "wire" spans columns 2-5.
	a.sel = &selectionState{startX: 2, startY: 6, curX: 5, curY: 6}
	if got := a.selectedText(); got != "wire" {
		t.Errorf("selectedText() = %q, want %q", got, "wire")
	}
}

// TestSelectedTextWithScrollOffsetAndHeader is the required cell→text
// mapping test: it builds an attach transcript long enough to overflow the
// viewport (so the header is scrolled away at the tail), scrolls all the
// way back (bringing the header AND the transcript's earliest content back
// into view together — the exact shape a real scrolled-back selection
// covers), locates the now-visible "turn 0" line, and selects exactly that
// item's text — proving selectedText() reads through App.render()'s own
// scroll-adjusted, header-prefixed output rather than some separate
// unscrolled coordinate space.
func TestSelectedTextWithScrollOffsetAndHeader(t *testing.T) {
	meta := GoldenMeta()
	meta.AttachSessionID = "sess-x"
	a := NewApp(theme.Test(), &internalFakeSup{}, meta, GoldenCommandEnv())
	mdl, _ := a.Update(tea.WindowSizeMsg{Width: testkit.Width, Height: testkit.Height})
	a = mdl.(App)

	const turns = 40
	for i := 0; i < turns; i++ {
		mdl, _ = a.Update(sessEventMsg{
			id: "sess-x",
			ev: event.NewMessageFinished("sess-x", event.MessageUser, fmt.Sprintf("turn %d", i)),
		})
		a = mdl.(App)
	}

	// Scroll fully back — scrollTail clamps an oversized offset to the
	// content's start, so this reliably lands on the earliest content
	// (the header, then "turn 0") regardless of exactly how much overflowed.
	a.scroll = 1_000_000
	const wantHeader = "gofer v0.3.0"
	rendered := a.render()
	if !strings.Contains(rendered, wantHeader) {
		t.Fatalf("precondition failed: fully scrolled-back render is missing the header:\n%s", rendered)
	}

	lines := strings.Split(rendered, "\n")
	const wantLine = "○ turn 0" // itemUser's marker + the exact text (GlyphHuman, model.go)
	row := -1
	for i, l := range lines {
		if l == wantLine {
			row = i
			break
		}
	}
	if row < 0 {
		t.Fatalf("precondition failed: %q not found in the fully scrolled-back render:\n%s", wantLine, rendered)
	}

	// "○ turn 0": the glyph + space occupy columns 0-1, "turn 0" spans
	// columns 2-7 inclusive.
	a.sel = &selectionState{startX: 2, startY: row, curX: 7, curY: row}
	if got := a.selectedText(); got != "turn 0" {
		t.Errorf("selectedText() with a scroll offset + header present = %q, want %q\n(row %d of):\n%s", got, "turn 0", row, rendered)
	}
}

// TestSelectionHighlightAndCopyExcludeChrome reproduces the reported bug:
// on the attach screen with an overflowing (tailed) transcript, a
// click-drag that starts inside the transcript and extends DOWN past the
// transcript's own bottom edge into the input box and its framing rules
// used to paint a full-width reverse-video bar over those rows too
// (highlightSelection/selectedText operated on App.render's ENTIRE frame,
// clamped only to [0, len(lines)-1] — never to the transcript region) and
// copy their text into the clipboard on release. Both must now be clamped to
// [App.transcriptRegion]: the highlight never touches the input/rule rows
// below the transcript, and both selectedText and the OSC 52 clipboard
// payload handleMouseRelease produces carry only the transcript's own text.
//
// This test fails against the pre-fix highlightSelection/selectedText (no
// region clamp, whole-frame [0, len(lines)-1] bound only): the input row
// would carry the reverse-video SGR and selectedText/the clipboard payload
// would include the input line's "> " prompt text.
func TestSelectionHighlightAndCopyExcludeChrome(t *testing.T) {
	meta := GoldenMeta()
	meta.AttachSessionID = "sess-x"
	a := NewApp(testkit.ColorTheme(), &internalFakeSup{}, meta, GoldenCommandEnv())
	mdl, _ := a.Update(tea.WindowSizeMsg{Width: testkit.Width, Height: testkit.Height})
	a = mdl.(App)

	// Overflow the transcript (scrolled/tailed) — the exact shape the bug
	// report showed, not the single-turn golden.
	const turns = 40
	for i := 0; i < turns; i++ {
		mdl, _ = a.Update(sessEventMsg{
			id: "sess-x",
			ev: event.NewMessageFinished("sess-x", event.MessageUser, fmt.Sprintf("turn %d", i)),
		})
		a = mdl.(App)
	}

	// Locate the input line and the transcript's own last real content row
	// by scanning the rendered frame directly — not through
	// [App.transcriptRegion] itself, so this precondition (and the repro
	// below) doesn't depend on the very code path under test.
	rendered := a.render()
	lines := strings.Split(rendered, "\n")
	inputRow, lastTranscriptRow := -1, -1
	for i, l := range lines {
		plain := ansi.Strip(l) // testkit.ColorTheme() styles the marker glyph, breaking a raw-string prefix match
		if strings.HasPrefix(plain, "> ") {
			inputRow = i
		}
		if strings.HasPrefix(plain, "○ turn ") {
			lastTranscriptRow = i // the loop's final match is the tailmost one
		}
	}
	if inputRow < 0 {
		t.Fatalf("precondition failed: no input line (\"> \") found in the rendered frame:\n%s", rendered)
	}
	if lastTranscriptRow < 0 {
		t.Fatalf("precondition failed: no transcript row (\"○ turn N\") found in the rendered frame:\n%s", rendered)
	}
	if lastTranscriptRow >= inputRow {
		t.Fatalf("precondition failed: expected the transcript's last row (%d) above the input line (%d)", lastTranscriptRow, inputRow)
	}
	// The rule row directly below the input line — Model.view's footer is
	// rule, input, rule (model.go) — must also stay untouched.
	ruleRow := inputRow + 1
	if ruleRow >= len(lines) || !strings.Contains(lines[ruleRow], "─") {
		t.Fatalf("precondition failed: row %d isn't the input box's closing rule, got %q", ruleRow, lines[ruleRow])
	}

	// Drag from column 2 of the transcript's own last (bottommost,
	// most-recent) row, past the transcript entirely, down through the
	// input line and its closing rule — same shape as the bug screenshot (a
	// drag that runs off the transcript into the input row while still in
	// progress).
	a.sel = &selectionState{dragging: true, startX: 2, startY: lastTranscriptRow, curX: 5, curY: ruleRow}

	highlighted := a.render()
	hLines := strings.Split(highlighted, "\n")
	const reverseOn = "\x1b[7m"
	if !strings.Contains(hLines[lastTranscriptRow], reverseOn) {
		t.Errorf("highlightSelection did not paint the transcript's own last row (%d): %q", lastTranscriptRow, hLines[lastTranscriptRow])
	}
	if strings.Contains(hLines[ruleRow], reverseOn) {
		t.Errorf("highlightSelection painted the input box's rule row (%d), outside the transcript region: %q", ruleRow, hLines[ruleRow])
	}
	if strings.Contains(hLines[inputRow], reverseOn) {
		t.Errorf("highlightSelection painted the input line (%d), outside the transcript region: %q", inputRow, hLines[inputRow])
	}

	// selectedText must likewise carry only the transcript's own text.
	text := a.selectedText()
	if text == "" {
		t.Fatal("selectedText() returned empty for a selection that covers a real transcript row")
	}
	if strings.Contains(text, ">") || strings.Contains(text, "─") {
		t.Errorf("selectedText() leaked chrome text (input/rule) = %q", text)
	}

	// handleMouseRelease's Cmd is the OSC 52 clipboard write (tea.SetClipboard)
	// — its resulting message stringifies to exactly the text it copies (a
	// named string type with no custom String(), so %v prints the string
	// value directly). It must carry the SAME region-clamped text, never the
	// input line.
	released, cmd := a.handleMouseRelease(tea.MouseReleaseMsg{X: 5, Y: ruleRow, Button: tea.MouseLeft})
	_ = released
	if cmd == nil {
		t.Fatal("handleMouseRelease returned a nil Cmd for a non-empty selection")
	}
	copied := fmt.Sprintf("%v", cmd())
	if copied != text {
		t.Errorf("handleMouseRelease's clipboard Cmd copied %q, want the same region-clamped text selectedText() returned (%q)", copied, text)
	}
	if strings.Contains(copied, ">") || strings.Contains(copied, "─") {
		t.Errorf("clipboard payload leaked chrome text (input/rule) = %q", copied)
	}
}

// TestSelectedTextMultiLineSpan covers a drag spanning several rows: the
// first row from its start column to the end, full rows in between, and the
// last row from its own start to the release column. Both rows are inside
// the roster body (the overview screen's transcript-region equivalent).
func TestSelectedTextMultiLineSpan(t *testing.T) {
	a := newAppForGolden(t, newInternalFakeSup(GoldenRoster()))
	rendered := a.render()
	lines := strings.Split(rendered, "\n")
	if len(lines) < 7 {
		t.Fatalf("precondition failed: expected at least 7 rendered rows, got %d", len(lines))
	}

	// Row 5 = "~/orchestration" (the cwd group header, 15 runes), row 6 =
	// "▸ wire the app root …" (the first roster row). Select all of row 5
	// (from column 0) through column 5 of row 6 ("▸ wire", the caret+space
	// prefix plus the first 4 letters of the title).
	a.sel = &selectionState{startX: 0, startY: 5, curX: 5, curY: 6}
	got := a.selectedText()
	want := "~/orchestration\n▸ wire"
	if got != want {
		t.Errorf("selectedText() multi-line span = %q, want %q", got, want)
	}
}

// TestSelectedTextNilSelection covers the no-op case: no selection means no
// text and no panic.
func TestSelectedTextNilSelection(t *testing.T) {
	a := newAppForGolden(t, newInternalFakeSup(GoldenRoster()))
	if got := a.selectedText(); got != "" {
		t.Errorf("selectedText() with no selection = %q, want empty", got)
	}
}

// TestHighlightSelectionAppliesReverseVideo covers highlightSelection's ANSI
// output directly: the covered cells carry the reverse-video SGR (7) and
// the uncovered ones don't. content is a single line, so the region is just
// that one row.
func TestHighlightSelectionAppliesReverseVideo(t *testing.T) {
	content := "hello world"
	sel := selectionState{startX: 0, startY: 0, curX: 4, curY: 0}
	got := highlightSelection(content, sel, testkit.ColorTheme(), 0, 0)

	const reverseOn = "\x1b[7m"
	if !strings.Contains(got, reverseOn) {
		t.Fatalf("highlightSelection output missing the reverse-video SGR, got %q", got)
	}
	if !strings.Contains(got, "hello") {
		t.Errorf("highlightSelection output missing the selected text \"hello\", got %q", got)
	}
	if !strings.Contains(got, " world") {
		t.Errorf("highlightSelection output missing the untouched trailing text \" world\", got %q", got)
	}
}

// TestHighlightSelectionOutOfRangeIsNoOp covers a selection whose row is
// entirely outside the region ([0, 2] here, content's own line range) — e.g.
// a stale selection left over after a resize/scroll shrank the content.
// highlightSelection must not panic and must return content unchanged, not
// clamp the out-of-region span onto the region's near edge as a false
// single-row overlap.
func TestHighlightSelectionOutOfRangeIsNoOp(t *testing.T) {
	content := "one\ntwo\nthree"
	sel := selectionState{startX: 0, startY: 100, curX: 5, curY: 200}
	if got := highlightSelection(content, sel, testkit.ColorTheme(), 0, 2); got != content {
		t.Errorf("highlightSelection with an out-of-range span = %q, want unchanged %q", got, content)
	}
}
