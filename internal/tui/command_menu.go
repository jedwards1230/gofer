package tui

// command_menu.go implements the input autocomplete popup: a filtered,
// scrollable list rendered above the input line's rule (App.render, app.go)
// whenever [activeToken] finds an active sigil token in the buffer at the
// cursor. Two sources feed the same popup — the slash-command registry
// (`/status`) and the `@` file-mention candidate list (filemention.go) — via
// one [menuRow] row type, so the mechanics below (highlight, scroll window,
// Tab/Enter/Esc handling, rendering) exist once rather than once per sigil.
// Like [commandPanel]/[modelPickerView] it is a pure value — every method
// returns an updated copy, so a fixed key sequence renders identically in
// every golden test. Wired into both text-entry surfaces it applies to — the
// overview dispatch bar and the attach input, never peek's reply input or an
// open panel's own state — from App (see App.syncMenu, App.handleMenuKey,
// app.go).

import (
	"fmt"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// commandMenuMaxRows caps how many rows the popup shows at once before it
// scrolls.
const commandMenuMaxRows = 6

// menuCandidateLimit caps how many candidates a source may hand the popup.
// The command registry never approaches it; the `@` file list routinely does
// (a repo has thousands of paths), and [commandMenu.Lines] builds a rendered
// string per row on every frame — so the cap is what keeps a large repo's
// mention popup as cheap to render as the command one.
const menuCandidateLimit = 50

// menuRow is one popup row, whatever produced it.
type menuRow struct {
	// insert is what replaces the active token when the row is accepted —
	// the sigil included ("/status", "@internal/tui/app.go") — plus a
	// trailing space when the row expects more typing after it (a command
	// with an ArgHint, or a file mention, which is always followed by more
	// prompt).
	insert string

	label   string // display text in the left column
	summary string // right-hand description; empty for a file mention

	// cmd is the command an Enter-select runs, for a command row. nil for a
	// file mention, where Enter inserts the path instead of running anything
	// (see [App.runMenuSelection]).
	cmd *Command
}

// activeToken finds the active sigil token in buf at cursor (a rune index
// into buf, not a byte index): a "/" or "@" that is at buffer start or
// immediately preceded by whitespace, with no whitespace between it and
// cursor. It returns the sigil, the partial text typed after it, the rune
// index of the sigil itself — the token's replacement point
// [commandMenu.complete] needs — and whether a token is active at all.
//
// A sigil preceded by any non-space character (a backtick — "`/x", or
// mid-word — "foo/bar", "user@example.com") is literal text, not a token: ok
// is false. This holds because the scan below always lands on the start of
// the maximal whitespace-delimited run ending at cursor; that run's first
// rune is preceded by whitespace or nothing (buffer start) by construction,
// so checking whether the run itself starts with a sigil is exactly the
// trigger rule — a sigil anywhere else in the run is necessarily preceded by
// another non-space rune from the same run. This is the same rule the
// submitted-buffer prefixes use ([hasInputPrefix]), applied at a token
// boundary instead of at position 0.
func activeToken(buf string, cursor int) (sigil rune, partial string, start int, ok bool) {
	r := []rune(buf)
	if cursor < 0 {
		cursor = 0
	}
	if cursor > len(r) {
		cursor = len(r)
	}
	j := cursor
	for j > 0 && !unicode.IsSpace(r[j-1]) {
		j--
	}
	if j == cursor {
		return 0, "", 0, false
	}
	switch r[j] {
	case '/', '@':
		return r[j], string(r[j+1 : cursor]), j, true
	}
	return 0, "", 0, false
}

// commandToken is [activeToken] narrowed to the slash-command sigil — the
// grammar the command popup and its tests are written against.
func commandToken(buf string, cursor int) (partial string, start int, ok bool) {
	sigil, partial, start, ok := activeToken(buf, cursor)
	if !ok || sigil != '/' {
		return "", 0, false
	}
	return partial, start, true
}

// commandMenu is the autocomplete popup: the rows matching the active token,
// with one highlighted. The zero value is closed (no active token, nothing to
// show or act on).
type commandMenu struct {
	theme theme.Theme

	// start is the rune index of the active token's sigil in the buffer
	// commandMenu was built from — [commandMenu.complete]'s replacement
	// point.
	start int

	rows   []menuRow // filtered candidates, already ordered by their source
	cursor int       // highlighted row index into rows; meaningful only when len(rows) > 0
}

// newInputMenu returns the popup for buf/cursor: command rows from reg for a
// `/` token, file-mention rows from files (the cached candidate list, see
// filemention.go) for an `@` token, and the zero value (closed) when
// [activeToken] finds no token or nothing matches.
func newInputMenu(th theme.Theme, reg Registry, files []string, buf string, cursor int) commandMenu {
	sigil, partial, start, ok := activeToken(buf, cursor)
	if !ok {
		return commandMenu{}
	}
	var rows []menuRow
	switch sigil {
	case '/':
		rows = commandRows(reg.matching(partial))
	case '@':
		rows = fileRows(matchFilePaths(files, partial, menuCandidateLimit))
	}
	if len(rows) == 0 {
		return commandMenu{}
	}
	return commandMenu{theme: th, start: start, rows: rows, cursor: 0}
}

// newCommandMenu is [newInputMenu] with no file candidates — the command-only
// popup the registry-level tests exercise.
func newCommandMenu(th theme.Theme, reg Registry, buf string, cursor int) commandMenu {
	return newInputMenu(th, reg, nil, buf, cursor)
}

// commandRows converts matched commands to popup rows: "/name [arg]" in the
// left column, the command's Summary on the right, and the ArgHint trailing
// space on accept (ready for an argument) that a command without one skips
// (ready to submit).
func commandRows(cmds []Command) []menuRow {
	rows := make([]menuRow, len(cmds))
	for i, cmd := range cmds {
		label := "/" + cmd.Name
		insert := label
		if cmd.ArgHint != "" {
			label += " " + cmd.ArgHint
			insert += " "
		}
		rows[i] = menuRow{insert: insert, label: label, summary: cmd.Summary, cmd: &cmds[i]}
	}
	return rows
}

// fileRows converts matched paths to popup rows. A mention always accepts
// with a trailing space: unlike a command, a path is never the whole prompt —
// the user is mid-sentence and about to keep typing.
func fileRows(paths []string) []menuRow {
	rows := make([]menuRow, len(paths))
	for i, p := range paths {
		rows[i] = menuRow{insert: "@" + p + " ", label: "@" + p}
	}
	return rows
}

// open reports whether the menu has matches to show and act on — the gate
// [App.Update] checks before routing ↑/↓/Tab/Enter/Esc to
// [App.handleMenuKey] ahead of the per-screen handlers.
func (m commandMenu) open() bool { return len(m.rows) > 0 }

// moveDown moves the highlight one row down, clamped at the last row.
func (m commandMenu) moveDown() commandMenu {
	if m.cursor < len(m.rows)-1 {
		m.cursor++
	}
	return m
}

// moveUp moves the highlight one row up, clamped at the first row.
func (m commandMenu) moveUp() commandMenu {
	if m.cursor > 0 {
		m.cursor--
	}
	return m
}

// selectedRow returns the highlighted row, or the zero row and false when the
// menu is closed.
func (m commandMenu) selectedRow() (menuRow, bool) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return menuRow{}, false
	}
	return m.rows[m.cursor], true
}

