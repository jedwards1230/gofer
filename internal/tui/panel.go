package tui

// panel.go is the command-panel host: a bottom overlay a slash command opens
// (see command.go), composed over whatever screen App is currently showing
// rather than being a fourth [screen]. Its routing mirrors the approval
// overlay's exactly (see dialog.go) — one field on [App], checked ahead of
// the per-screen key handlers in App.Update, and one insertion point in
// App.render(). M4 step 1 proved this seam with a one-line placeholder per
// tab; M4 step 2 landed the real /status body (status.go); M4 step 3 landed
// the real /config body (config_view.go); M4 step 4 lands the real /model
// body (modelpicker.go) and its Enter/select coupling
// ([App.handleModelSelect], below) — the final M4 command-view piece.

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/modelmeta"
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
// tab bar, the footer) plus up to panelBodyRows for the active tab's body.
// The binding case is the Model tab with both providers authenticated: its
// free-text entry line plus two provider headers plus the catalog's eight
// rows (gofer supports at most two providers today, see
// runner.SupportedProviders). /status's worst realistic case (two providers,
// both config layers present) fits inside the same budget. A tab whose body
// is shorter is not padded to it — [commandPanel.Height] reserves only the
// rows actually rendered. The Model tab's typed-candidate line can push one
// row past the budget mid-typing; that is deliberate, since it costs the last
// catalog row only while the user is typing an id rather than browsing the
// list, and growing the overlay permanently would cost the roster above it a
// row on every open.
const (
	panelBodyRows = 11
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
	// reads (see status.go) — the Model tab's [modelPickerView] (below) reads
	// the same three fields, captured once more into its own struct at open
	// time.
	env          CommandEnv
	sess         *SessionInfo
	defaultModel string

	// cfg is the Config tab's state (config_view.go), built from env at open
	// time the same way sess/defaultModel are — regardless of which tab the
	// panel opens on, so switching to Config mid-session shows a
	// consistent, once-loaded working copy rather than reloading on every
	// tab switch.
	cfg configView

	// model is the Model tab's state (modelpicker.go), built the same way as
	// cfg — env/sess/defaultModel captured once at open time.
	model modelPickerView
}

// newCommandPanel returns a panel open on tab, rendering through th, with env
// and the current session snapshot (nil on the overview) captured at open
// time for the Status tab to read, and the Config/Model tabs' working state
// loaded from env at the same time.
func newCommandPanel(th theme.Theme, tab commandPanelTab, env CommandEnv, sess *SessionInfo, defaultModel string) commandPanel {
	return commandPanel{
		theme:        th,
		active:       tab,
		env:          env,
		sess:         sess,
		defaultModel: defaultModel,
		cfg:          newConfigView(th, env),
		model:        newModelPickerView(th, env, sess, defaultModel),
	}
}

// handleKey applies one key press to the panel. ←/→ always move the active
// tab, regardless of what the active tab's own state is (an in-progress
// Config-tab edit, or the Model tab's row highlight, is simply left as-is on
// tab-away, same as any other unsaved buffer) — this is also why the Model
// tab's deferred effort-adjust has no room on ←/→ (see modelpicker.go).
// Every other key routes to the active tab's own handler; Status has none,
// matching its read-only, no-selection design. Esc is handled by the caller
// ([App.handlePanelKey]) via [commandPanel.handleEscape] instead of here,
// since closing the panel mutates App state (a.panel = nil) that this pure
// value doesn't hold.
func (p commandPanel) handleKey(msg tea.KeyPressMsg) commandPanel {
	switch msg.Key().Code {
	case tea.KeyRight:
		return p.moveTab(1)
	case tea.KeyLeft:
		return p.moveTab(-1)
	}
	switch p.active {
	case panelConfig:
		p.cfg = p.cfg.handleKey(msg)
	case panelModel:
		p.model = p.model.handleKey(msg)
	}
	return p
}

