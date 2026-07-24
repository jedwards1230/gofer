package tui

// resumepicker.go implements the /resume command-panel tab: a picker over
// every session on disk — live and offline alike — that Enter brings back
// under live supervision and attaches into.
//
// It is the panel sibling of modelpicker.go and shares its shape: a pure value
// whose methods each return an updated copy (so a fixed key sequence replays
// to the same rendered output in every golden test), a type-to-filter box like
// configView's, and ↑/↓ row navigation. Enter is intercepted one level up by
// [App.handlePanelKey] for the same reason the Model tab's is — resuming needs
// IO a pure value has no seam for.
//
// Where it deliberately differs from the Model tab:
//
//   - Its list is not compiled in and has no offline floor. There is no honest
//     way to guess what sessions exist, so the tab opens on an explicit
//     "Loading…" line and [App.listSessionsCmd] fills it in off the Update
//     loop. A load failure says so; it never renders an empty list as "no
//     sessions", which would be a claim the picker cannot back up.
//   - It WINDOWS its rows around the highlight rather than truncating at the
//     panel's body budget. A model catalog is a handful of rows; a session
//     store grows without bound, and a picker whose 12th row can never be
//     reached is not a picker.
//   - There is no free-text entry line. `/resume <id>` (command.go) is the
//     typed path, and it is a plain command argument rather than a second
//     buffer inside the panel.

import (
	"path/filepath"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// resumePickerView renders and drives the Resume tab.
type resumePickerView struct {
	theme theme.Theme

	// now is the reference instant relative ages are measured against — the
	// overview's [Overview.Now], captured at open time exactly like the Stats
	// tab's, so golden output stays deterministic across machines.
	now time.Time

	// live is the set of session ids the roster snapshot already holds. A live
	// session is still listable and still selectable (Resume is idempotent);
	// the mark exists so the user can tell "bring this back" from "jump to
	// this", which are the same keystroke but not the same action.
	live map[string]bool

	// sessions is the list, newest-first, resolved ONCE per panel open by
	// [App.listSessionsCmd] and applied through [resumePickerView.withSessions]
	// — never on the render path, which runs several times per keystroke and
	// must not issue an RPC.
	sessions []SessionRef

	// loaded distinguishes "the list is genuinely empty" from "the list has not
	// arrived yet", which render as different lines and must never be conflated.
	loaded bool
	// loadErr is the listing failure, if any, rendered in place of the rows.
	loadErr string

	filter string
	cursor int // index into filtered(); -1 = no row highlighted
}

// newResumePickerView returns a Resume tab in its pre-load state, with now and
// the live-id set captured at open time the way every other tab captures its
// inputs (command.go's openPanel).
func newResumePickerView(th theme.Theme, now time.Time, roster []SessionInfo) resumePickerView {
	live := make(map[string]bool, len(roster))
	for _, s := range roster {
		live[s.ID] = true
	}
	return resumePickerView{theme: th, now: now, live: live, cursor: -1}
}

// withSessions returns a copy of the view listing refs, newest-first, marked
// loaded. It is the sole application point for [sessionsListedMsg]'s success
// case.
//
// Unlike [modelPickerView.withCatalog] an EMPTY result is adopted, not ignored:
// there is no floor beneath this list, so empty genuinely means "this store has
// no sessions" and saying so is the honest answer. The highlight is dropped
// rather than carried across — the list arrives exactly once per open, so there
// is no prior selection for it to preserve.
func (v resumePickerView) withSessions(refs []SessionRef) resumePickerView {
	sorted := append([]SessionRef(nil), refs...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if !sorted[i].Updated.Equal(sorted[j].Updated) {
			return sorted[i].Updated.After(sorted[j].Updated)
		}
		return sorted[i].ID > sorted[j].ID
	})
	v.sessions = sorted
	v.loaded = true
	v.loadErr = ""
	v.cursor = -1
	return v
}

// withLoadError returns a copy of the view reporting err in place of its rows.
// A failed listing is loaded=true with a message: the picker is done waiting,
// and it has something true to say about why there is nothing to show.
func (v resumePickerView) withLoadError(err error) resumePickerView {
	v.sessions = nil
	v.loaded = true
	v.loadErr = err.Error()
	v.cursor = -1
	return v
}