// selected returns the highlighted COMMAND, and false when the menu is closed
// or the highlighted row is a file mention (which has nothing to run).
func (m commandMenu) selected() (Command, bool) {
	row, ok := m.selectedRow()
	if !ok || row.cmd == nil {
		return Command{}, false
	}
	return *row.cmd, true
}

// complete returns buf with the highlighted row's replacement text spliced in
// place of the active token — from m.start (the sigil, kept) through cursor.
// ok is false when nothing is highlighted (the menu is closed).
func (m commandMenu) complete(buf string, cursor int) (newBuf string, ok bool) {
	row, ok := m.selectedRow()
	if !ok {
		return buf, false
	}
	r := []rune(buf)
	if cursor > len(r) {
		cursor = len(r)
	}
	return string(r[:m.start]) + row.insert + string(r[cursor:]), true
}

// completionCursor returns the rune index the cursor should land at after
// [commandMenu.complete] splices the highlighted row in — right after the
// inserted replacement (including its trailing space, if any), not the end of
// the whole buffer, which may carry trailing text the splice left in place.
// ok is false when the menu is closed, mirroring complete's own ok.
func (m commandMenu) completionCursor() (cursor int, ok bool) {
	row, ok := m.selectedRow()
	if !ok {
		return 0, false
	}
	return m.start + len([]rune(row.insert)), true
}

