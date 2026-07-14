package tui

// panel.go is the command-panel host: a bottom overlay a slash command opens
// (see command.go), composed over whatever screen App is currently showing
// rather than being a fourth [screen]. Its routing mirrors the approval
// overlay's exactly (see dialog.go) — one field on [App], checked ahead of
// the per-screen key handlers in App.Update, and one insertion point in
// App.render(). M4 step 1 proved this seam with a one-line placeholder per
// tab; M4 step 2 lands the real /status body (see status.go) — Config and
// Model stay placeholders until their own steps.

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
// lower region of whichever screen it overlays: 4 fixed rows (two rules, the
// tab bar, the footer) plus up to panelBodyRows for the active tab's body —
// sized for /status's worst realistic case (two providers, both config
// layers present; gofer supports at most two providers today, see
// runner.SupportedProviders).
const (
	panelBodyRows = 10
	panelHeight   = panelBodyRows + 4
)

// commandPanel is the bottom panel a slash command opens: a tab bar plus the
// active tab's body. Like [Overview]/[Model]/[Peek] it is a pure value —
// every method returns an updated copy — so a fixed key sequence renders
// identically in every golden test.
type commandPanel struct {
	theme  theme.Theme
	active commandPanelTab

	// env, sess, and defaultModel are the data the Status tab's [statusView]
	// reads (see status.go); the Config/Model placeholders ignore them until
	// their own steps land.
	env          CommandEnv
	sess         *SessionInfo
	defaultModel string
}

// newCommandPanel returns a panel open on tab, rendering through th, with env
// and the current session snapshot (nil on the overview) captured at open
// time for the Status tab to read.
func newCommandPanel(th theme.Theme, tab commandPanelTab, env CommandEnv, sess *SessionInfo, defaultModel string) commandPanel {
	return commandPanel{theme: th, active: tab, env: env, sess: sess, defaultModel: defaultModel}
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

// panelFixedRows is the row count every tab spends on chrome — two rules,
// the tab bar, and the footer — before the active tab's body gets whatever
// remains of the panel's height budget.
const panelFixedRows = 4

// View renders the panel's tab bar and active-tab body at the given size,
// clipped to at most panelHeight rows. The body may itself be multiple
// lines (the Status tab's [statusView] is); each is truncated to width and
// the whole block is capped to the rows left after the fixed chrome.
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
	tabBar := truncate(p.tabBar(), width)
	footer := truncate(p.theme.MutedStyle().Render("←/→ to switch tabs · esc to close"), width)

	bodyRows := h - panelFixedRows
	if bodyRows < 0 {
		bodyRows = 0
	}
	var bodyLines []string
	if text := p.body(width, bodyRows); text != "" {
		bodyLines = strings.Split(text, "\n")
		if len(bodyLines) > bodyRows {
			bodyLines = bodyLines[:bodyRows]
		}
		for i, l := range bodyLines {
			bodyLines[i] = truncate(l, width)
		}
	}

	lines := make([]string, 0, panelFixedRows+len(bodyLines))
	lines = append(lines, rule, tabBar, rule)
	lines = append(lines, bodyLines...)
	lines = append(lines, footer)

	if len(lines) > h {
		lines = lines[:h]
	}
	return strings.Join(lines, "\n")
}

// Height returns the number of rows p.View(width, panelHeight) will actually
// render — the fixed chrome plus however many lines the active tab's body
// needs, capped to panelHeight. [App.render] reserves exactly this many rows
// rather than always the worst-case panelHeight, so a short body (the
// Config/Model placeholders, or Status with little to report) doesn't steal
// screen space the roster above it could use.
func (p commandPanel) Height(width int) int {
	bodyRows := panelHeight - panelFixedRows
	lines := 0
	if text := p.body(width, bodyRows); text != "" {
		lines = strings.Count(text, "\n") + 1
		if lines > bodyRows {
			lines = bodyRows
		}
	}
	h := panelFixedRows + lines
	if h > panelHeight {
		h = panelHeight
	}
	return h
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

// body renders the active tab's content at the given width/bodyRows budget.
// The Status tab renders the real [statusView]; Config and Model still
// render their step-1 placeholder until their own steps land.
func (p commandPanel) body(width, bodyRows int) string {
	if p.active == panelStatus {
		v := statusView{theme: p.theme, env: p.env, sess: p.sess, defaultModel: p.defaultModel}
		return v.View(width, bodyRows)
	}
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
