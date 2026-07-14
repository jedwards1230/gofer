package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// Row layout column budgets. The row is prefix + body(title, statusword ·
// summary) + a right-aligned age. Body flexes with terminal width.
const (
	rowPrefixW  = 5  // selection caret, space, status glyph(+pending digit), space
	rowRightW   = 5  // right-aligned compact age ("now", "59m", "23h", "2d")
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
// selected row (-1 when nothing is selected). The flat view groups rows under a
// cwd header per working directory (recency within each group); the grouped
// view interleaves Working / Needs input / Finished section headers. Both
// interleave a blank line between groups.
func (o Overview) rows(width int) (lines []string, selLine int) {
	selLine = -1
	appendRow := func(s SessionInfo, showStatus bool) {
		if s.ID == o.selectedID {
			selLine = len(lines)
		}
		lines = append(lines, o.row(s, width, showStatus))
	}
	header := func(label string) { lines = append(lines, truncate(o.theme.MutedStyle().Render(label), width)) }

	if o.view == viewFlat {
		// Group the recency-ordered roster by cwd, preserving first-appearance
		// order so the most-recently-active cwd's group comes first. The status
		// word rides on each row here since the flat view has no status section
		// to state it.
		var order []string
		groups := map[string][]SessionInfo{}
		for _, s := range o.ordered() {
			key := o.cwdLabel(s)
			if _, seen := groups[key]; !seen {
				order = append(order, key)
			}
			groups[key] = append(groups[key], s)
		}
		for i, key := range order {
			if i > 0 {
				lines = append(lines, "")
			}
			header(key)
			for _, s := range groups[key] {
				appendRow(s, true)
			}
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
		header(st.String())
		for _, s := range group {
			// The section header already states the status, so the row omits it.
			appendRow(s, false)
		}
	}
	return lines, selLine
}

// cwdLabel is the cwd group key/header text for a session: its own Cwd when
// set, else the app-wide cwd from the header context (the common single-dir
// case, and the fallback for disk-only rows with no journaled cwd).
func (o Overview) cwdLabel(s SessionInfo) string {
	if s.Cwd != "" {
		return s.Cwd
	}
	return o.meta.Cwd
}

// row renders one session as a single line: selection caret, status glyph,
// title, a one-line summary (optionally prefixed with the status word when the
// enclosing view has no status section to state it), and a right-aligned age.
func (o Overview) row(s SessionInfo, width int, showStatus bool) string {
	caret := " "
	if s.ID == o.selectedID {
		caret = "▸"
	}
	prefix := padTo(fmt.Sprintf("%s %s", caret, o.statusGlyph(s)), rowPrefixW)

	right := o.age(s)
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

	body := s.Summary
	if showStatus {
		body = effectiveStatus(s).String() + " · " + s.Summary
	}

	title := padTo(s.Title, titleW)
	summary := o.theme.MutedStyle().Render(padTo(body, summaryW))
	line := prefix + title + strings.Repeat(" ", rowColGap) + summary + padLeft(right, rowRightW)

	if s.ID == o.selectedID {
		return o.theme.AccentStyle().Render(line)
	}
	return line
}

// statusGlyph renders a session's state-colored ● marker (marker-only
// styling): yellow while working or awaiting input (a pending approval keeps
// it yellow too — see effectiveStatus), green once finished. A pending
// permission request additionally appends its live count, e.g. "●2", clamped
// to a single digit ("●9+") so the marker never grows past two columns and
// skews row alignment (rowPrefixW budgets exactly one extra column for it).
func (o Overview) statusGlyph(s SessionInfo) string {
	style := o.theme.WarnStyle()
	if effectiveStatus(s) == StatusFinished {
		style = o.theme.OKStyle()
	}
	marker := style.Render(o.theme.GlyphAgent)
	if s.Pending > 0 {
		if s.Pending > 9 {
			return marker + "+"
		}
		return marker + fmt.Sprintf("%d", s.Pending)
	}
	return marker
}

// age renders the right-aligned compact relative age for a row.
func (o Overview) age(s SessionInfo) string {
	return humanAge(o.meta.Now.Sub(s.Updated))
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

// humanDuration renders a duration as a long-form string ("just now",
// "2 minutes", "3 hours", "1 day") for the peek card's status line, pluralizing
// the unit.
func humanDuration(d time.Duration) string {
	plural := func(n int, unit string) string {
		if n == 1 {
			return fmt.Sprintf("%d %s", n, unit)
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour")
	default:
		return plural(int(d.Hours()/24), "day")
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

// padTo returns s padded with trailing spaces to exactly w terminal cells
// (display width), truncating with an ellipsis when wider.
func padTo(s string, w int) string {
	sw := ansi.StringWidth(s)
	if sw == w {
		return s
	}
	if sw > w {
		return truncate(s, w)
	}
	return s + strings.Repeat(" ", w-sw)
}

// padLeft returns s padded with leading spaces to exactly w terminal cells
// (display width), truncating with an ellipsis when wider.
func padLeft(s string, w int) string {
	sw := ansi.StringWidth(s)
	if sw > w {
		return truncate(s, w)
	}
	return strings.Repeat(" ", w-sw) + s
}
