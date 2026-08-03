package tui

// mouse_docscroll_test.go covers gofer#312: auto-scroll while drag-selecting
// on the attach screen. The scenario every test here drives is the same
// shape — a drag that starts on-screen, is held at the transcript window's
// top edge across a scroll boundary, and ends on content that was never
// simultaneously visible with where the drag started — because that is
// exactly the case the rejected "shift a screen-row anchor by the scroll
// delta" design (see mouse.go's package doc and selectionState.docMode)
// cannot get right: selectedText() must return the FULL range the drag
// covered, while highlightSelection (via App.render) must only ever paint
// whatever the CURRENT frame actually shows.

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/tui/layout"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// buildOverflowingAttachApp builds an attach [App] with turns numbered
// "turn 0".."turn N-1" and a window small enough that the transcript
// overflows and tails to the newest turn (scroll 0's default) with the
// identity header scrolled fully out of view — the shape every test in this
// file needs as a starting point.
func buildOverflowingAttachApp(t *testing.T, th theme.Theme, turns int) App {
	t.Helper()
	meta := GoldenMeta()
	meta.AttachSessionID = "sess-x"
	a := NewApp(th, &internalFakeSup{}, meta, GoldenCommandEnv())
	mdl, _ := a.Update(tea.WindowSizeMsg{Width: testkit.Width, Height: testkit.Height})
	a = mdl.(App)

	for i := 0; i < turns; i++ {
		mdl, _ = a.Update(sessEventMsg{
			id: "sess-x",
			ev: event.NewMessageFinished("sess-x", event.MessageUser, fmt.Sprintf("turn %d", i)),
		})
		a = mdl.(App)
	}

	rendered := ansi.Strip(a.render())
	if strings.Contains(rendered, "gofer v") {
		t.Fatalf("precondition failed: identity header still visible at scroll 0 — not enough turns to overflow:\n%s", rendered)
	}
	return a
}

// dragToTopEdge holds a docMode drag at the transcript window's top edge for
// up to maxSteps [tea.MouseMotionMsg] events, one per iteration (mirroring a
// user continuing to drag past the edge), stopping early once a.scroll stops
// advancing (the document's own top has been reached — see
// [App.extendDocSelection]). It returns the resulting App and how many steps
// actually moved a.scroll.
func dragToTopEdge(a App, maxSteps int) (App, int) {
	steps := 0
	for i := 0; i < maxSteps; i++ {
		before := a.scroll
		a = a.handleMouseMotion(tea.MouseMotionMsg{X: 0, Y: layout.TopPadding, Button: tea.MouseLeft})
		if a.scroll == before {
			break
		}
		steps++
	}
	return a, steps
}

// lastTranscriptTurnRow returns the absolute screen row of the LAST (most
// recent, tail-most) "○ turn N" line in rendered, and -1 if none is found.
func lastTranscriptTurnRow(rendered string) int {
	row := -1
	for i, l := range strings.Split(ansi.Strip(rendered), "\n") {
		if strings.HasPrefix(l, "○ turn ") {
			row = i
		}
	}
	return row
}

// firstTranscriptTurnRow returns the absolute screen row of the FIRST
// (topmost, currently-visible-but-oldest) "○ turn N" line in rendered, and
// -1 if none is found.
func firstTranscriptTurnRow(rendered string) int {
	for i, l := range strings.Split(ansi.Strip(rendered), "\n") {
		if strings.HasPrefix(l, "○ turn ") {
			return i
		}
	}
	return -1
}