// View renders the filter line plus a height-bounded window of rows,
// width-truncated — the same Renderable contract every other panel component
// follows ([testkit.Renderable]).
//
// A height of 0 renders nothing at all rather than indexing into an empty
// slice: [App.render] can hand a panel a zero body budget on a terminal too
// short to hold one, and the first frame before any WindowSizeMsg has that
// shape too. The `height > 0` guard on the truncation below is NOT redundant
// with that early return — a NEGATIVE height is [testkit.Renderable]'s
// "unbounded" convention, and without the sign check `lines[:height]` would
// panic on it (pinned by TestResumeNegativeHeightIsUnbounded).
func (v resumePickerView) View(width, height int) string {
	if height == 0 {
		return ""
	}
	lines := v.lines(height)
	if height > 0 && len(lines) > height {
		lines = lines[:height]
	}
	for i, l := range lines {
		lines[i] = truncate(l, width)
	}
	return strings.Join(lines, "\n")
}

// lines builds the filter row followed by either a single state line (loading,
// load error, empty store, or filtered-to-nothing) or the visible window of
// session rows. budget is the caller's total row allowance; a negative budget
// means "unbounded" (the [testkit.Renderable] convention View follows).
func (v resumePickerView) lines(budget int) []string {
	out := []string{v.filterLine()}
	if line, ok := v.stateLine(); ok {
		return append(out, line)
	}
	rows := v.filtered()
	// The filter row is already spent; whatever is left is for session rows.
	rowBudget := budget - len(out)
	if budget < 0 {
		rowBudget = len(rows)
	}
	start, end := v.window(len(rows), rowBudget)
	for i := start; i < end; i++ {
		out = append(out, v.rowLine(i, rows[i]))
	}
	return out
}

// window returns the half-open [start, end) slice of n rows to render for a
// budget of at most size lines, keeping the highlighted row inside it: the list
// scrolls only once the cursor would leave the window, so browsing the first
// screenful never moves anything under the user.
func (v resumePickerView) window(n, size int) (int, int) {
	if size <= 0 || n == 0 {
		return 0, 0
	}
	if n <= size {
		return 0, n
	}
	start := 0
	if v.cursor >= size {
		start = v.cursor - size + 1
	}
	if start > n-size {
		start = n - size
	}
	return start, start + size
}

// stateLine returns the one line that REPLACES the row list, and whether there
// is one: the list has not arrived, arrived as an error, arrived empty, or the
// filter excluded everything. Each is a distinct fact and gets its own wording
// — collapsing "not loaded yet" into "no sessions" would assert something the
// picker does not know.
func (v resumePickerView) stateLine() (string, bool) {
	switch {
	case v.loadErr != "":
		return v.theme.DangerStyle().Render("Couldn't list sessions: " + v.loadErr), true
	case !v.loaded:
		return v.theme.MutedStyle().Render("Loading sessions…"), true
	case len(v.sessions) == 0:
		return v.theme.MutedStyle().Render("No sessions on disk yet."), true
	case len(v.filtered()) == 0:
		return v.theme.MutedStyle().Render("No sessions match."), true
	}
	return "", false
}

// filterLine renders the search box, mirroring [configView.filterLine].
func (v resumePickerView) filterLine() string {
	if v.filter == "" {
		return v.theme.MutedStyle().Render("Search sessions…")
	}
	return "Search: " + v.filter
}

// rowLine renders one session's row: marker, a "live" mark for a session
// already under supervision, the title, its short id, its last-active age, and
// the project directory's base name. Every segment that can be unknown says so
// rather than rendering an empty column.
func (v resumePickerView) rowLine(i int, ref SessionRef) string {
	marker := "  "
	selected := i == v.cursor
	if selected {
		marker = "▸ "
	}
	mark := "  "
	if v.live[ref.ID] {
		mark = "● "
	}
	title := ref.Title
	if title == "" {
		title = "(untitled)"
	}
	line := marker + mark + title + " · " + shortSessionID(ref.ID) + " · " + v.ageOf(ref)
	if base := filepath.Base(ref.Cwd); ref.Cwd != "" && base != "." {
		line += " · " + base
	}
	if selected {
		return v.theme.AccentStyle().Render(line)
	}
	return line
}

// ageOf renders ref's last-active age relative to v.now, or "age unknown" for a
// session whose journal carried no usable timestamp — never "now", which a zero
// Updated would otherwise produce for every legacy journal.
func (v resumePickerView) ageOf(ref SessionRef) string {
	if ref.Updated.IsZero() {
		return "age unknown"
	}
	return humanAge(v.now.Sub(ref.Updated))
}

