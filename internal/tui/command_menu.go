package tui

// command_menu.go implements the slash-command autocomplete popup: a
// filtered, scrollable list of registry commands rendered above the input
// line's rule (App.render, app.go) whenever [commandToken] finds an active
// command token in the buffer at the cursor. Like [commandPanel]/
// [modelPickerView] it is a pure value — every method returns an updated
// copy, so a fixed key sequence renders identically in every golden test.
// Wired into both text-entry surfaces it applies to — the overview dispatch
// bar and the attach input, never peek's reply input or an open panel's own
// state — from App (see App.syncMenu, App.handleMenuKey, app.go).

import (
	"fmt"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// commandMenuMaxRows caps how many command rows the popup shows at once
// before it scrolls.
const commandMenuMaxRows = 6

// commandToken finds the active slash-command token in buf at cursor (a rune
// index into buf, not a byte index): a "/" that is at buffer start or
// immediately preceded by whitespace, with no whitespace between it and
// cursor. It returns the partial command name typed so far (without the
// leading "/"), the rune index of the "/" itself — the token's replacement
// point [commandMenu.complete] needs — and whether a token is active at all.
//
// A "/" preceded by any non-space character (a backtick — "`/x", or
// mid-word — "foo/bar") is literal text, not a command token: ok is false.
// This holds because the scan below always lands on the start of the
// maximal whitespace-delimited run ending at cursor; that run's first rune
// is preceded by whitespace or nothing (buffer start) by construction, so
// checking whether the run itself starts with "/" is exactly the trigger
// rule — a "/" anywhere else in the run is necessarily preceded by another
// non-space rune from the same run.
func commandToken(buf string, cursor int) (partial string, start int, ok bool) {
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
	if j == cursor || r[j] != '/' {
		return "", 0, false
	}
	return string(r[j+1 : cursor]), j, true
}

// commandMenu is the autocomplete popup: the commands matching the active
// token, Name-sorted with the first match highlighted. The zero value is
// closed (no active token, nothing to show or act on).
type commandMenu struct {
	theme theme.Theme

	// start is the rune index of the active token's leading "/" in the
	// buffer commandMenu was built from — [commandMenu.complete]'s
	// replacement point.
	start int

	rows   []Command // filtered candidates, Name-sorted ([Registry.matching])
	cursor int       // highlighted row index into rows; meaningful only when len(rows) > 0
}

// newCommandMenu returns the menu for buf/cursor against reg: the zero value
// (closed) when [commandToken] finds no active token or it matches no
// command, otherwise open on every matching command with the first one
// highlighted.
func newCommandMenu(th theme.Theme, reg Registry, buf string, cursor int) commandMenu {
	partial, start, ok := commandToken(buf, cursor)
	if !ok {
		return commandMenu{}
	}
	rows := reg.matching(partial)
	if len(rows) == 0 {
		return commandMenu{}
	}
	return commandMenu{theme: th, start: start, rows: rows, cursor: 0}
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

// selected returns the highlighted command, or the zero Command and false
// when the menu is closed.
func (m commandMenu) selected() (Command, bool) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return Command{}, false
	}
	return m.rows[m.cursor], true
}

// complete returns buf with the highlighted command's Name spliced in place
// of the active token — from m.start (the leading "/", kept) through cursor
// — appending a trailing space when the command carries an ArgHint (ready
// for an argument) and leaving the buffer as-is otherwise (ready to submit).
// ok is false when nothing is highlighted (the menu is closed).
func (m commandMenu) complete(buf string, cursor int) (newBuf string, ok bool) {
	cmd, ok := m.selected()
	if !ok {
		return buf, false
	}
	r := []rune(buf)
	if cursor > len(r) {
		cursor = len(r)
	}
	repl := "/" + cmd.Name
	if cmd.ArgHint != "" {
		repl += " "
	}
	return string(r[:m.start]) + repl + string(r[cursor:]), true
}