// TestDragAutoScrollExtendsSelectionAcrossBoundary is #312's headline
// scenario: click the newest (tail) transcript row, then drag-hold the top
// edge until the drag has scrolled all the way back to the identity header —
// a range that spans far more than any single frame ever shows at once.
// selectedText() must carry the FULL range (the tail row included, even
// though it has since scrolled off-screen), while the CURRENT render must
// no longer show that row at all — proving highlight and clipboard read two
// different, correctly-scoped things rather than disagreeing (#307's
// invariant, restated for a moving viewport).
func TestDragAutoScrollExtendsSelectionAcrossBoundary(t *testing.T) {
	const turns = 40
	a := buildOverflowingAttachApp(t, theme.Test(), turns)

	rendered := a.render()
	tailRow := lastTranscriptTurnRow(rendered)
	if tailRow < 0 {
		t.Fatalf("precondition failed: no transcript row found:\n%s", rendered)
	}
	if got := ansi.Strip(strings.Split(rendered, "\n")[tailRow]); got != "○ turn 39" {
		t.Fatalf("precondition failed: expected the tail row to be turn 39, got %q", got)
	}

	// X: 999 — past every row's real width, so [App.selectedDocText]'s
	// per-row clamp (`clampInt(x1+1, 0, width)`) takes the row's own end
	// rather than column 2, capturing "○ turn 39" in full once span()
	// normalizes this click into the span's END bound (see
	// TestSelectedTextMultiLineSpan's sibling tests for the same pattern
	// against the frame-based path).
	a, cmd := a.handleMouseClick(tea.MouseClickMsg{X: 999, Y: tailRow, Button: tea.MouseLeft})
	if cmd != nil {
		t.Fatalf("click on a transcript row returned an unexpected Cmd")
	}
	if a.sel == nil || !a.sel.docMode {
		t.Fatalf("click inside the attach transcript did not start a document-coordinate selection: %+v", a.sel)
	}

	a, steps := dragToTopEdge(a, 200)
	if steps == 0 {
		t.Fatalf("precondition failed: holding the drag at the top edge never scrolled a.scroll")
	}

	finalRendered := ansi.Strip(a.render())
	if !strings.Contains(finalRendered, "gofer v") {
		t.Fatalf("expected the held drag to have scrolled all the way back to the identity header:\n%s", finalRendered)
	}
	if strings.Contains(finalRendered, "turn 39") {
		t.Fatalf("expected the drag's starting row (turn 39) to have scrolled off-screen by now:\n%s", finalRendered)
	}

	text := a.selectedText()
	if !strings.Contains(text, "turn 39") {
		t.Errorf("selectedText() dropped the drag's starting row, which has since scrolled off-screen: %q", text)
	}
	if !strings.Contains(text, "turn 0") {
		t.Errorf("selectedText() did not reach turn 0 despite scrolling all the way back: %q", text)
	}

	released, cmd := a.handleMouseRelease(tea.MouseReleaseMsg{X: 0, Y: layout.TopPadding, Button: tea.MouseLeft})
	if cmd == nil {
		t.Fatal("handleMouseRelease returned a nil Cmd for a non-empty cross-boundary selection")
	}
	copied := fmt.Sprintf("%v", cmd())
	if copied != text {
		t.Errorf("handleMouseRelease's clipboard Cmd copied %q, want the same text selectedText() returned (%q)", copied, text)
	}
	if released.sel.dragging {
		t.Errorf("handleMouseRelease left the selection marked as still dragging")
	}
}

// TestDragAutoScrollHighlightOnlyPaintsCurrentlyVisibleSlice is the highlight
// half of the boundary scenario above, using the color theme so the
// reverse-video SGR is observable: once the drag has scrolled back past the
// tail row, that row's highlight must be gone from the CURRENT frame (it
// isn't rendered at all any more), while the now-visible top-of-window row —
// which the same selection also covers — must be actively painted. Both
// halves read through the exact same a.sel; only which rows the frame
// currently contains differs.
func TestDragAutoScrollHighlightOnlyPaintsCurrentlyVisibleSlice(t *testing.T) {
	const turns = 40
	const reverseOn = "\x1b[7m"
	a := buildOverflowingAttachApp(t, testkit.ColorTheme(), turns)

	rendered := a.render()
	tailRow := lastTranscriptTurnRow(rendered)
	if tailRow < 0 {
		t.Fatalf("precondition failed: no transcript row found:\n%s", rendered)
	}

	a, _ = a.handleMouseClick(tea.MouseClickMsg{X: 2, Y: tailRow, Button: tea.MouseLeft})
	if a.sel == nil || !a.sel.docMode {
		t.Fatalf("click inside the attach transcript did not start a document-coordinate selection: %+v", a.sel)
	}

	var steps int
	a, steps = dragToTopEdge(a, 200)
	if steps == 0 {
		t.Fatalf("precondition failed: holding the drag at the top edge never scrolled a.scroll")
	}

	finalRendered := a.render()
	finalLines := strings.Split(finalRendered, "\n")
	if strings.Contains(ansi.Strip(finalRendered), "turn 39") {
		t.Fatalf("precondition failed: turn 39 still visible after scrolling back:\n%s", ansi.Strip(finalRendered))
	}

	// The now-visible top-of-window row (layout.TopPadding, the header's own
	// first line — it is part of the same document range the drag covers)
	// must be painted.
	if !strings.Contains(finalLines[layout.TopPadding], reverseOn) {
		t.Errorf("top-of-window row %d not highlighted despite being inside the (now scrolled-back) selection: %q", layout.TopPadding, finalLines[layout.TopPadding])
	}

	// No row in the final frame carries stale highlight for content the
	// selection covers but the CURRENT frame doesn't show — every rendered
	// row either has real content (checked above) or, if it did carry
	// reverse video, would be painting something that isn't on screen. Since
	// turn 39 itself isn't rendered at all any more, the only way to prove
	// this negative is structural: the region this frame CAN paint is
	// exactly [layout.TopPadding, layout.TopPadding+avail-1] (attachDocWindow's
	// avail), so nothing beyond it should be reverse-video from this
	// selection either.
	start, avail, ok := a.attachDocWindow(len(a.attachDocLines()))
	if !ok {
		t.Fatalf("attachDocWindow reported not ok after scrolling back")
	}
	if start != 0 {
		t.Fatalf("expected scrolling to have reached the document's own top (start=0), got start=%d", start)
	}
	for y := layout.TopPadding + avail; y < len(finalLines); y++ {
		if strings.Contains(finalLines[y], reverseOn) && strings.Contains(ansi.Strip(finalLines[y]), "turn") {
			t.Errorf("row %d outside the transcript window carries a stale highlight over transcript text: %q", y, finalLines[y])
		}
	}
}

