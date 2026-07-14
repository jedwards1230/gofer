package tui

// config_view.go implements the /config command-panel tab (M4 step 3): a
// search-list over the settings registry (settings.go) — a filter box, a
// scrolling "Label … value" list, and in-place editing by [SettingKind]. A
// committed edit calls [CommandEnv.SaveConfig] immediately (no separate save
// step), matching the reference cc-config UX. Like [statusView] it is a pure
// value: every method returns an updated copy, so a fixed key sequence
// replays to the same rendered output in every golden test. It never
// resolves a credential or hits a provider — env.Config/env.SaveConfig are
// local reads/writes only, so the view opens cleanly with zero providers
// authenticated (auth-independence, docs/projects/gofer-m4-command-views-plan.md §5).

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// configView renders and drives the Config tab. cursor is the index into
// filteredSettings() of the highlighted row; -1 means no row is highlighted
// (the filter box has focus). editing/editKey/editBuf track an in-progress
// string edit — the one setting kind that needs more than a single keypress
// to commit.
type configView struct {
	theme    theme.Theme
	env      CommandEnv
	settings []Setting

	// cfg is the working copy every edit reads and writes: loaded once from
	// env.Config() at open time (newConfigView), then updated in place by
	// each committed edit so a second edit composes on top of the first
	// without a re-read. env.SaveConfig persists it to disk on every commit.
	cfg config.Config

	filter string
	cursor int

	editing bool
	editKey string
	editBuf string

	// err holds the last env.SaveConfig error, if any, shown as a trailing
	// row until the next successful edit clears it. A read failure at open
	// time is recorded here too — non-fatal, matching statusView's
	// auth-independence contract: the view still opens and edits (against
	// the zero Config) rather than blocking.
	err string
}

// newConfigView returns a Config tab reading/writing through env, with its
// working copy loaded from env.Config() once at open time (a nil closure or
// a read error yields the zero Config — never a reason to block the view).
func newConfigView(th theme.Theme, env CommandEnv) configView {
	v := configView{theme: th, env: env, settings: settingsRegistry(), cursor: -1}
	if env.Config != nil {
		if cfg, err := env.Config(); err != nil {
			v.err = err.Error()
		} else {
			v.cfg = cfg
		}
	}
	return v
}

// View renders the filter box, the filtered settings list, and any trailing
// error, width-truncated and capped to height — the same Renderable contract
// every other panel/screen component follows ([testkit.Renderable]).
func (v configView) View(width, height int) string {
	lines := v.lines()
	if height >= 0 && len(lines) > height {
		lines = lines[:height]
	}
	for i, l := range lines {
		lines[i] = truncate(l, width)
	}
	return strings.Join(lines, "\n")
}

// lines builds the filter row, one row per filtered setting (or a "no
// match" line when the filter excludes everything), and a trailing error
// row when set.
func (v configView) lines() []string {
	out := []string{v.filterLine()}
	rows := v.filteredSettings()
	if len(rows) == 0 {
		out = append(out, v.theme.MutedStyle().Render("No settings match."))
		return out
	}
	for i, s := range rows {
		out = append(out, v.rowLine(i, s))
	}
	if v.err != "" {
		out = append(out, v.theme.DangerStyle().Render("Error: "+v.err))
	}
	return out
}

// filterLine renders the search box: the "Search settings…" placeholder
// when empty, else the typed filter text.
func (v configView) filterLine() string {
	if v.filter == "" {
		return v.theme.MutedStyle().Render("Search settings…")
	}
	return "Search: " + v.filter
}

// rowLine renders one setting's "Label … value" row, marking and
// accent-styling the highlighted row (i == cursor) and substituting an
// in-progress string edit's buffer for the committed value.
func (v configView) rowLine(i int, s Setting) string {
	marker := "  "
	selected := i == v.cursor
	if selected {
		marker = "▸ "
	}
	value := s.Get(v.cfg)
	if v.editing && v.editKey == s.Key {
		value = v.editBuf + "▏"
	}
	line := marker + s.Label + " … " + value
	if selected {
		return v.theme.AccentStyle().Render(line)
	}
	return line
}

// filteredSettings returns the registry rows whose Label or Key contains the
// filter text (case-insensitive), in registry order. An empty filter matches
// every row.
func (v configView) filteredSettings() []Setting {
	if v.filter == "" {
		return v.settings
	}
	q := strings.ToLower(v.filter)
	out := make([]Setting, 0, len(v.settings))
	for _, s := range v.settings {
		if strings.Contains(strings.ToLower(s.Label), q) || strings.Contains(strings.ToLower(s.Key), q) {
			out = append(out, s)
		}
	}
	return out
}

// handleKey applies one key press: typed text always filters (except while
// editing, where it edits the in-progress value instead); ↓/Enter move onto
// and then activate the highlighted row; ↑ walks back up to no row
// highlighted ("back to the tab bar" per the footer hint — ←/→ still switch
// tabs regardless of cursor state, handled one level up in
// [commandPanel.handleKey]); Backspace edits the filter. Esc is handled
// separately by [configView.handleEscape] — the panel host intercepts it
// before this method is ever called (see [App.handlePanelKey]).
func (v configView) handleKey(msg tea.KeyPressMsg) configView {
	key := msg.Key()
	if v.editing {
		return v.handleEditKey(key)
	}
	switch key.Code {
	case tea.KeyDown:
		return v.selectDown()
	case tea.KeyUp:
		return v.selectUp()
	case tea.KeyEnter:
		return v.activate()
	case tea.KeyBackspace:
		return v.backspaceFilter()
	}
	if key.Text != "" {
		return v.typeFilter(key.Text)
	}
	return v
}