// shortSessionIDLen is how many leading characters of a session id a picker row
// shows — the same width `gofer ps` abbreviates to (cmd/gofer's shortIDLen), so
// an id recognized in one surface is recognizable in the other. It is for
// RECOGNITION only: `/resume` takes a full id, and the picker's own Enter needs
// no id typed at all.
const shortSessionIDLen = 8

// shortSessionID truncates id to [shortSessionIDLen] characters. An id already
// that short is returned whole.
func shortSessionID(id string) string {
	if len(id) <= shortSessionIDLen {
		return id
	}
	return id[:shortSessionIDLen]
}

// filtered returns the sessions whose title, id, or cwd contains the filter
// text (case-insensitive), newest-first. An empty filter matches everything.
func (v resumePickerView) filtered() []SessionRef {
	if v.filter == "" {
		return v.sessions
	}
	q := strings.ToLower(v.filter)
	out := make([]SessionRef, 0, len(v.sessions))
	for _, s := range v.sessions {
		if strings.Contains(strings.ToLower(s.Title), q) ||
			strings.Contains(strings.ToLower(s.ID), q) ||
			strings.Contains(strings.ToLower(s.Cwd), q) {
			out = append(out, s)
		}
	}
	return out
}

// handleKey applies one key press: ↓/↑ move the row highlight, Backspace edits
// the filter, and any other text key types into it (dropping the highlight, for
// the same reason [configView.typeFilter] does — the result set reshapes on
// every keystroke, so the old index means nothing in the new one). Enter is a
// no-op here; [App.handleResumeSelect] (panel.go) intercepts it, since resuming
// is IO this pure value has no seam for. Esc is routed to
// [resumePickerView.handleEscape] by the panel host.
func (v resumePickerView) handleKey(msg tea.KeyPressMsg) resumePickerView {
	key := msg.Key()
	switch key.Code {
	case tea.KeyDown:
		return v.selectDown()
	case tea.KeyUp:
		return v.selectUp()
	case tea.KeyEnter:
		return v
	case tea.KeyBackspace:
		return v.backspaceFilter()
	}
	if key.Text != "" {
		return v.typeFilter(key.Text)
	}
	return v
}

// typeFilter appends text to the filter and drops the row highlight.
func (v resumePickerView) typeFilter(text string) resumePickerView {
	v.filter += text
	v.cursor = -1
	return v
}

// backspaceFilter removes the filter's last rune, if any, dropping the row
// highlight for the same reason [resumePickerView.typeFilter] does.
func (v resumePickerView) backspaceFilter() resumePickerView {
	if v.filter == "" {
		return v
	}
	r := []rune(v.filter)
	v.filter = string(r[:len(r)-1])
	v.cursor = -1
	return v
}

// handleEscape applies one Esc press, reporting whether it was consumed here
// (true, the panel stays open) or should bubble up and close the panel (false)
// — the same two-stage contract [configView.handleEscape] has, so clearing a
// filter doesn't also take the panel down.
func (v resumePickerView) handleEscape() (resumePickerView, bool) {
	if v.filter != "" {
		v.filter = ""
		v.cursor = -1
		return v, true
	}
	return v, false
}

// selectDown moves the highlight onto the first filtered row (from none) or one
// row further down, clamped at the last.
func (v resumePickerView) selectDown() resumePickerView {
	rows := v.filtered()
	if len(rows) == 0 {
		return v
	}
	if v.cursor < 0 {
		v.cursor = 0
		return v
	}
	if v.cursor < len(rows)-1 {
		v.cursor++
	}
	return v
}

// selectUp moves the highlight up one row, clamped at the first — the Model
// tab's behavior, not configView's "back to the filter box", since a dropped
// highlight here would leave Enter with nothing to commit.
func (v resumePickerView) selectUp() resumePickerView {
	if v.cursor > 0 {
		v.cursor--
	}
	return v
}

// selected returns the session Enter would resume, and whether there is one. It
// is the seam [App.handleResumeSelect] reads: nothing highlighted, an empty
// list, or a filter that excludes everything all report false, which the caller
// treats as a no-op rather than as an error.
func (v resumePickerView) selected() (SessionRef, bool) {
	rows := v.filtered()
	if v.cursor < 0 || v.cursor >= len(rows) {
		return SessionRef{}, false
	}
	return rows[v.cursor], true
}
