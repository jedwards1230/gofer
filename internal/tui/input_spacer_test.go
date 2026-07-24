package tui

// input_spacer_test.go is the mutation guard for the transcript↔input spacer:
// exactly one blank row sits between the last transcript message and the input
// block when the transcript overflows and tails flush to the frame.

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jedwards1230/gofer/internal/tui/testkit"
)

// TestInputSpacerExactlyOneBlankRow forces a clearly-overflowing attach
// transcript (so scrollTail fills every available row and pad() lays down NO
// bottom filler), then asserts the footer tail is exactly
// [spacer, top-rule, input, bottom-rule] — one blank row above the input's top
// rule, with transcript content directly above that blank. Before the fix the
// newest message butted against the top rule with no gap; drop the
// `footer = append(footer, "")` spacer in Model.view and the row above the top
// rule becomes content, flipping this red. (In a SHORT, padded frame the spacer
// is indistinguishable from pad()'s filler, which is why this guard forces
// overflow rather than trusting a golden.)
func TestInputSpacerExactlyOneBlankRow(t *testing.T) {
	a := buildOverflowingMixedTranscript(t)
	// A deliberately small height guarantees content > avail, so the transcript
	// tails flush with no bottom padding — the only blank row that can sit above
	// the input block is the spacer.
	mdl, _ := a.Update(tea.WindowSizeMsg{Width: testkit.Width, Height: 16})
	a = mdl.(App)

	lines := strings.Split(a.render(), "\n")
	n := len(lines)
	if n < 5 {
		t.Fatalf("frame too short to test the input footer: %d rows", n)
	}

	isRule := func(s string) bool {
		stripped := ansi.Strip(s)
		return stripped != "" && strings.Trim(stripped, "─") == ""
	}

	// Footer tail, bottom-up: [spacer, top-rule, input line, bottom-rule].
	if !isRule(lines[n-1]) {
		t.Fatalf("expected the input's bottom rule on the last row, got %q", ansi.Strip(lines[n-1]))
	}
	if !isRule(lines[n-3]) {
		t.Fatalf("expected the input's top rule at row n-3, got %q", ansi.Strip(lines[n-3]))
	}
	if spacer := ansi.Strip(lines[n-4]); strings.TrimSpace(spacer) != "" {
		t.Errorf("expected a single blank spacer row above the input top rule, got %q", spacer)
	}
	if content := ansi.Strip(lines[n-5]); strings.TrimSpace(content) == "" {
		t.Errorf("expected transcript content directly above the spacer (exactly one blank row), found a second blank row")
	}
}