// TestDragAutoScrollStepIsMonotonicPerMotionEvent pins dragScrollStep: N
// consecutive [tea.MouseMotionMsg] events held at the top edge must advance
// a.scroll by exactly N (one row per event), never a jump straight to the
// document's start. This is the "grows monotonically...never jumping"
// requirement from a single-event granularity a full-boundary test can't
// distinguish from a jump that happens to also converge.
func TestDragAutoScrollStepIsMonotonicPerMotionEvent(t *testing.T) {
	const turns = 40
	a := buildOverflowingAttachApp(t, theme.Test(), turns)

	rendered := a.render()
	topRow := firstTranscriptTurnRow(rendered)
	if topRow < 0 {
		t.Fatalf("precondition failed: no transcript row found:\n%s", rendered)
	}

	a, _ = a.handleMouseClick(tea.MouseClickMsg{X: 2, Y: topRow, Button: tea.MouseLeft})
	if a.sel == nil || !a.sel.docMode {
		t.Fatalf("click inside the attach transcript did not start a document-coordinate selection: %+v", a.sel)
	}

	const steps = 5
	for i := 1; i <= steps; i++ {
		a = a.handleMouseMotion(tea.MouseMotionMsg{X: 0, Y: layout.TopPadding, Button: tea.MouseLeft})
		if want := i * dragScrollStep; a.scroll != want {
			t.Fatalf("after %d motion event(s) held at the top edge, a.scroll = %d, want %d (exactly dragScrollStep per event)", i, a.scroll, want)
		}
	}
}

// TestWordSelectDragAutoScrollAcrossBoundary is the word-select ([selectionState.wordSelect])
// counterpart of TestDragAutoScrollExtendsSelectionAcrossBoundary: a
// double-click anchors a whole word on the tail row, then the same
// held-at-the-edge drag scrolls back to the header. The resulting selection
// spans complete rows between the anchor and the cursor's word exactly like
// character-mode does, so selectedText() must still carry both the anchor
// row and everything scrolled past to reach it.
func TestWordSelectDragAutoScrollAcrossBoundary(t *testing.T) {
	const turns = 40
	a := buildOverflowingAttachApp(t, theme.Test(), turns)

	rendered := a.render()
	tailRow := lastTranscriptTurnRow(rendered)
	if tailRow < 0 {
		t.Fatalf("precondition failed: no transcript row found:\n%s", rendered)
	}
	// "○ turn 39": glyph+space at columns 0-1, "turn" at [2,6), "39" at [7,9).
	click := tea.MouseClickMsg{X: 7, Y: tailRow, Button: tea.MouseLeft}
	a, _ = a.handleMouseClick(click) // first click — plain
	a, _ = a.handleMouseClick(click) // second click — double, word-select

	if a.sel == nil || !a.sel.docMode || !a.sel.wordSelect {
		t.Fatalf("double-click inside the attach transcript did not start a document-coordinate word selection: %+v", a.sel)
	}
	if got := a.selectedText(); got != "39" {
		t.Fatalf("precondition failed: double-click selected %q, want the anchor word %q", got, "39")
	}

	a, steps := dragToTopEdge(a, 200)
	if steps == 0 {
		t.Fatalf("precondition failed: holding the drag at the top edge never scrolled a.scroll")
	}

	finalRendered := ansi.Strip(a.render())
	if !strings.Contains(finalRendered, "gofer v") {
		t.Fatalf("expected the held drag to have scrolled all the way back to the identity header:\n%s", finalRendered)
	}
	if strings.Contains(finalRendered, "turn 39") {
		t.Fatalf("expected the drag's anchor row (turn 39) to have scrolled off-screen by now:\n%s", finalRendered)
	}

	text := a.selectedText()
	if !strings.Contains(text, "turn 39") {
		t.Errorf("word-select selectedText() dropped the anchor row, which has since scrolled off-screen: %q", text)
	}
	if !strings.Contains(text, "turn 0") {
		t.Errorf("word-select selectedText() did not reach turn 0 despite scrolling all the way back: %q", text)
	}
}

