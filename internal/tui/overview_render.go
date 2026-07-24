package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jedwards1230/gofer/internal/tui/theme"
	"github.com/jedwards1230/gofer/internal/versionskew"
)

// Row layout column budgets. The row is prefix + body(title, statusword ·
// summary) + a right-aligned age. Body flexes with terminal width.
const (
	rowPrefixW  = 2  // selection caret + blocked-gutter marker (state otherwise rides the status word's color, not a glyph)
	rowRightW   = 5  // right-aligned compact age ("now", "59m", "23h", "2d")
	rowTitleW   = 28 // title column before the summary
	rowColGap   = 2  // gap between title and summary
	rowIndentW  = 2  // per-depth title-column indent for a subagent row
	headerLines = 4  // app line, model·cwd line, counts line, blank
	dispatchH   = 3  // rule, input line, hint line

	// rowTallyW sizes the right column for a roster that HAS subagents in it:
	// docs/TUI.md's subagent row shape, "5m 9s · ↓ 214.7k tokens" (23 cells),
	// plus room for the widest elapsed/token forms [humanElapsed]/[humanTokens]
	// can produce and a visible gap before the summary — instead of rowRightW's
	// bare age. It is deliberately not the unconditional width; see
	// [Overview.layout].
	rowTallyW = 26

	// blockedMark is the gutter glyph a row carries when it, or anything below
	// it in the tree, is awaiting the user.
	blockedMark = "!"
)

// rosterLayout is the per-render row geometry the roster computes ONCE, in
// [Overview.layout], and hands to every [Overview.row] call. Both of its
// interesting fields are whole-roster facts a single row cannot answer for
// itself: how wide the right column is, and which rows have a blocked
// descendant.
type rosterLayout struct {
	// tree reports whether this roster carries any subagent at all — any row
	// with a parent link or an agent identity. It gates every tree affordance
	// (the wide tally column, the blocked gutter, the indent), so an ordinary
	// roster renders byte-identically to a build with none of this in it.
	tree bool
	// indent enables the per-depth title indent. Tree AND flat view only: the
	// grouped view's sections are status buckets, not a hierarchy (see
	// [Overview.ordered]).
	indent bool
	// rightW is the right column's width — rowRightW normally, rowTallyW for a
	// tree roster. Passed down rather than read as a const so the whole roster
	// is sized by one decision instead of each row re-deciding.
	rightW int
	// blocked is [blockedTree]'s rollup: session id -> this row or a descendant
	// is awaiting the user. Nil for a non-tree roster.
	blocked map[string]bool
}

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
		// Strip a leading "v" before re-adding one so a version that already
		// carries it — every Go pseudo-version (vX.Y.Z-0.<ts>-<sha>) and release
		// tag effectiveVersion() reports — renders "v0.3.1", not "vv0.3.1". A
		// version without the prefix (the test fixtures' "0.3.0") is unchanged.
		title += " " + th.MutedStyle().Render("v"+strings.TrimPrefix(meta.Version, "v"))
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

// header renders the app identity, model·cwd context, status counts, and a
// fourth line that is normally the blank separator but carries the stale-daemon
// banner when the roster came from an out-of-date daemon (see
// [Overview.skewSeparator]). Reusing the separator slot keeps the header a fixed
// [headerLines] tall — the banner costs no body rows and hit-testing/layout math
// is unchanged — and a non-skewed roster renders byte-identically.
func (o Overview) header(width int) []string {
	working, needsInput, finished := o.counts()
	counts := fmt.Sprintf("%d awaiting input · %d working · %d completed", needsInput, working, finished)
	return append(identityHeaderLines(o.theme, o.meta, width), truncate(o.theme.MutedStyle().Render(counts), width), o.skewSeparator(width))
}

