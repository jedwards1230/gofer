package tui

import (
	"fmt"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// Row layout column budgets. The row is prefix + body(title, statusword ·
// summary) + a right-aligned age. Body flexes with terminal width.
const (
	rowPrefixW  = 2  // selection caret + trailing space (state now rides the status word's color, not a glyph)
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
	return o.render(width, height, nil, 0, false)
}

// ViewWithMenu renders like View but splices menuLines — pre-rendered,
// already width-truncated rows from [commandMenu.Lines] — directly above the
// dispatch bar's rule, shrinking the roster body's row budget by the same
// amount so the frame still totals height rows, the same way the command
// panel and status footer already do in [App.render]. scroll is the roster's
// manual scroll-back offset (0 = tail-to-latest, the default — the selected
// row stays the anchor; see [Overview.body]); hideDispatch blanks the
// dispatch bar's three rows in place of its rule/input/hint while a command
// panel is open (see [Overview.dispatch]), since the panel then owns the
// bottom of the screen. Called only from App.render; a nil/empty menuLines
// with scroll 0 and hideDispatch false renders identically to View.
func (o Overview) ViewWithMenu(width, height int, menuLines []string, scroll int, hideDispatch bool) string {
	return o.render(width, height, menuLines, scroll, hideDispatch)
}

func (o Overview) render(width, height int, menuLines []string, scroll int, hideDispatch bool) string {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}

	var out []string
	out = append(out, o.header(width)...)

	bodyAvail := height - headerLines - dispatchH - len(menuLines)
	if bodyAvail < 0 {
		bodyAvail = 0
	}
	out = append(out, o.body(width, bodyAvail, scroll)...)
	out = append(out, menuLines...)
	out = append(out, o.dispatch(width, hideDispatch)...)

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
	out = append(out, o.body(width, bodyAvail, 0)...)

	if len(out) > height {
		out = out[:height]
	}
	for len(out) < height {
		out = append(out, "")
	}
	return strings.Join(out, "\n")
}

// identityHeaderLines renders the two lines every screen's chrome opens
// with: the app name + version, then `model · cwd` — [Overview.header]'s own
// title/context lines, and (via [attachHeaderLines] in app.go) the same two
// lines topping the attach screen, its approval prompts, and its command-
// menu/panel overlays, so every screen's identity chrome renders through one
// styling definition instead of two copies drifting apart.
func identityHeaderLines(th theme.Theme, meta OverviewMeta, width int) []string {
	app := meta.App
	if app == "" {
		app = "gofer"
	}
	title := th.AccentStyle().Render(app)
	if meta.Version != "" {
		title += " " + th.MutedStyle().Render("v"+meta.Version)
	}

	context := meta.Model
	if meta.Cwd != "" {
		if context != "" {
			context += " · "
		}
		context += meta.Cwd
	}

	return []string{truncate(title, width), truncate(th.MutedStyle().Render(context), width)}
}

// header renders the app identity, model·cwd context, and status counts.
func (o Overview) header(width int) []string {
	working, needsInput, finished := o.counts()
	counts := fmt.Sprintf("%d awaiting input · %d working · %d completed", needsInput, working, finished)
	return append(identityHeaderLines(o.theme, o.meta, width), truncate(o.theme.MutedStyle().Render(counts), width), "")
}