// Lines renders the popup: a plain rule (matching the panel/dispatch bar's
// rule-based chrome, no lipgloss borders), then up to [commandMenuMaxRows]
// rows scrolled to keep the highlighted row visible, with a muted "↑ N
// more"/"↓ N more" affordance when the list overflows that window. Empty
// (nil) when closed — [App.render] treats that as "no rows to budget for".
func (m commandMenu) Lines(width int) []string {
	if !m.open() {
		return nil
	}
	if width < 1 {
		width = 1
	}

	nameW := 0
	for _, row := range m.rows {
		if w := len([]rune(row.label)); w > nameW {
			nameW = w
		}
	}

	rows := make([]string, len(m.rows))
	for i, row := range m.rows {
		marker := "  "
		if i == m.cursor {
			marker = "▸ "
		}
		// The label column is only padded when something follows it: a
		// summary-less row (every file mention) would otherwise render with
		// trailing spaces out to the widest label in the list.
		line := marker + row.label
		if row.summary != "" {
			line = marker + padTo(row.label, nameW) + "  " + row.summary
		}
		if i == m.cursor {
			line = m.theme.AccentStyle().Render(line)
		}
		rows[i] = truncate(line, width)
	}

	n := len(rows)
	visibleN := n
	if visibleN > commandMenuMaxRows {
		visibleN = commandMenuMaxRows
	}
	start := 0
	if n > visibleN {
		start = m.cursor - visibleN + 1
		if start < 0 {
			start = 0
		}
		if start > n-visibleN {
			start = n - visibleN
		}
	}

	lines := make([]string, 0, visibleN+3)
	lines = append(lines, strings.Repeat("─", width))
	if start > 0 {
		lines = append(lines, truncate(m.theme.MutedStyle().Render(fmt.Sprintf("↑ %d more", start)), width))
	}
	lines = append(lines, rows[start:start+visibleN]...)
	if hidden := n - start - visibleN; hidden > 0 {
		lines = append(lines, truncate(m.theme.MutedStyle().Render(fmt.Sprintf("↓ %d more", hidden)), width))
	}
	return lines
}

// syncMenu recomputes a.menu from whichever input buffer is live for the
// current screen — the overview dispatch bar or the attach input, the two
// surfaces the token grammar covers (peek's reply input and an open panel are
// always closed, see the default/a.panel cases below) — against the buffer's
// real cursor position ([inputBuffer.Cursor], inputbuf.go), so the popup
// tracks the active token wherever the cursor actually sits, not just
// end-of-buffer. [App.Update] calls this after every per-screen key handler
// returns, so a.menu always reflects the just-applied edit before the next
// key's precedence check (a.menu.open()) runs.
//
// It returns a [tea.Cmd] because an `@` token is the trigger for enumerating
// the cwd's files, which must happen OFF the Update loop
// ([App.fileCandidatesCmd]) — the popup is simply closed until that lands.
func (a App) syncMenu() (App, tea.Cmd) {
	if a.panel != nil {
		a.menu = commandMenu{}
		a.menuToken = false
		return a, nil
	}
	var buf inputBuffer
	switch a.scr {
	case screenOverview:
		buf = a.over.input
	case screenAttach:
		buf = a.sess.input
	default:
		a.menu = commandMenu{}
		a.menuToken = false
		return a, nil
	}
	// Reload the registry's markdown layer on the closed→open EDGE of the
	// command token — the moment the user types "/" — not on every sync.
	// syncMenu runs after every key press, so reloading unconditionally would
	// walk the commands directories once per keystroke; reloading never would
	// leave a file written after startup permanently invisible. See
	// [App.reloadUserCommands].
	//
	// This deliberately gates on [commandToken] — the `/`-only narrowing of
	// [activeToken] — and NOT on activeToken itself. There are no markdown
	// commands behind `@`, so an `@` mention must neither trigger the walk nor
	// latch menuToken. Two distinct costs if it did:
	//
	//   - Every mention pays a commands-directory walk it can never use, and
	//     an unloadable command file's skipped-file warning would surface as a
	//     status note in the middle of typing a path. This is the certain one,
	//     and what TestMentionTokenDoesNotReloadTheMarkdownLayer probes.
	//   - A latch set by `@` can eat the next `/`'s edge. Typing can't reach
	//     that (every transition between token kinds passes through a
	//     no-token sync, which clears the latch), but a PASTE can: pasting
	//     " /st" onto a buffer already ending in an `@` token lands one sync
	//     that sees a `/` token with the latch already set, and the reload is
	//     skipped.
	_, _, active := commandToken(buf.String(), buf.Cursor())
	if active && !a.menuToken {
		a = a.reloadUserCommands()
	}
	a.menuToken = active

	// The `@` half of the same grammar: an active mention token is what kicks
	// off the cwd enumeration, off the Update loop (filemention.go). It is the
	// file-candidate mirror of the markdown reload above — the same "once per
	// token, not once per keystroke" discipline, a different source.
	app, cmd := a.syncFileCandidates(buf)
	a = app
	a.menu = newInputMenu(a.theme, a.registry, a.files.paths, buf.String(), buf.Cursor())
	return a, cmd
}

