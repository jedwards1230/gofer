package tui

import (
	"fmt"
	"strings"
	"time"
)

// Row layout column budgets. The row is prefix + body(title,summary) + a
// right-aligned metrics block (cost + age). Body flexes with terminal width.
const (
	rowPrefixW  = 4  // selection caret, space, status glyph, space
	rowRightW   = 16 // right-aligned "$cost  age" block
	rowTitleW   = 28 // title column before the summary
	rowColGap   = 2  // gap between title and summary
	headerLines = 4  // app line, model·cwd line, counts line, blank
	dispatchH   = 3  // rule, input line, hint line
)

// View renders the header, roster body, and dispatch bar at the given size.
// The body windows to keep the selected row visible; the dispatch bar is
// always pinned to the bottom.
func (o Overview) View(width, height int) string {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}

	var out []string
	out = append(out, o.header(width)...)

	bodyAvail := height - headerLines - dispatchH
	if bodyAvail < 0 {
		bodyAvail = 0
	}
	out = append(out, o.body(width, bodyAvail)...)
	out = append(out, o.dispatch(width)...)

	// Clip defensively so a component never overruns its allotted height.
	if len(out) > height {
		out = out[:height]
	}
	return strings.Join(out, "\n")
}

// Rail renders the roster as a peek/split rail — the header and body only, no
// dispatch bar — filled to exactly height rows. Selection and view state
// render identically to the full screen, so the roster reads the same whether
// it owns the terminal or shares it with a tail pane.
func (o Overview) Rail(width, height int) string {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}

	out := o.header(width)
	bodyAvail := height - headerLines
	if bodyAvail < 0 {
		bodyAvail = 0
	}
	out = append(out, o.body(width, bodyAvail)...)

	if len(out) > height {
		out = out[:height]
	}
	for len(out) < height {
		out = append(out, "")
	}
	return strings.Join(out, "\n")
}

// header renders the app identity, model·cwd context, and status counts.
func (o Overview) header(width int) []string {
	working, needsInput, finished := o.counts()

	app := o.meta.App
	if app == "" {
		app = "gofer"
	}
	title := o.theme.AccentStyle().Render(app)
	if o.meta.Version != "" {
		title += " " + o.theme.MutedStyle().Render("v"+o.meta.Version)
	}

	context := o.meta.Model
	if o.meta.Cwd != "" {
		if context != "" {
			context += " · "
		}
		context += o.meta.Cwd
	}

	counts := fmt.Sprintf("%d awaiting input · %d working · %d completed", needsInput, working, finished)

	return []string{
		truncate(title, width),
		truncate(o.theme.MutedStyle().Render(context), width),
		truncate(o.theme.MutedStyle().Render(counts), width),
		"",
	}
}

// body renders the roster rows for the current view, windowed to avail lines
// with the selected row kept visible.
func (o Overview) body(width, avail int) []string {
	if avail <= 0 {
		return nil
	}

	lines, selLine := o.rows(width)
	if len(lines) == 0 {
		empty := o.theme.MutedStyle().Render("No sessions yet — type below to start one.")
		return pad([]string{truncate(empty, width)}, avail)
	}
	lines = window(lines, selLine, avail)
	return pad(lines, avail)
}

// rows renders the roster into display lines and reports the line index of the
// selected row (-1 when nothing is selected). Grouped view interleaves section
// headers and a blank line between sections.
func (o Overview) rows(width int) (lines []string, selLine int) {
	selLine = -1
	appendRow := func(s SessionInfo) {
		if s.ID == o.selectedID {
			selLine = len(lines)
		}
		lines = append(lines, o.row(s, width))
	}

	if o.view == viewFlat {
		for _, s := range o.ordered() {
			appendRow(s)
		}
		return lines, selLine
	}

	first := true
	for _, st := range []SessionStatus{StatusWorking, StatusNeedsInput, StatusFinished} {
		group := byRecency(o.filter(st))
		if len(group) == 0 {
			continue
		}
		if !first {
			lines = append(lines, "")
		}
		first = false
		lines = append(lines, truncate(o.theme.MutedStyle().Render(st.String()), width))
		for _, s := range group {
			appendRow(s)
		}
	}
	return lines, selLine
}