// skewSeparator returns the header's fourth line: the stale-daemon banner when
// the daemon this roster came from is out of date relative to the running CLI,
// else the empty string the header has always ended on. The banner is the
// TUI-visible counterpart of the CLI's stderr version-skew warning — a warning
// printed before bubbletea takes the alt-screen is never seen, so a TUI user on
// a stale daemon (silently missing whatever the new build added) gets no signal
// without this. It renders every frame while skewed, so it persists rather than
// flashing once. Classification is shared with the CLI path
// (internal/versionskew), so the two surfaces never disagree on what counts as
// skewed: unknown/equal/newer daemons stay silent.
func (o Overview) skewSeparator(width int) string {
	switch versionskew.Classify(o.meta.Version, o.meta.DaemonVersion) {
	case versionskew.Older:
		return truncate(o.theme.WarnStyle().Render(fmt.Sprintf(
			"⚠ daemon is stale (%s < %s) — run: gofer daemon restart",
			shortVersion(o.meta.DaemonVersion), shortVersion(o.meta.Version))), width)
	case versionskew.Differs:
		return truncate(o.theme.WarnStyle().Render(fmt.Sprintf(
			"⚠ daemon is a different build (%s) — run: gofer daemon restart if it is stale",
			shortVersion(o.meta.DaemonVersion))), width)
	default:
		return ""
	}
}

// shortVersion collapses a Go pseudo-version (vX.Y.Z-0.<timestamp>-<sha>) to a
// readable vX.Y.Z-<short-sha> so the banner fits one line without the 14-digit
// timestamp; release tags (vX.Y.Z) and local "dev-<sha>" builds are already
// short and pass through unchanged. It is display-only — the authoritative
// comparison always runs on the full version (see [Overview.skewSeparator]).
func shortVersion(v string) string {
	i := strings.Index(v, "-0.")
	if i <= 0 {
		return v
	}
	base := v[:i]
	sha := v[strings.LastIndex(v, "-")+1:]
	if len(sha) > 7 {
		sha = sha[:7]
	}
	return base + "-" + sha
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
	var below int
	if scroll > 0 {
		// scrollTail clamps the offset internally; mirror the clamp to learn how
		// many lines the window it returns leaves below itself.
		if len(lines) > avail {
			below = min(scroll, len(lines)-avail)
		}
		lines = scrollTail(lines, avail, scroll)
	} else {
		lines, below = window(lines, selLine, avail)
	}
	return pad(o.overflowNote(lines, below, width), avail)
}

// overflowNote replaces the last visible line with a muted "↓ N more" when
// below rows fall off the bottom of the window, so a roster taller than its
// budget says how much it is hiding instead of just ending. The line it
// displaces is itself hidden, hence the +1.
//
// It is the tree's overflow signal above all: a deep tree can push a parent's
// children past the viewport, and a roster that silently stops is
// indistinguishable from one that has nothing more to show.
func (o Overview) overflowNote(lines []string, below, width int) []string {
	if below <= 0 || len(lines) == 0 {
		return lines
	}
	// Copy: window/scrollTail return sub-slices of the caller's backing array.
	out := append([]string(nil), lines...)
	out[len(out)-1] = truncate(o.theme.MutedStyle().Render(fmt.Sprintf("↓ %d more", below+1)), width)
	return out
}

