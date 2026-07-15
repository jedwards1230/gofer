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
// released-over cell.
func TestSelectedTextSingleLine(t *testing.T) {
	a := newAppForGolden(t, newInternalFakeSup(GoldenRoster()))
	// Row 1 of the rendered content (row 0 is layout.TopPadding's blank
	// filler) is the identity header's first line: "gofer v0.3.0".
	a.sel = &selectionState{startX: 0, startY: 1, curX: 4, curY: 1}
	if got := a.selectedText(); got != "gofer" {
		t.Errorf("selectedText() = %q, want %q", got, "gofer")
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

// TestSelectedTextMultiLineSpan covers a drag spanning several rows: the
// first row from its start column to the end, full rows in between, and the
// last row from its own start to the release column.
func TestSelectedTextMultiLineSpan(t *testing.T) {
	a := newAppForGolden(t, newInternalFakeSup(GoldenRoster()))
	rendered := a.render()
	lines := strings.Split(rendered, "\n")
	if len(lines) < 3 {
		t.Fatalf("precondition failed: expected at least 3 rendered rows, got %d", len(lines))
	}

	// Row 1 = "gofer v0.3.0", row 2 = "claude-sonnet-5 · ~/orchestration".
	// Select from column 6 of row 1 ("v0.3.0") through column 14 of row 2
	// ("claude-sonnet-5" — 15 runes, columns 0-14).
	a.sel = &selectionState{startX: 6, startY: 1, curX: 14, curY: 2}
	got := a.selectedText()
	want := "v0.3.0\nclaude-sonnet-5"
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
// the uncovered ones don't.
func TestHighlightSelectionAppliesReverseVideo(t *testing.T) {
	content := "hello world"
	sel := selectionState{startX: 0, startY: 0, curX: 4, curY: 0}
	got := highlightSelection(content, sel, testkit.ColorTheme())

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
// entirely outside content's line range (e.g. scrolled/resized away) —
// highlightSelection must not panic and must return content unchanged.
func TestHighlightSelectionOutOfRangeIsNoOp(t *testing.T) {
	content := "one\ntwo\nthree"
	sel := selectionState{startX: 0, startY: 100, curX: 5, curY: 200}
	if got := highlightSelection(content, sel, testkit.ColorTheme()); got != content {
		t.Errorf("highlightSelection with an out-of-range span = %q, want unchanged %q", got, content)
	}
}