// body renders the roster rows for the current view. With scroll at its
// default of 0, the body windows the rows to keep the selected row visible
// (the existing selection-follow behavior, unchanged). A positive scroll —
// set by a mouse wheel or PgUp/PgDn (see App.handleWheel/handleOverviewKey)
// — overrides that and instead scrolls the row list back from its tail via
// [scrollTail], the same manual scroll-back the attach transcript uses (see
// Model.view), so the roster can be browsed independently of the selection.
func (o Overview) body(width, avail, scroll int) []string {
	if avail <= 0 {
		return nil
	}

	lines, selLine := o.rows(width)
	if len(lines) == 0 {
		empty := o.theme.MutedStyle().Render("No sessions yet — type below to start one.")
		return pad([]string{truncate(empty, width)}, avail)
	}
	if scroll > 0 {
		lines = scrollTail(lines, avail, scroll)
	} else {
		lines = window(lines, selLine, avail)
	}
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
		// The section header IS the status field in the grouped view, so it
		// carries the state color (yellow working/needs-input, green finished)
		// rather than the muted styling a plain cwd header gets.
		lines = append(lines, truncate(o.statusColorFor(st).Render(st.String()), width))
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

// row renders one session as a single line: a selection caret, the title, a
// one-line summary, and a right-aligned age. In the flat view (showStatus) the
// summary is prefixed with the state-colored status word, since that view has
// no status section to state it; the color is the sole status signal — there
// is no leading glyph.
func (o Overview) row(s SessionInfo, width int, showStatus bool) string {
	caret := " "
	if s.ID == o.selectedID {
		caret = "▸"
	}
	prefix := padTo(caret, rowPrefixW)

	right := o.age(s)
	bodyW := width - rowPrefixW - rowRightW
	if bodyW < 1 {
		// Too narrow for the full row; show just the caret and title.
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

	// The colored status word rides the summary column in the flat view; padTo
	// measures display width (ANSI-aware), so the color codes don't skew the
	// column — the #61 lesson, guarded by the styled-golden + color-layout tests.
	summary := o.theme.MutedStyle().Render(padTo(s.Summary, summaryW))
	if showStatus {
		word := effectiveStatus(s).String()
		styled := o.statusColorFor(effectiveStatus(s)).Render(word) + o.theme.MutedStyle().Render(" · "+s.Summary)
		summary = padTo(styled, summaryW)
	}

	title := padTo(s.Title, titleW)
	line := prefix + title + strings.Repeat(" ", rowColGap) + summary + padLeft(right, rowRightW)

	if s.ID == o.selectedID {
		return o.theme.AccentStyle().Render(line)
	}
	return line
}

// statusColorFor returns the state color a session's effective status renders
// in: yellow while working or awaiting input (a pending request keeps it
// yellow — see effectiveStatus), green once finished. Pending is a boolean
// folded into the status, not a count — one or many pending approvals both
// read as a plain "Needs input".
func (o Overview) statusColorFor(st SessionStatus) lipgloss.Style {
	if st == StatusFinished {
		return o.theme.OKStyle()
	}
	return o.theme.WarnStyle()
}

// age renders the right-aligned compact relative age for a row.
func (o Overview) age(s SessionInfo) string {
	return humanAge(o.meta.Now.Sub(s.Updated))
}

// dispatch renders the bottom dispatch bar: a rule, the input line (a
// placeholder until the user types), and a one-line shortcut hint. hide, set
// while a command panel is open (App.render passes a.panel != nil), blanks
// all three rows instead — the panel then owns the bottom of the screen, and
// the roster's own (un-typeable, while the panel claims every key) dispatch
// chrome would otherwise render redundantly beneath it. The row COUNT never
// changes either way, so the frame still totals the same height.
func (o Overview) dispatch(width int, hide bool) []string {
	if hide {
		return make([]string, dispatchH)
	}

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

// scrollTail returns at most avail lines from lines, scrolled back offset
// lines from the tail (the most recent content) — the shared manual
// scroll-back window behind both the attach transcript's header+transcript
// scroll ([Model.view]) and the roster's mouse-wheel/PgUp-PgDn scroll
// ([Overview.body]). offset 0 (the default, "tail-to-latest") is
// byte-identical to a plain trailing slice; a larger offset is clamped to
// [0, len(lines)-avail] so it can never run the window past the start of the
// content. avail <= 0 (no room at all — the zero-height first frame this
// package's #87 regression test guards) returns nil rather than slicing,
// matching the truncate-to-nothing a zero-width window already implies.
func scrollTail(lines []string, avail, offset int) []string {
	if avail <= 0 {
		return nil
	}
	if len(lines) <= avail {
		return lines
	}
	max := len(lines) - avail
	if offset < 0 {
		offset = 0
	}
	if offset > max {
		offset = max
	}
	end := len(lines) - offset
	start := end - avail
	return lines[start:end]
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
