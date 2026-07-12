// Package layout holds the geometry helpers the TUI screens share: the
// peek-split breakpoint, pane-size division, and block-joining. Everything
// here is pure string/int math so screens stay golden-testable without a
// terminal.
package layout

import "strings"

// PeekHorizontalMinWidth is the terminal width at or above which the peek
// screen splits side-by-side (roster left, tail right) instead of stacked
// (roster top, tail bottom). Below it, a horizontal split would leave each
// pane too narrow for a readable roster row, so peek stacks vertically. 120
// columns gives each pane ~58 cols after the divider — enough for the roster's
// title+summary+metrics row.
const PeekHorizontalMinWidth = 120

// columnDivider separates side-by-side panes: a space, a vertical rule, a
// space.
const columnDivider = " │ "

// Orientation is how the peek screen arranges its two panes.
type Orientation int

const (
	// Vertical stacks the panes (roster on top, tail below).
	Vertical Orientation = iota
	// Horizontal places the panes side by side (roster left, tail right).
	Horizontal
)

// PeekOrientation picks the peek split orientation for a terminal width.
func PeekOrientation(width int) Orientation {
	if width >= PeekHorizontalMinWidth {
		return Horizontal
	}
	return Vertical
}

// SplitWidth divides total columns into a left and right pane separated by the
// column divider. The left pane takes the floor of half the remaining width so
// the split is stable and the two panes sum (with the divider) to exactly
// total.
func SplitWidth(total int) (left, right int) {
	inner := total - len(columnDivider)
	if inner < 2 {
		// Too narrow to split meaningfully; give each pane at least 1 col.
		return 1, 1
	}
	left = inner / 2
	right = inner - left
	return left, right
}

// SplitHeight divides total rows into a top and bottom pane separated by a
// one-row divider. The top pane (the roster) takes the ceiling of half so the
// roster gets the extra row when the height is odd.
func SplitHeight(total int) (top, bottom int) {
	inner := total - 1 // divider row
	if inner < 2 {
		return 1, 1
	}
	top = (inner + 1) / 2
	bottom = inner - top
	return top, bottom
}

// JoinColumns zips two blocks side by side with the column divider between
// them, padding the shorter block with blank lines and every line to its
// block's widest line so the divider stays plumb. Callers that pre-size each
// block to a fixed width get exact, deterministic output.
func JoinColumns(left, right string) string {
	l := strings.Split(left, "\n")
	r := strings.Split(right, "\n")
	rows := max(len(l), len(r))

	lw := blockWidth(l)
	rw := blockWidth(r)

	var b strings.Builder
	for i := range rows {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(padRunes(lineAt(l, i), lw))
		b.WriteString(columnDivider)
		b.WriteString(padRunes(lineAt(r, i), rw))
	}
	return b.String()
}

// blockWidth returns the widest line (in runes) across lines.
func blockWidth(lines []string) int {
	w := 0
	for _, ln := range lines {
		if n := len([]rune(ln)); n > w {
			w = n
		}
	}
	return w
}

// lineAt returns the i-th line, or "" past the end.
func lineAt(lines []string, i int) string {
	if i < len(lines) {
		return lines[i]
	}
	return ""
}

// padRunes pads s with trailing spaces to exactly w runes. It never truncates:
// callers size blocks so lines fit.
func padRunes(s string, w int) string {
	n := len([]rune(s))
	if n >= w {
		return s
	}
	return s + strings.Repeat(" ", w-n)
}