// TestDragIntoAttachFooterAfterDocSelectionClampsAtDocumentEnd pins the
// scoped trade-off selectionState.docMode's doc names: once a drag has
// started inside the attach document, continuing it into the footer/menu
// chrome below no longer selects that chrome's own text the way a
// non-docMode drag still does (TestSelectionReachesChrome) — it clamps to
// the document's own last line. This is deliberate: mixing the scrolling
// document range with the footer's fixed screen rows in one selection would
// need two coordinate spaces to agree, which is exactly the complexity
// document coordinates exist to avoid. A drag that starts on the footer
// (docMode false) is unaffected — that content never scrolls, so there is no
// boundary to cross and the existing frame-based path still reaches it.
func TestDragIntoAttachFooterAfterDocSelectionClampsAtDocumentEnd(t *testing.T) {
	const turns = 3 // deliberately NOT enough to overflow
	a := NewApp(theme.Test(), &internalFakeSup{}, func() OverviewMeta {
		m := GoldenMeta()
		m.AttachSessionID = "sess-x"
		return m
	}(), GoldenCommandEnv())
	mdl, _ := a.Update(tea.WindowSizeMsg{Width: testkit.Width, Height: testkit.Height})
	a = mdl.(App)
	for i := 0; i < turns; i++ {
		mdl, _ = a.Update(sessEventMsg{
			id: "sess-x",
			ev: event.NewMessageFinished("sess-x", event.MessageUser, fmt.Sprintf("turn %d", i)),
		})
		a = mdl.(App)
	}

	rendered := ansi.Strip(a.render())
	lines := strings.Split(rendered, "\n")
	tailRow, inputRow := -1, -1
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "○ turn "):
			tailRow = i
		case inputRow < 0 && strings.HasPrefix(l, "> "):
			inputRow = i
		}
	}
	if tailRow < 0 || inputRow < 0 || tailRow >= inputRow {
		t.Fatalf("precondition failed: tailRow=%d inputRow=%d in:\n%s", tailRow, inputRow, rendered)
	}

	// X: 0 on the click, 999 on the motion — since both end up mapped to the
	// SAME document row (there's nothing left to scroll to; see the
	// assertion below), this keeps span()'s two column bounds at [0, row
	// width) instead of collapsing to whatever single column the click
	// happened to land on, so the assertions below can tell "clamped to the
	// whole last line" apart from "clamped to nothing".
	a, _ = a.handleMouseClick(tea.MouseClickMsg{X: 0, Y: tailRow, Button: tea.MouseLeft})
	if a.sel == nil || !a.sel.docMode {
		t.Fatalf("click inside the attach transcript did not start a document-coordinate selection: %+v", a.sel)
	}
	a = a.handleMouseMotion(tea.MouseMotionMsg{X: 999, Y: inputRow, Button: tea.MouseLeft})

	// a.scroll never moved — there was nothing above tailRow left to reveal
	// past the (already tiny, non-overflowing) document's own end.
	if a.scroll != 0 {
		t.Errorf("a.scroll moved to %d dragging into the footer of a non-overflowing transcript; want 0 (nothing to scroll)", a.scroll)
	}

	text := a.selectedText()
	if strings.Contains(text, "─") {
		t.Errorf("docMode selection reached the input box's framing rule; want it clamped to the document's own last line: %q", text)
	}
	if !strings.Contains(text, fmt.Sprintf("turn %d", turns-1)) {
		t.Errorf("docMode selection lost the document's own last line while clamping: %q", text)
	}
}
