package tui

// choicelist_test.go covers the shared vertical choice list directly — the seam
// both interactive prompts answer through (approval.go, decision.go). The
// prompts' own golden and Update tests exercise it in situ; these pin the two
// invariants both rely on without a whole prompt around them: the caret marks
// exactly the focused row, and the cursor clamps rather than wrapping.

import (
	"strings"
	"testing"

	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// TestChoiceListLinesCaretMarksFocusedRow proves the accent caret sits on the
// cursor's row and every other row gets a blank gutter of equal width, so
// focusing a row never shifts the columns beneath it.
func TestChoiceListLinesCaretMarksFocusedRow(t *testing.T) {
	rows := []choiceRow{
		{leader: "[a] ", label: "Yes"},
		{leader: "[d] ", label: "No"},
	}
	got := choiceListLines(theme.Test(), rows, 1, 40)
	if len(got) != 2 {
		t.Fatalf("choiceListLines returned %d rows, want 2:\n%q", len(got), got)
	}
	if strings.HasPrefix(got[0], choiceCaret) {
		t.Errorf("unfocused row 0 carries the caret: %q", got[0])
	}
	if !strings.HasPrefix(got[1], choiceCaret) {
		t.Errorf("focused row 1 is missing the caret: %q", got[1])
	}
	// The gutters are the same display width, caret or not: the leader "[" lands
	// on the same column in both rows.
	if a, b := leaderCol(got[0]), leaderCol(got[1]); a != b {
		t.Errorf("gutters differ in width between focused and unfocused rows: %q (col %d) vs %q (col %d)", got[0], a, got[1], b)
	}
}

// leaderCol is the rune index (display column, since these rows carry no ANSI
// under theme.Test) of the first "[" — the leader's start — used to prove the
// caret gutter and the blank gutter are equal width.
func leaderCol(s string) int {
	for i, r := range []rune(s) {
		if r == '[' {
			return i
		}
	}
	return -1
}

// TestStepChoiceCursorClamps proves the shared cursor step clamps to the list
// rather than wrapping — the property that keeps a stray key from selecting the
// opposite answer in either prompt.
func TestStepChoiceCursorClamps(t *testing.T) {
	tests := []struct {
		name          string
		cur, delta, n int
		want          int
	}{
		{"down within range", 0, 1, 2, 1},
		{"down clamps at end", 1, 1, 2, 1},
		{"up clamps at start", 0, -1, 2, 0},
		{"empty list floors to 0", 3, 1, 0, 0},
		{"big jump clamps", 0, 9, 3, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := stepChoiceCursor(tc.cur, tc.delta, tc.n); got != tc.want {
				t.Errorf("stepChoiceCursor(%d, %d, %d) = %d, want %d", tc.cur, tc.delta, tc.n, got, tc.want)
			}
		})
	}
}
