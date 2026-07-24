package tui

// help.go is /help's command-panel body: the commands and keys this build
// actually has, rendered from the live sources rather than a hand-maintained
// list.
//
//   - COMMANDS come straight from [Registry.List] — the same registry
//     [App.dispatchSlash] resolves against and the autocomplete popup filters.
//     A command registered anywhere (a builtin here, a plugin's
//     registerCommand in M7) appears in /help with no edit to this file, which
//     is what TestHelpListsANewlyRegisteredCommand pins.
//   - KEYS come from [keymap]. Its global rows are the same values
//     [dispatchGlobalKey] dispatches through; its per-screen rows are
//     descriptive and can drift from the screens' inline switches — see
//     keymap.go's doc, which is the honest statement of how live "live" is
//     here.
//
// The body scrolls (↑/↓) because the full table is far longer than the command
// panel's row budget (panelBodyRows) and a silently truncated help screen is
// worse than no help screen.

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// helpView renders the Help tab. Like every other panel body it is a pure
// value — handleKey returns an updated copy — so a fixed key sequence renders
// identically in every golden test.
type helpView struct {
	theme theme.Theme
	// reg is the live command registry, captured from [App] at panel-open time
	// the same way env/sess are.
	reg Registry
	// offset is the first visible line of [helpView.lines].
	offset int
}

// newHelpView returns the Help tab's initial state, scrolled to the top.
func newHelpView(th theme.Theme, reg Registry) helpView {
	return helpView{theme: th, reg: reg}
}

// handleKey applies one key press: ↑/↓ scroll a line at a time, PgUp/PgDn a
// screenful (approximated by the panel's own body budget, the only height this
// pure value can know without being told). Any other key is ignored — the Help
// tab has nothing to select or commit.
func (v helpView) handleKey(msg tea.KeyPressMsg) helpView {
	switch msg.Key().Code {
	case tea.KeyUp:
		return v.scroll(-1)
	case tea.KeyDown:
		return v.scroll(1)
	case tea.KeyPgUp:
		return v.scroll(-(panelBodyRows - 1))
	case tea.KeyPgDown:
		return v.scroll(panelBodyRows - 1)
	}
	return v
}

// scroll moves the viewport by delta lines, clamped so at least one line is
// always visible.
func (v helpView) scroll(delta int) helpView {
	v.offset += delta
	if max := len(v.lines()) - 1; v.offset > max {
		v.offset = max
	}
	if v.offset < 0 {
		v.offset = 0
	}
	return v
}

// View renders the windowed table, width-truncated and capped to height — the
// same Renderable contract every other panel body follows. A height of 0 (or
// less) renders nothing at all rather than indexing into the line slice: the
// panel's fixed chrome can consume the whole budget on a very short terminal,
// and a first frame arrives before any WindowSizeMsg has been seen.
func (v helpView) View(width, height int) string {
	if height <= 0 {
		return ""
	}
	lines := v.lines()
	if len(lines) == 0 {
		return ""
	}

	offset := v.offset
	if offset > len(lines)-1 {
		offset = len(lines) - 1
	}
	if offset < 0 {
		offset = 0
	}

	visible := height
	clipped := offset > 0 || len(lines)-offset > height
	if clipped {
		// One row goes to the scroll affordance, so the reader can tell a
		// truncated table from the whole of it.
		visible = height - 1
	}
	if visible < 0 {
		visible = 0
	}
	end := offset + visible
	if end > len(lines) {
		end = len(lines)
	}

	out := make([]string, 0, height)
	for _, l := range lines[offset:end] {
		out = append(out, truncate(l, width))
	}
	if clipped {
		out = append(out, truncate(v.theme.MutedStyle().Render(scrollHint(offset, len(lines)-end)), width))
	}
	return strings.Join(out, "\n")
}

// scrollHint renders the "there is more above/below" affordance, mirroring the
// autocomplete popup's own wording (command_menu.go).
func scrollHint(above, below int) string {
	var parts []string
	if above > 0 {
		parts = append(parts, fmt.Sprintf("↑ %d more", above))
	}
	if below > 0 {
		parts = append(parts, fmt.Sprintf("↓ %d more", below))
	}
	return strings.Join(parts, " · ")
}

// lines builds the whole table: the command section from the live registry,
// then one section per key scope. Sections with no rows are omitted rather
// than rendered as an empty heading.
func (v helpView) lines() []string {
	var out []string
	if cmds := v.commandRows(); len(cmds) > 0 {
		out = append(out, "Commands")
		out = append(out, cmds...)
	}
	for _, scope := range keyScopeOrder {
		rows := v.keyRows(scope)
		if len(rows) == 0 {
			continue
		}
		out = append(out, scope.label())
		out = append(out, rows...)
	}
	return out
}

// commandRows renders one indented row per registered, non-hidden command:
// "/name [args]" against its Summary, with any aliases as a suffix on the
// SUMMARY rather than the name.
//
// The name column is padded to its widest entry, so anything in it is paid for
// by every other row. `/thinking [low|medium|high|off]` is already 31 columns;
// adding `(/effort)` to it pushed the column past half the 80-column floor and
// truncated every summary in the table. Aliases are worth showing — /help is
// the one place a user learns /cfg exists — but not at that price, so they ride
// on the summary side where only their own row pays.
func (v helpView) commandRows() []string {
	cmds := v.reg.List()
	names := make([]string, len(cmds))
	summaries := make([]string, len(cmds))
	for i, cmd := range cmds {
		name := "/" + cmd.Name
		if cmd.ArgHint != "" {
			name += " " + cmd.ArgHint
		}
		names[i] = name

		summary := cmd.Summary
		for _, alias := range cmd.Aliases {
			summary = strings.TrimSpace(summary + " (/" + alias + ")")
		}
		summaries[i] = summary
	}
	return helpRows(names, summaries)
}

// keyRows renders the bindings declared for scope.
func (v helpView) keyRows(scope keyScope) []string {
	var keys, descs []string
	for _, b := range keymap() {
		if b.Scope != scope {
			continue
		}
		keys = append(keys, b.Keys)
		descs = append(descs, b.Desc)
	}
	return helpRows(keys, descs)
}

// helpRows pads lefts to a common column and joins each with its right-hand
// text, indented two spaces under its section heading.
func helpRows(lefts, rights []string) []string {
	w := 0
	for _, l := range lefts {
		if n := len([]rune(l)); n > w {
			w = n
		}
	}
	out := make([]string, len(lefts))
	for i, l := range lefts {
		out[i] = "  " + padTo(l, w) + "  " + rights[i]
	}
	return out
}