// completionCursor returns the rune index the cursor should land at after
// [commandMenu.complete] splices the highlighted command's Name in — right
// after the inserted replacement (the name, plus the ArgHint trailing
// space, if any), not the end of the whole buffer, which may carry trailing
// text the splice left in place after the original cursor. ok is false when
// the menu is closed, mirroring complete's own ok.
func (m commandMenu) completionCursor() (cursor int, ok bool) {
	cmd, ok := m.selected()
	if !ok {
		return 0, false
	}
	repl := "/" + cmd.Name
	if cmd.ArgHint != "" {
		repl += " "
	}
	return m.start + len([]rune(repl)), true
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
	names := make([]string, len(m.rows))
	for i, cmd := range m.rows {
		n := "/" + cmd.Name
		if cmd.ArgHint != "" {
			n += " " + cmd.ArgHint
		}
		names[i] = n
		if w := len([]rune(n)); w > nameW {
			nameW = w
		}
	}

	rows := make([]string, len(m.rows))
	for i, cmd := range m.rows {
		marker := "  "
		if i == m.cursor {
			marker = "▸ "
		}
		line := marker + padTo(names[i], nameW) + "  " + cmd.Summary
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
// surfaces the command-token grammar covers (peek's reply input and an open
// panel are always closed, see the default/a.panel cases below) — against
// the buffer's real cursor position ([inputBuffer.Cursor], inputbuf.go), so
// the popup tracks the active token wherever the cursor actually sits, not
// just end-of-buffer. [App.Update] calls this after every per-screen key
// handler returns, so a.menu always reflects the just-applied edit before
// the next key's precedence check (a.menu.open()) runs.
func (a App) syncMenu() App {
	if a.panel != nil {
		a.menu = commandMenu{}
		return a
	}
	var buf inputBuffer
	switch a.scr {
	case screenOverview:
		buf = a.over.input
	case screenAttach:
		buf = a.sess.input
	default:
		a.menu = commandMenu{}
		return a
	}
	a.menu = newCommandMenu(a.theme, a.registry, buf.String(), buf.Cursor())
	return a
}

// handleMenuKey applies one key press to the open command menu, ahead of the
// per-screen handlers (dispatch precedence: panel > approval > menu > active
// screen > global — see App.Update): ↓/↑ move the highlight, Tab completes
// the highlighted command's Name into whichever buffer is live, Enter runs
// it, and Esc closes the menu but keeps the typed text. Any other key isn't
// the menu's to consume — handled reports false and [App.Update] falls
// through to the normal per-screen handler, which (for a text/backspace key)
// mutates the buffer and the caller resyncs the menu via [App.syncMenu] on
// its own return path.
func (a App) handleMenuKey(msg tea.KeyPressMsg) (next tea.Model, cmd tea.Cmd, handled bool) {
	switch msg.Key().Code {
	case tea.KeyDown:
		a.menu = a.menu.moveDown()
		return a, nil, true
	case tea.KeyUp:
		a.menu = a.menu.moveUp()
		return a, nil, true
	case tea.KeyTab:
		return a.completeMenu(), nil, true
	case tea.KeyEnter:
		next, cmd = a.runMenuSelection()
		return next, cmd, true
	case tea.KeyEscape:
		a.menu = commandMenu{}
		return a, nil, true
	}
	return a, nil, false
}

// completeMenu applies Tab: splices the highlighted command's Name into
// whichever buffer is live via [commandMenu.complete], places the cursor
// right after the spliced-in text ([commandMenu.completionCursor] — not the
// end of the whole buffer, which may carry trailing text the splice left in
// place), then resyncs the menu against the new buffer (closed when a
// trailing space was added — the ArgHint case; reopened, matching just that
// one command, otherwise).
func (a App) completeMenu() App {
	var buf inputBuffer
	switch a.scr {
	case screenOverview:
		buf = a.over.input
	case screenAttach:
		buf = a.sess.input
	default:
		return a
	}
	newBuf, ok := a.menu.complete(buf.String(), buf.Cursor())
	if !ok {
		return a
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

// runMenuSelection applies Enter on the open menu: clears whichever buffer
// is live (Run doesn't expect the still-typed "/name" text the way
// [App.dispatchSlash]'s caller already cleared it via Submit) and runs the
// highlighted command with no arguments — Enter here is name-only, matching
// the picker's "select, don't type args" affordance; an ArgHint command
// still takes typed arguments the normal way once Tab-completed and
// submitted as a whole line. The menu is always closed afterward, regardless
// of what Run does.
func (a App) runMenuSelection() (App, tea.Cmd) {
	cmd, ok := a.menu.selected()
	if !ok {
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