// handleEscape applies one Esc press, reporting whether it was consumed here
// (true, the panel stays open) or should bubble to the panel host to close
// (false). Priority: cancel an in-progress string edit first, then clear a
// non-empty filter — the "Esc clears filter then closes" contract, with
// edit-cancel as the finer-grained stage a partially typed value needs before
// falling back to the filter.
func (v configView) handleEscape() (configView, bool) {
	if v.editing {
		v.editing = false
		v.editKey = ""
		v.editBuf = ""
		return v, true
	}
	if v.filter != "" {
		v.filter = ""
		v.cursor = -1
		return v, true
	}
	return v, false
}

// typeFilter appends text to the filter buffer and drops the row highlight —
// the result set can change shape on every keystroke, so the previous
// highlighted index is not meaningful for the new one.
func (v configView) typeFilter(text string) configView {
	v.filter += text
	v.cursor = -1
	return v
}

// backspaceFilter removes the last rune from the filter buffer, if any, and
// drops the row highlight for the same reason [configView.typeFilter] does.
func (v configView) backspaceFilter() configView {
	if v.filter == "" {
		return v
	}
	r := []rune(v.filter)
	v.filter = string(r[:len(r)-1])
	v.cursor = -1
	return v
}

// selectDown moves the highlight onto the first row (from no highlight) or
// one row further down, clamped at the last row.
func (v configView) selectDown() configView {
	rows := v.filteredSettings()
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

// selectUp moves the highlight up one row, or drops it entirely (cursor =
// -1, "back to the tab bar") once already at the first row.
func (v configView) selectUp() configView {
	if v.cursor <= 0 {
		v.cursor = -1
		return v
	}
	v.cursor--
	return v
}

// activate applies Enter: with no row highlighted it highlights the first
// one (mirroring [configView.selectDown] — "Enter/↓ to select" in the
// footer), otherwise it edits the highlighted row per its [SettingKind].
func (v configView) activate() configView {
	rows := v.filteredSettings()
	if len(rows) == 0 {
		return v
	}
	if v.cursor < 0 {
		v.cursor = 0
		return v
	}
	if v.cursor >= len(rows) {
		v.cursor = len(rows) - 1
	}
	return v.edit(rows[v.cursor])
}

// edit dispatches to the right affordance for s.Kind: a bool toggles and
// saves immediately, an enum cycles and saves immediately, a string opens an
// inline edit line (committed separately by Enter in [configView.handleEditKey]).
func (v configView) edit(s Setting) configView {
	switch s.Kind {
	case SettingBool:
		return v.toggleBool(s)
	case SettingEnum:
		return v.cycleEnum(s)
	case SettingString:
		return v.startEdit(s)
	}
	return v
}

// toggleBool flips s between "true" and "false" and saves immediately.
func (v configView) toggleBool(s Setting) configView {
	next := "true"
	if s.Get(v.cfg) == "true" {
		next = "false"
	}
	return v.applyAndSave(s.Set(v.cfg, next))
}

// cycleEnum advances s to its next Option, wrapping, and saves immediately.
func (v configView) cycleEnum(s Setting) configView {
	if len(s.Options) == 0 {
		return v
	}
	cur := s.Get(v.cfg)
	idx := 0
	for i, o := range s.Options {
		if o == cur {
			idx = i
			break
		}
	}
	next := s.Options[(idx+1)%len(s.Options)]
	return v.applyAndSave(s.Set(v.cfg, next))
}

// startEdit opens s's inline edit line, seeded with its current value.
func (v configView) startEdit(s Setting) configView {
	v.editing = true
	v.editKey = s.Key
	v.editBuf = s.Get(v.cfg)
	return v
}

// handleEditKey applies one key press while a string edit is in progress:
// Enter commits (via [configView.commitEdit]), Backspace edits the buffer,
// any other text appends to it. Esc cancels — handled by
// [configView.handleEscape], not here.
func (v configView) handleEditKey(key tea.Key) configView {
	switch key.Code {
	case tea.KeyEnter:
		return v.commitEdit()
	case tea.KeyBackspace:
		if v.editBuf != "" {
			r := []rune(v.editBuf)
			v.editBuf = string(r[:len(r)-1])
		}
		return v
	}
	if key.Text != "" {
		v.editBuf += key.Text
	}
	return v
}

// commitEdit applies the in-progress edit buffer to its setting and saves
// immediately, then exits editing mode.
func (v configView) commitEdit() configView {
	s, ok := v.settingByKey(v.editKey)
	v.editing = false
	editBuf := v.editBuf
	v.editKey = ""
	v.editBuf = ""
	if !ok {
		return v
	}
	return v.applyAndSave(s.Set(v.cfg, editBuf))
}

// settingByKey looks up a registry row by its Key.
func (v configView) settingByKey(key string) (Setting, bool) {
	for _, s := range v.settings {
		if s.Key == key {
			return s, true
		}
	}
	return Setting{}, false
}

// applyAndSave updates the working copy to newCfg and persists it via
// env.SaveConfig (a nil closure — the zero CommandEnv — is a no-op, matching
// every other CommandEnv field's nil-safe contract). A write failure is
// recorded in err and shown as a trailing row rather than reverting the
// in-memory edit or blocking further edits.
func (v configView) applyAndSave(newCfg config.Config) configView {
	v.cfg = newCfg
	v.err = ""
	if v.env.SaveConfig != nil {
		if err := v.env.SaveConfig(newCfg); err != nil {
			v.err = err.Error()
		}
	}
	return v
}
