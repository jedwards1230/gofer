package tui

// panel.go is the command-panel host: a bottom overlay a slash command opens
// (see command.go), composed over whatever screen App is currently showing
// rather than being a fourth [screen]. Its routing mirrors the approval
// overlay's exactly (see dialog.go) — one field on [App], checked ahead of
// the per-screen key handlers in App.Update, and one insertion point in
// App.render(). M4 step 1 proves this seam only: each tab's body is a
// placeholder naming the tab. The real /status, /config, and /model views
// replace the stub body in follow-up PRs without changing this host.

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// commandPanelTab identifies one tab in the command panel's tab bar.
type commandPanelTab int

const (
	panelStatus commandPanelTab = iota
	panelConfig
	panelModel
)

// panelTab pairs a commandPanelTab with its display label.
type panelTab struct {
	tab   commandPanelTab
	label string
}

// panelTabs is the fixed left-to-right tab order every command panel opens
// with, regardless of which tab the slash command that opened it targeted —
// once open, all three are reachable with ←/→.
var panelTabs = []panelTab{
	{panelStatus, "Status"},
	{panelConfig, "Config"},
	{panelModel, "Model"},
}

// panelHeight is the fixed number of rows the command panel occupies in the
// lower region of whichever screen it overlays.
const panelHeight = 8

// commandPanel is the bottom panel a slash command opens: a tab bar plus the
// active tab's body. Like [Overview]/[Model]/[Peek] it is a pure value —
// every method returns an updated copy — so a fixed key sequence renders
// identically in every golden test.
type commandPanel struct {
	theme  theme.Theme
	active commandPanelTab
}

// newCommandPanel returns a panel open on tab, rendering through th.
func newCommandPanel(th theme.Theme, tab commandPanelTab) commandPanel {
	return commandPanel{theme: th, active: tab}
}

// handleKey applies one key press to the panel. ←/→ move the active tab;
// every other key is swallowed — while the panel is open it commandeers all
// input. Esc is handled by the caller ([App.handlePanelKey]) instead of
// here, since closing the panel mutates App state (a.panel = nil) that this
// pure value doesn't hold.
func (p commandPanel) handleKey(msg tea.KeyPressMsg) commandPanel {
	switch msg.Key().Code {
	case tea.KeyRight:
		return p.moveTab(1)
	case tea.KeyLeft:
		return p.moveTab(-1)
	}
	return p
}

// moveTab shifts the active tab by delta, wrapping around panelTabs.
func (p commandPanel) moveTab(delta int) commandPanel {
	idx := 0
	for i, t := range panelTabs {
		if t.tab == p.active {
			idx = i
			break
		}
	}
	idx = (idx + delta + len(panelTabs)) % len(panelTabs)
	p.active = panelTabs[idx].tab
	return p
}

// View renders the panel's tab bar and active-tab body at the given size,
// clipped to at most panelHeight rows.
func (p commandPanel) View(width, height int) string {
	if width < 1 {
		width = 1
	}
	h := height
	if h > panelHeight {
		h = panelHeight
	}
	if h < 1 {
		h = 1
	}

	rule := strings.Repeat("─", width)
	lines := []string{
		rule,
		truncate(p.tabBar(), width),
		rule,
		truncate(p.body(), width),
		truncate(p.theme.MutedStyle().Render("←/→ to switch tabs · esc to close"), width),
	}

	if len(lines) > h {
		lines = lines[:h]
	}
	return strings.Join(lines, "\n")
}

// tabBar renders the tab labels, bracketing the active one.
func (p commandPanel) tabBar() string {
	parts := make([]string, len(panelTabs))
	for i, t := range panelTabs {
		label := t.label
		if t.tab == p.active {
			label = "[" + label + "]"
		}
		parts[i] = label
	}
	return strings.Join(parts, "  ")
}

// body renders the active tab's placeholder content. Each tab's real body
// (a statusView, configView, or the model picker) replaces this stub in a
// follow-up PR — the panel host itself doesn't change.
func (p commandPanel) body() string {
	for _, t := range panelTabs {
		if t.tab == p.active {
			return t.label + " — coming soon."
		}
	}
	return ""
}

// handlePanelKey routes a key press to the open command panel. [App.Update]
// calls this before the approval overlay and per-screen handlers whenever
// a.panel != nil, matching the dispatch precedence panel > approval > active
// screen > global.
func (a App) handlePanelKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.Key()
	switch {
	case key.Mod.Contains(tea.ModCtrl) && key.Code == 'c':
		return a, tea.Quit
	case key.Code == tea.KeyEscape:
		a.panel = nil
		return a, nil
	}
	p := a.panel.handleKey(msg)
	a.panel = &p
	return a, nil
}
