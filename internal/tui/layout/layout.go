// Package layout holds the geometry helpers the TUI screens share: the
// peek-split breakpoint, pane-size division, and block-joining. Everything
// here is pure string/int math so screens stay golden-testable without a
// terminal.
package layout

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// PeekHorizontalMinWidth is the terminal width at or above which the peek
// screen splits side-by-side (roster left, tail right) instead of stacked
// (roster top, tail bottom). Below it, a horizontal split would leave each
// pane too narrow for a readable roster row, so peek stacks vertically. 120
// columns gives each pane ~58 cols after the divider — enough for the roster's
// title+summary+metrics row.
const PeekHorizontalMinWidth = 120

// TopPadding is the number of blank rows prepended to every live TUI frame
// (overview, peek, attach) before it is rendered. Some terminal emulators —
// observed on a macOS beta running fullscreen — clip the top row of the
// alt-screen frame, swallowing half the header; this compensates by pushing
// the whole frame down one row. Safe to revert to 0, or make configurable,
// once the underlying terminal bug is fixed or better understood.
const TopPadding = 1

// columnDivider separates side-by-side panes: a space, a vertical rule, a
// space. columnDividerWidth is its DISPLAY width in columns — three cells —
// which differs from len(columnDivider) in bytes (the rule is a multi-byte
// rune), so pane-width math must use the width constant, never len().
const (
	columnDivider      = " │ "
	columnDividerWidth = 3
)

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
// column divider. The left pane takes the floor of the inner width so the split
// is stable, and `left + right + len(columnDivider) == total` holds for every
// total ≥ len(columnDivider)+2 — i.e. wide enough for two ≥1-column panes plus
// the divider. The peek screen only ever splits at [PeekHorizontalMinWidth]
// (120), far inside that range. Below the minimum a two-pane-plus-divider split
// cannot sum to total, so both panes clamp to 1 (a defensive path the peek gate
// makes unreachable).
func SplitWidth(total int) (left, right int) {
	inner := total - columnDividerWidth
	if inner < 2 {
		return 1, 1
	}
	left = inner / 2
	right = inner - left
	return left, right
}

// SplitHeight divides total rows into a top and bottom pane separated by a
// one-row divider. The top pane (the roster) takes the ceiling of the inner
// height so it gets the extra row when the height is odd, and
// `top + bottom + 1 == total` holds for every total ≥ 3 — wide enough for two
// ≥1-row panes plus the divider row. Below that a split cannot sum to total, so
// both panes clamp to 1 (a defensive path the peek layout makes unreachable).
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
		b.WriteString(padDisplay(lineAt(l, i), lw))
		b.WriteString(columnDivider)
		b.WriteString(padDisplay(lineAt(r, i), rw))
	}
	return b.String()
}

// blockWidth returns the widest line, in terminal cells (display width, so
// ANSI styling and wide runes are measured correctly), across lines.
func blockWidth(lines []string) int {
	w := 0
	for _, ln := range lines {
		if n := ansi.StringWidth(ln); n > w {
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

// padDisplay pads s with trailing spaces to exactly w terminal cells (display
// width). It never truncates: callers size blocks so lines fit.
func padDisplay(s string, w int) string {
	n := ansi.StringWidth(s)
	if n >= w {
		return s
	}
	return s + strings.Repeat(" ", w-n)
}