// rows renders the roster into display lines and reports the line index of the
// selected row (-1 when nothing is selected). The flat view groups rows under a
// cwd header per working directory (recency within each group); the grouped
// view interleaves Working / Needs input / Finished section headers. Both
// interleave a blank line between groups.
func (o Overview) rows(width int) (lines []string, selLine int) {
	selLine = -1
	lay := o.layout()
	appendRow := func(s SessionInfo, showStatus bool) {
		if s.ID == o.selectedID {
			selLine = len(lines)
		}
		lines = append(lines, o.row(s, width, showStatus, lay))
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

// layout resolves the whole-roster render decisions [Overview.row] must not
// make per row: how wide the right column is, and which rows carry a blocked
// descendant.
//
// The right column widens ONLY for a roster that actually contains a subagent.
// At 80 columns rowRightW's bare age leaves ~43 cells of summary, and a
// permanently ~24-wide tally column would take half of that away from the most
// informative column on every ordinary roster — to show a token count nobody
// asked for. So the tally is a property of the roster, decided once: tree
// rosters get the tally, and a roster with no subagents renders byte-for-byte
// as it did before any of this existed.
func (o Overview) layout() rosterLayout {
	lay := rosterLayout{rightW: rowRightW}
	for _, s := range o.sessions {
		// Either half of the subagent link is enough: a `gofer run --agent`
		// session is agent-attributed without a parent, and an orphan child has a
		// parent link with no parent on screen.
		if s.ParentID != "" || s.Agent != "" {
			lay.tree = true
			break
		}
	}
	if lay.tree {
		lay.rightW = rowTallyW
		lay.indent = o.view == viewFlat
		lay.blocked = blockedTree(o.sessions)
	}
	return lay
}

// row renders one session as a single line: a selection caret, a blocked-tree
// gutter marker, the row label, a one-line summary, and a right-aligned age (or
// run/token tally, in a tree roster — see [Overview.layout]). In the flat view
// (showStatus) the summary is prefixed with the state-colored status word, since
// that view has no status section to state it; the color is the sole status
// signal — there is no leading glyph.
//
// lay carries the per-render decisions this row must not make for itself; see
// [rosterLayout].
func (o Overview) row(s SessionInfo, width int, showStatus bool, lay rosterLayout) string {
	caret := " "
	if s.ID == o.selectedID {
		caret = "▸"
	}
	// The blocked marker rides the existing 2-cell prefix column — the caret's
	// trailing space — so a pending approval anywhere below a row is visible on
	// the row itself without descending into it. Styled BEFORE padTo for the #61
	// reason below, and only composed when there IS a mark: styling an empty
	// string still emits escape codes, which would change every unblocked row's
	// bytes.
	//
	// On a SELECTED blocked row the marker's own color ends the accent run that
	// wraps the line (lipgloss does not re-open a style after a nested reset —
	// the same reason the existing status word ends it further along the row).
	// That is the intended trade: the caret still marks the selection, while the
	// marker is the one signal that must survive a glance across the roster.
	prefix := padTo(caret, rowPrefixW)
	if lay.blocked[s.ID] {
		prefix = padTo(caret+o.theme.WarnStyle().Render(blockedMark), rowPrefixW)
	}

	right := o.age(s)
	if lay.tree {
		right = o.tally(s)
	}
	bodyW := width - rowPrefixW - lay.rightW
	if bodyW < 1 {
		// Too narrow for the full row; show just the caret and label.
		return truncate(prefix+o.rowLabel(s), width)
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
	// The binary-skew mark rides it too, for the same reason: it is styled, so it
	// must be composed BEFORE the single padTo that sizes the column.
	//
	// The mark LEADS the summary text rather than trailing it. Trailing reads
	// more naturally but makes the mark the first thing truncate() eats in a
	// narrow terminal — and the mark is the anomaly signal, so it is exactly what
	// must survive. A summary is prose and degrades gracefully; a half-rendered
	// version string ("(v0.2…") tells an operator nothing.
	mark := o.binaryMark(s)
	summary := o.theme.MutedStyle().Render(padTo(s.Summary, summaryW))
	if mark != "" {
		summary = padTo(o.theme.WarnStyle().Render(mark+" ")+o.theme.MutedStyle().Render(s.Summary), summaryW)
	}
	if showStatus {
		word := effectiveStatus(s).String()
		styled := o.statusColorFor(effectiveStatus(s)).Render(word)
		// Only when there IS a mark: styling an empty string still emits the
		// style's escape codes, which would change every unskewed row's bytes and
		// break the styled goldens for a mark nobody can see.
		if mark != "" {
			styled += o.theme.WarnStyle().Render(" " + mark)
		}
		styled += o.theme.MutedStyle().Render(" · " + s.Summary)
		summary = padTo(styled, summaryW)
	}

	// The subagent indent lives INSIDE the title column, not ahead of the row, so
	// every other column stays aligned no matter how deep the tree goes. It is
	// clamped to half the column so a deep chain can never indent the label off
	// the screen.
	label := o.rowLabel(s)
	if lay.indent && s.Depth > 0 {
		label = strings.Repeat(" ", min(s.Depth*rowIndentW, titleW/2)) + label
	}

	title := padTo(label, titleW)
	line := prefix + title + strings.Repeat(" ", rowColGap) + summary + padLeft(right, lay.rightW)

	if s.ID == o.selectedID {
		return o.theme.AccentStyle().Render(line)
	}
	return line
}

// rowLabel is the identity text a row's title column carries: a CHILD session's
// agent id, else the session's title. A spawned session's identity is its role —
// "go-developer", "owner" — which is what makes a child row readable as
// something other than an anonymous session; its title is derived from the
// prompt its parent handed it and usually just restates the parent's task.
// docs/TUI.md's roster sketch names the agent for exactly this reason.
//
// It keys off ParentID, not Agent alone: a ROOT session can carry an agent id
// too (`gofer run --agent <name>` sets one so its tool-call events are
// attributed), and that session has a real title of its own that the operator
// chose. Substituting the agent id there would discard the more informative
// text to answer a "which child is this?" question nobody asked of a root row.
func (o Overview) rowLabel(s SessionInfo) string {
	if s.ParentID != "" && s.Agent != "" {
		return s.Agent
	}
	return s.Title
}

// tally renders a tree roster's right column — "5m 9s · ↓ 214.7k tokens", the
// row shape docs/TUI.md specifies for a subagent: how long the session has been
// running and what it has spent to get there, which is the question a fan-out
// tree exists to answer. Tokens sum the same four normalized counters the
// /stats rollup does (see stats.go), so the roster and the panel never disagree.
func (o Overview) tally(s SessionInfo) string {
	u := s.Usage
	total := u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
	return humanElapsed(sessionElapsed(s)) + " · ↓ " + humanTokens(total) + " tokens"
}

// sessionElapsed is how long a session has been running: last activity minus
// start. A row with no Created — a disk-only row, or any daemon that doesn't
// report one — reports 0 rather than an absurd duration measured from the zero
// time.
func sessionElapsed(s SessionInfo) time.Duration {
	if s.Created.IsZero() || !s.Updated.After(s.Created) {
		return 0
	}
	return s.Updated.Sub(s.Created)
}

// humanElapsed renders a run duration in two units at most ("9s", "5m 9s",
// "41m", "1h 2m", "2d 3h") — precise enough to compare sibling subagents, short
// enough to share the right column with a token tally. Seconds are dropped past
// ten minutes: they stop being information there, and they are what would push
// the column past its budget ("41m 40s" is the widest two-unit form).
func humanElapsed(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < 10*time.Minute:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh", int(d.Hours()/24), int(d.Hours())%24)
	}
}

// humanTokens renders a token count compactly ("847", "214.7k", "1.3M"). A
// roster row has no room for seven digits, and the exact number is the /usage
// panel's job.
func humanTokens(n int) string {
	switch {
	case n < 1_000:
		return strconv.Itoa(n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
}

// binaryMark renders a session's [SessionInfo.BinaryVersion] as a warn-colored
// row marker — "(v0.4.1)" — when, and only when, it differs from the version
// this app is running. [Overview.row] places it ahead of the summary text so it
// survives truncation in a narrow terminal.
//
// Under M6 process isolation each session runs in its own worker process, so a
// daemon upgrade does not migrate live sessions: they finish their turns on the
// binary they started with while new sessions come up on the new one. This mark
// is how that drain becomes visible to an operator instead of being a raw-wire
// fact (design §11's "session/list shows mixed binaryVersions"). It is
// deliberately CONDITIONAL: in the overwhelmingly common all-matching case an
// identical version stamped on every row carries no information and costs the
// summary column real estate, so only the anomaly renders. `gofer ps` shows the
// version unconditionally in its own BINARY column for the full picture.
//
// An empty version — an offline row, or any pre-M6 daemon that never sends the
// field — marks nothing rather than rendering as a difference from the app's
// version, which would light up every offline row in the --all view. Likewise a
// meta with no version of its own has nothing to compare against.
//
// The mark carries NO padding of its own — every caller owns the separator it
// needs. Baking a leading space in made the status-view join (`" " + mark`)
// render a double space between the status word and the mark.
func (o Overview) binaryMark(s SessionInfo) string {
	if s.BinaryVersion == "" || o.meta.Version == "" || s.BinaryVersion == o.meta.Version {
		return ""
	}
	return "(v" + s.BinaryVersion + ")"
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

	// Plain full-width rule (round-5 reverted the labeled shell-mode rule): shell
	// mode is signalled by the sigil leading the input line, not a rule label.
	rule := strings.Repeat("─", width)

	var line string
	switch {
	case o.input.Empty():
		// The empty dispatch bar is the ordinary prompt where a new session
		// starts, so it carries the `! for shell mode` discoverability hint (the
		// only place a user can learn `!` opens shell mode before typing it).
		line = "❯ " + o.theme.MutedStyle().Render("describe a task for a new session · ! for shell mode")
	case strings.HasPrefix(o.input.String(), "!"):
		// Shell mode: the sigil is the prompt — no `❯ ` glyph (round-5).
		line = shellInputLine(o.theme, o.input, "▏")
	default:
		line = "❯ " + shellInputLine(o.theme, o.input, "▏")
	}

	hint := o.theme.MutedStyle().Render(o.hintText())

	return []string{rule, truncate(line, width), truncate(hint, width)}
}

// hintText is the dispatch bar's one-line shortcut hint. A roster holding any
// subagent (see [Overview.layout]) swaps the trailing "? shortcuts" entry for
// the bulk stop binding; an ordinary roster's hint is byte-identical to what it
// has always been, the same "a tree affordance costs a flat roster nothing"
// rule the tally column and the blocked gutter follow.
//
// The swap is a WIDTH decision, not a taste one: the flat hint already spends
// ~72 of the 80 cells this line is budgeted at, and "ctrl-t stop agents" needs
// 18 more, so something has to yield. "? shortcuts" is the entry that yields
// because it is the only one with a second way in: "?" on an empty dispatch bar
// opens the /help panel (see [App.handleOverviewKey]), and so does typing
// /help, which the autocomplete popup offers on a bare "/". ctrl-t has no such
// alternative and names a destructive binding an operator needs to be able to
// find.
func (o Overview) hintText() string {
	// "ctrl-x×2" signals the two-press confirm: the first ctrl-x arms, the second
	// kills/archives (see [App.confirmDestroy]). The verb stays "kill" — the hint
	// is state-blind, so it names the common case; the confirm LINE names the
	// exact verb (archive for a finished session).
	const base = "enter open · space peek · tab toggle view · ctrl-x×2 kill"
	if o.layout().tree {
		return base + " · ctrl-t stop agents"
	}
	return base + " · ? shortcuts"
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

// plural renders a count with its unit, pluralized by adding an "s" past one
// ("1 subagent", "3 subagents") — shared by [humanDuration]'s long-form units
// and the bulk-stop status note (see [App.handleOverviewKey]).
func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
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
// visible, and reports how many lines fall BELOW the returned window — what
// [Overview.overflowNote] turns into the "↓ N more" indicator. A sel of -1
// (nothing selected) shows the top.
func window(lines []string, sel, n int) (out []string, below int) {
	if len(lines) <= n {
		return lines, 0
	}
	start := 0
	if sel >= n {
		start = sel - n + 1
	}
	if start > len(lines)-n {
		start = len(lines) - n
	}
	return lines[start : start+n], len(lines) - (start + n)
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