// row renders one session as a single line: selection caret, status glyph,
// title, one-line summary, and a right-aligned cost·age metrics block.
func (o Overview) row(s SessionInfo, width int) string {
	caret := " "
	if s.ID == o.selectedID {
		caret = "▸"
	}
	prefix := fmt.Sprintf("%s %s ", caret, o.statusGlyph(s))

	right := o.metrics(s)
	bodyW := width - rowPrefixW - rowRightW
	if bodyW < 1 {
		// Too narrow for the full row; show just caret, glyph, and title.
		return truncate(prefix+s.Title, width)
	}

	titleW := rowTitleW
	if titleW > bodyW-rowColGap-1 {
		titleW = bodyW - rowColGap - 1
	}
	if titleW < 1 {
		titleW = 1
	}
	summaryW := bodyW - titleW - rowColGap

	title := padTo(s.Title, titleW)
	summary := o.theme.MutedStyle().Render(padTo(s.Summary, summaryW))
	line := prefix + title + strings.Repeat(" ", rowColGap) + summary + padLeft(right, rowRightW)

	if s.ID == o.selectedID {
		return o.theme.AccentStyle().Render(line)
	}
	return line
}

// statusGlyph maps a session's status to its roster glyph, promoting to the
// approval glyph when a permission request is pending.
func (o Overview) statusGlyph(s SessionInfo) string {
	if s.Pending > 0 {
		return o.theme.GlyphApproval
	}
	switch s.Status {
	case StatusWorking:
		return o.theme.GlyphStreaming
	case StatusFinished:
		return o.theme.GlyphOK
	default:
		return o.theme.GlyphIdle
	}
}

// metrics renders the right-aligned cost·age block for a row. Cost is omitted
// until a turn has accrued any; age is always shown.
func (o Overview) metrics(s SessionInfo) string {
	age := humanAge(o.meta.Now.Sub(s.Updated))
	if s.Cost.USD <= 0 {
		return age
	}
	return fmt.Sprintf("$%.4f  %s", s.Cost.USD, age)
}

// dispatch renders the bottom dispatch bar: a rule, the input line (a
// placeholder until the user types), and a one-line shortcut hint.
func (o Overview) dispatch(width int) []string {
	rule := strings.Repeat("─", width)

	var line string
	if o.input == "" {
		line = "❯ " + o.theme.MutedStyle().Render("describe a task for a new session")
	} else {
		line = "❯ " + o.input + "▏"
	}

	hint := o.theme.MutedStyle().Render("enter peek · → attach · tab toggle view · ctrl-x kill · ? shortcuts")

	return []string{rule, truncate(line, width), truncate(hint, width)}
}

// humanAge renders a duration as a compact age string ("now", "5m", "3h",
// "2d").
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// pad returns lines extended with blank lines to exactly n rows (never
// truncating; callers window first when they may exceed n).
func pad(lines []string, n int) []string {
	for len(lines) < n {
		lines = append(lines, "")
	}
	return lines
}

// window returns at most n lines from lines, scrolled so the sel-th line stays
// visible. A sel of -1 (nothing selected) shows the top.
func window(lines []string, sel, n int) []string {
	if len(lines) <= n {
		return lines
	}
	start := 0
	if sel >= n {
		start = sel - n + 1
	}
	if start > len(lines)-n {
		start = len(lines) - n
	}
	return lines[start : start+n]
}

// padTo returns s padded with trailing spaces to exactly w runes, truncating
// with an ellipsis when longer.
func padTo(s string, w int) string {
	r := []rune(s)
	if len(r) == w {
		return s
	}
	if len(r) > w {
		return truncate(s, w)
	}
	return s + strings.Repeat(" ", w-len(r))
}

// padLeft returns s padded with leading spaces to exactly w runes, truncating
// with an ellipsis when longer.
func padLeft(s string, w int) string {
	r := []rune(s)
	if len(r) > w {
		return truncate(s, w)
	}
	return strings.Repeat(" ", w-len(r)) + s
}