// handleEscape applies one Esc press to the panel, reporting whether it was
// consumed by the active tab's own state (ok=false, the panel stays open —
// the Config tab clearing a filter or canceling an in-progress edit
// ([configView.handleEscape]), or the Model tab discarding a half-typed model
// id ([modelPickerView.handleEscape])) or should close the panel (ok=true:
// the Status tab, and either of the other two once it has no state of its own
// left to clear).
func (p commandPanel) handleEscape() (commandPanel, bool) {
	switch p.active {
	case panelConfig:
		cfg, consumed := p.cfg.handleEscape()
		if consumed {
			p.cfg = cfg
			return p, false
		}
	case panelModel:
		model, consumed := p.model.handleEscape()
		if consumed {
			p.model = model
			return p, false
		}
	}
	return p, true
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
	footer := truncate(p.theme.MutedStyle().Render(p.footerText()), width)

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
// rather than always the worst-case panelHeight, so a short body (Status with
// little to report, or the Model tab's empty-list warning) doesn't steal
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

// footerText returns the active tab's footer hint. The Config tab's search
// list and the Model tab's free-text entry each have their own key contract,
// so both override the default "switch tabs / close" hint the read-only
// Status tab shows.
func (p commandPanel) footerText() string {
	switch p.active {
	case panelConfig:
		return "Type to filter · Enter/↓ to select · ↑ to tabs · Esc to clear"
	case panelModel:
		return "Type a model id · ↑/↓ to browse · Enter to select · Esc to clear"
	}
	return "←/→ to switch tabs · esc to close"
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

// body renders the active tab's content at the given width/bodyRows budget —
// every tab (Status, Config, Model) renders its real view.
func (p commandPanel) body(width, bodyRows int) string {
	switch p.active {
	case panelStatus:
		v := statusView{theme: p.theme, env: p.env, sess: p.sess, defaultModel: p.defaultModel}
		return v.View(width, bodyRows)
	case panelConfig:
		return p.cfg.View(width, bodyRows)
	case panelModel:
		return p.model.View(width, bodyRows)
	}
	return ""
}

// handlePanelKey routes a key press to the open command panel. [App.Update]
// calls this before the approval overlay and per-screen handlers whenever
// a.panel != nil, matching the dispatch precedence panel > approval > active
// screen > global. Esc goes through [commandPanel.handleEscape] rather than
// closing unconditionally, since the Config tab consumes a first Esc to
// clear its own state (an in-progress edit, then a filter) before a
// subsequent Esc actually closes the panel.
func (a App) handlePanelKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.Key()
	switch {
	case key.Mod.Contains(tea.ModCtrl) && key.Code == 'c':
		return a, tea.Quit
	case key.Code == tea.KeyEscape:
		p, closePanel := a.panel.handleEscape()
		if closePanel {
			a.panel = nil
			return a, nil
		}
		a.panel = &p
		return a, nil
	case key.Code == tea.KeyEnter && a.panel.active == panelModel:
		// The Model tab's Enter/select coupling needs IO (SetModel,
		// SaveConfig) [commandPanel]/[modelPickerView] can't do as pure
		// values — App is the client that does the calling (invariant #2),
		// so it intercepts Enter here instead of routing it into
		// commandPanel.handleKey below.
		return a.handleModelSelect()
	}
	p := a.panel.handleKey(msg)
	a.panel = &p
	return a, nil
}

// handleModelSelect applies Enter on the Model tab: the coupled /model
// select (docs/projects/gofer-m4-command-views-plan.md §4b). It always
// persists the selected id — the highlighted row's, or the free-text entry's
// when no row is highlighted ([modelPickerView.selectedModel]) — as the
// session.model config default. That id may be one the compiled-in registry
// does not carry, which is fine: [provider.Resolve] decides what can run, and
// modelProvider below (the only thing this function asks about the id) reads
// through it.
// this alone is possible with zero providers authenticated, so it keeps
// Enter auth-independent (§5) even when there is nothing else to do. When a
// session is attached or peeked (a.panel.model.sess, captured at open time —
// the same field [modelPickerView.activeModel] reads), the decision to also
// hot-swap that session's live model is made HERE, client-side, before ever
// calling the daemon: same provider (compared via the SDK's static catalog,
// [provider.Lookup]) swaps through [Supervisor.SetModel] — the swap applies
// on the session's next turn, not the one in flight — while a cross-provider
// pick leaves the running session on its current model (a session's provider
// is fixed at creation, see [Supervisor.SetModel]'s doc) and the status note
// explains why instead. Selecting nothing (no row highlighted, or the picker's
// empty/warn state) is a pure no-op — the panel stays open, untouched. Every
// other outcome closes the panel: Enter is a committing action here, matching
// the picker footer's "select" semantics, leaving the outcome in the
// transient a.status line.
func (a App) handleModelSelect() (tea.Model, tea.Cmd) {
	selected := a.panel.model.selectedModel()
	if selected == "" {
		return a, nil
	}

	var cfg config.Config
	if a.commandEnv.Config != nil {
		c, err := a.commandEnv.Config()
		if err != nil {
			// A read failure must NOT fall through to SaveConfig with a
			// zero-value config — that would overwrite config.json and drop
			// the user's permissions/telemetry settings. Surface it and abort,
			// preserving the on-disk state (mirrors the SaveConfig-error path).
			a.status = "couldn't load config: " + err.Error()
			a.panel = nil
			return a, nil
		}
		cfg = c
	}
	cfg.Session.Model = selected
	if a.commandEnv.SaveConfig != nil {
		if err := a.commandEnv.SaveConfig(cfg); err != nil {
			a.status = "couldn't save default model: " + err.Error()
			a.panel = nil
			return a, nil
		}
	}

	sess := a.panel.model.sess
	if sess == nil {
		// The overview: no running session to swap, only the default.
		a.status = "Default model set to " + modelmeta.DisplayName(selected) + "."
		a.panel = nil
		return a, nil
	}

	if modelProvider(sess.Model) != modelProvider(selected) {
		a.status = "Live model swap needs the same provider — default set for new sessions; this session keeps its model."
		a.panel = nil
		return a, nil
	}

	a.status = "Model set to " + modelmeta.DisplayName(selected) + "."
	a.panel = nil
	sessionID, sup := sess.ID, a.sup
	return a, func() tea.Msg {
		// A defensive backstop, not the primary guard: the client-side
		// provider check above already keeps this call same-provider on the
		// common path. [Supervisor.SetModel]'s own cross-provider rejection
		// (its doc: the concrete error type does not cross the daemon wire)
		// still surfaces cleanly here — opDoneMsg's existing error handling
		// (App.Update) turns any error into the same transient status note
		// rather than a crash.
		err := sup.SetModel(context.Background(), sessionID, selected)
		return opDoneMsg{err: err}
	}
}

// modelProvider resolves id's provider family, or "" for an id whose backend
// cannot be determined at all — including "" itself, the state a session
// created with no explicit model override carries ([SessionInfo.Model]).
// [App.handleModelSelect] treats two unresolvable providers as a mismatch
// rather than guessing, so an unknown current model never triggers a live swap
// it can't reason about.
//
// This is a DECISION path, not a display one, so it uses Resolve rather than
// Lookup: it only ever reads .Provider, never metadata. Lookup here would
// return "" for an unregistered-but-runnable id (a model newer than this
// binary's registry), which handleModelSelect reads as a provider mismatch —
// so swapping between two models of the SAME provider would silently decline
// the live swap and only set the new-session default. Resolve infers the
// provider from the id's shape, keeping the swap available — which is exactly
// the path the picker's free-text entry takes, since a typed id need not be
// registered at all (see modelpicker.go). The picker's own metadata rendering
// branches on [provider.ModelInfo.Unregistered] instead, so an inferred record
// never presents its zero-value pricing or context window as fact.
func modelProvider(id string) string {
	info, err := provider.Resolve(id)
	if err != nil {
		return ""
	}
	return info.Provider
}