// handleMenuKey applies one key press to the open menu, ahead of the
// per-screen handlers (dispatch precedence: panel > approval > decision >
// menu > active screen > global — see App.Update): ↓/↑ move the highlight, Tab
// completes the highlighted row into whichever buffer is live, Enter accepts
// it, and Esc closes the menu but keeps the typed text. Any other key isn't
// the menu's to consume — handled reports false and [App.Update] falls through
// to the normal per-screen handler, which (for a text/backspace key) mutates
// the buffer and the caller resyncs the menu via [App.syncMenu] on its own
// return path.
func (a App) handleMenuKey(msg tea.KeyPressMsg) (next tea.Model, cmd tea.Cmd, handled bool) {
	switch msg.Key().Code {
	case tea.KeyDown:
		a.menu = a.menu.moveDown()
		return a, nil, true
	case tea.KeyUp:
		a.menu = a.menu.moveUp()
		return a, nil, true
	case tea.KeyTab:
		app, cmd := a.completeMenu()
		return app, cmd, true
	case tea.KeyEnter:
		next, cmd = a.runMenuSelection()
		return next, cmd, true
	case tea.KeyEscape:
		a.menu = commandMenu{}
		return a, nil, true
	}
	return a, nil, false
}

// completeMenu applies Tab: splices the highlighted row's replacement into
// whichever buffer is live via [commandMenu.complete], places the cursor
// right after the spliced-in text ([commandMenu.completionCursor] — not the
// end of the whole buffer, which may carry trailing text the splice left in
// place), then resyncs the menu against the new buffer (closed when a
// trailing space was added — the ArgHint and file-mention cases; reopened,
// matching just that one command, otherwise).
func (a App) completeMenu() (App, tea.Cmd) {
	var buf inputBuffer
	switch a.scr {
	case screenOverview:
		buf = a.over.input
	case screenAttach:
		buf = a.sess.input
	default:
		return a, nil
	}
	newBuf, ok := a.menu.complete(buf.String(), buf.Cursor())
	if !ok {
		return a, nil
	}
	newCursor, _ := a.menu.completionCursor() // ok mirrors complete's above
	switch a.scr {
	case screenOverview:
		a.over = a.over.SetInputCursor(newBuf, newCursor)
	case screenAttach:
		a.sess = a.sess.SetInputCursor(newBuf, newCursor)
	}
	return a.syncMenu()
}

// runMenuSelection applies Enter on the open menu. A file-mention row has
// nothing to run: Enter inserts the path exactly as Tab does, keeping the
// prompt the user is composing. A command row clears whichever buffer is live
// (Run doesn't expect the still-typed "/name" text the way
// [App.dispatchSlash]'s caller already cleared it via Submit) and runs the
// highlighted command with no arguments — Enter here is name-only, matching
// the picker's "select, don't type args" affordance; an ArgHint command still
// takes typed arguments the normal way once Tab-completed and submitted as a
// whole line. The menu is always closed afterward, regardless of what Run
// does.
func (a App) runMenuSelection() (App, tea.Cmd) {
	cmd, ok := a.menu.selected()
	if !ok {
		if _, isRow := a.menu.selectedRow(); isRow {
			return a.completeMenu()
		}
		return a, nil
	}
	switch a.scr {
	case screenOverview:
		a.over = a.over.SetInput("")
	case screenAttach:
		a.sess = a.sess.SetInput("")
	}
	a.menu = commandMenu{}
	return cmd.Run(a, nil)
}
