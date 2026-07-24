package tui

// choicelist.go is the vertical choice list both interactive prompts answer
// through: the approval prompt's Yes/No (approval.go) and the structured-
// decision prompt's options (decision.go). Factoring it here is the point — the
// two prompts MUST offer the same selection model (a caret on the focused row,
// ↑/↓ to move, Enter to take the focused row, quick keys beside it), and one
// renderer plus one clamp is how that stays true as either prompt changes. Each
// prompt keeps its own key handler (their quick keys and actions differ) but
// both route their answer rows through [choiceListLines] and their cursor
// through [stepChoiceCursor], so a change to the caret, the gutter, or the
// clamp is a single edit that both inherit.

import "github.com/jedwards1230/gofer/internal/tui/theme"

// choiceCaret is the focus marker every vertical selection list in this TUI
// shares — the roster, the command menu, /config, /model, and now both
// interactive prompts — and choiceGutter is its blank counterpart, exactly as
// wide (the caret plus its trailing space) so focusing a row never shifts the
// columns beneath it.
const (
	choiceCaret  = "▸"
	choiceGutter = "  "
)

// choiceRow is one selectable entry in a vertical choice list: a leader token
// drawn between the focus gutter and the label (a number, a "[a]" key hint, a
// glyph), the fully-composed label (any trailing marker is the caller's to
// append, so the renderer stays state-agnostic), any pre-styled continuation
// sub-lines drawn under the label, and whether a blank row precedes it.
type choiceRow struct {
	leader    string
	label     string
	sublines  []string
	sepBefore bool
}

// choiceListLines renders rows as a vertical, caret-navigable list at width:
// the row at cursor gets the accent caret in its gutter, every other row a
// blank gutter of equal width, and each label HANGS under its leader
// (hangingIndent) so a wrapped label lines up under its text rather than under
// the gutter. A row's pre-styled sub-lines follow its label, and a row marked
// sepBefore opens with one blank line — the only two shapes either prompt needs
// beyond the flat list.
//
// Only the caret carries color; the leaders and labels are the caller's own
// (already styled where they bear state, e.g. an accent "(Recommended)"), which
// keeps this the marker-only render the styled goldens read: an Ascii golden
// sees the exact geometry, the caret is the one state token color adds.
func choiceListLines(th theme.Theme, rows []choiceRow, cursor, width int) []string {
	out := make([]string, 0, len(rows)*2)
	for i, row := range rows {
		if row.sepBefore {
			out = append(out, "")
		}
		gutter := choiceGutter
		if i == cursor {
			gutter = th.AccentStyle().Render(choiceCaret) + " "
		}
		out = append(out, hangingIndent(gutter+row.leader, row.label, width)...)
		out = append(out, row.sublines...)
	}
	return out
}

// stepChoiceCursor moves cur by delta over an n-row list, CLAMPED to
// [0, n-1] rather than wrapping: both prompts' lists are short and ordered, and
// wrapping from the last row back onto the first is precisely the surprise that
// gets the wrong answer sent. An empty list floors to 0.
func stepChoiceCursor(cur, delta, n int) int {
	if n <= 0 {
		return 0
	}
	return clampInt(cur+delta, 0, n-1)
}
