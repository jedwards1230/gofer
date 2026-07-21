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
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/modelcatalog"
	"github.com/jedwards1230/gofer/internal/modelmeta"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// commandPanelTab identifies one tab in the command panel's tab bar.
type commandPanelTab int

const (
	panelStatus commandPanelTab = iota
	panelConfig
	panelModel
	panelUsage
	panelStats
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
	{panelUsage, "Usage"},
	{panelStats, "Stats"},
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

	// now and roster are the Stats tab's extra inputs (see stats.go), captured
	// once at open time the same way sess is: now is the overview's reference
	// time ([Overview.Now]) so the Stats tab's age/last-active elapsed output
	// stays deterministic in goldens, and roster is the full session snapshot
	// ([Overview.Roster]) the Stats rollup sums tokens + cost across.
	now    time.Time
	roster []SessionInfo

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
// time for the Status/Usage tabs to read, now and roster for the Stats tab,
// and the Config/Model tabs' working state loaded from env at the same time.
func newCommandPanel(th theme.Theme, tab commandPanelTab, env CommandEnv, sess *SessionInfo, defaultModel string, now time.Time, roster []SessionInfo) commandPanel {
	return commandPanel{
		theme:        th,
		active:       tab,
		env:          env,
		sess:         sess,
		defaultModel: defaultModel,
		now:          now,
		roster:       roster,
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
	case panelUsage:
		v := usageView{theme: p.theme, sess: p.sess}
		return v.View(width, bodyRows)
	case panelStats:
		v := statsView{theme: p.theme, sess: p.sess, now: p.now, roster: p.roster}
		return v.View(width, bodyRows)
	case panelConfig:
		return p.cfg.View(width, bodyRows)
	case panelModel:
		return p.model.View(width, bodyRows)
	}
	return ""
}

// modelsLoadedMsg carries the result of the Model tab's background catalog
// load ([App.discoverModelsCmd]). It has no error field on purpose: every
// failure mode already degrades to a usable list inside
// [modelcatalog.Catalog], and the panel's response to "nothing came back" is
// to keep the floor it opened with, which is what an empty models does.
type modelsLoadedMsg struct{ models []modelcatalog.Model }

// discoverModelsCmd fetches the live model listing for every authenticated
// provider, OFF the Update loop.
//
// This is the deliberate answer to "opening /model must not freeze the TUI".
// Live discovery is bounded (modelcatalog.DefaultDiscoveryTimeout, 3s) but 3s
// is an eternity in a key handler, and resolving before the first render would
// stall the panel for exactly as long as the vendor is slow. So the picker
// opens instantly on the compiled-in floor — a complete, usable, correct-for-
// this-credential list — and this command upgrades it in place when the
// listing lands. The alternatives were both worse: blocking trades a
// guaranteed freeze for a marginally fresher first frame, and rendering an
// empty list with a spinner shows nothing where a perfectly good answer was
// already available offline.
//
// It returns nil when there is nothing to fetch (no Models closure, or no
// authenticated provider), so a caller can dispatch it unconditionally.
// Failures are silent by design: the user asked to pick a model, not to hear
// about the vendor's uptime, and the floor beneath them is already right.
func (a App) discoverModelsCmd() tea.Cmd {
	env := a.commandEnv
	if env.Models == nil {
		return nil
	}
	authed := authedProviders(env)
	if len(authed) == 0 {
		return nil
	}
	return func() tea.Msg {
		var out []modelcatalog.Model
		for _, p := range authed {
			models, err := env.Models(context.Background(), p.Provider)
			if err != nil {
				// A broken auth.json for one provider must not cost the others
				// their rows; Catalog has already absorbed every discovery
				// failure by this point.
				continue
			}
			out = append(out, models...)
		}
		return modelsLoadedMsg{models: out}
	}
}

// applyModelsLoaded folds a completed catalog load into the open panel.
//
// A panel CLOSED while the fetch was in flight drops the result: the next open
// re-fetches, and a stale slice must never resurrect a dismissed panel. That is
// the only drop condition. A panel still open but sitting on another tab keeps
// the result — the load was dispatched for this panel, the Model tab is one
// ←/→ away, and discarding it would mean tabbing back re-fetches something
// already in hand (handlePanelKey's re-fetch is guarded on !live for exactly
// this reason).
//
// The result is applied through [modelPickerView.withCatalog], which is what
// makes a LATE arrival safe: an empty result (every provider failed) leaves the
// floor standing, and a populated one carries the user's row highlight across
// by model id and leaves a typed entry alone. So a load landing after the user
// has already typed or navigated upgrades the list underneath them without
// moving what they had selected.
func (a App) applyModelsLoaded(msg modelsLoadedMsg) App {
	if a.panel == nil {
		return a
	}
	p := *a.panel
	p.model = p.model.withCatalog(msg.models)
	a.panel = &p
	return a
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
	was := a.panel.active
	p := a.panel.handleKey(msg)
	a.panel = &p
	// Tabbing INTO the Model tab is the other way a user reaches the picker
	// (the panel opens on whichever tab the slash command named, but ←/→ reach
	// all three). Fetch on that transition too, once — a list already upgraded
	// by a completed load is not re-fetched on every tab bounce.
	if p.active == panelModel && was != panelModel && !p.model.live {
		return a, a.discoverModelsCmd()
	}
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
	return a.applyModelSelection(selected, a.panel.model.sess)
}

// applyModelSelection is the commit half of [App.handleModelSelect], split
// out so `/model <id>` (command.go) reaches the SAME config write, the same
// header refresh, the same daemon probe, and the same status notes as picking
// a row in the panel — rather than a parallel implementation that drifts. The
// two callers differ only in where selected and sess come from: the picker
// reads them off the open panel, the string form takes the typed id and
// [App.currentSessionInfo] (which is the same value openPanel hands the panel
// as sess, so the two paths see identical session state).
//
// selected is assumed non-empty and already admitted by [provider.Resolve] —
// both callers gate on that before calling in, and they differ in what an
// unroutable id does (the picker no-ops, `/model <id>` reports it), so the
// decision stays with them.
func (a App) applyModelSelection(selected string, sess *SessionInfo) (App, tea.Cmd) {
	var cfg config.Config
	if a.commandEnv.Config != nil {
		c, err := a.commandEnv.Config()
		if err != nil {
			// A read failure must NOT fall through to SaveConfig with a
			// zero-value config — that would overwrite config.json and drop
			// the user's permissions/telemetry settings. Surface it and abort,
			// preserving the on-disk state (mirrors the SaveConfig-error path).
			a.setStatus(sevDanger, "couldn't load config: "+err.Error())
			a.panel = nil
			return a, nil
		}
		cfg = c
	}
	cfg.Session.Model = selected
	if a.commandEnv.SaveConfig != nil {
		if err := a.commandEnv.SaveConfig(cfg); err != nil {
			a.setStatus(sevDanger, "couldn't save default model: "+err.Error())
			a.panel = nil
			return a, nil
		}
	}

	// Make the write visible where the user reads the default: the roster
	// header (and the attach screen's, which renders the same meta). On the
	// LOCAL backend that is immediate and unconditional — this process owns the
	// default. On the daemon path the header shows the DAEMON's default, which
	// only the daemon can answer, so it is refreshed asynchronously from the
	// gofer/hello probe [App.probeDaemonDefaultCmd] dispatches below (issue
	// #162 — before that probe existed the header simply stayed stale forever).
	if !a.commandEnv.DaemonBacked {
		a.over = a.over.WithDefaultModel(selected)
	}

	a.panel = nil

	if sess == nil {
		// The overview: no running session to swap, only the default.
		a.setStatus(a.defaultReachSeverity(sevOK), a.withDefaultReach(
			"Default model set to "+modelmeta.DisplayName(selected)+".",
			"Default saved; attached daemon adopts it unless pinned."))
		return a, a.probeDaemonDefaultCmd(outcomeDefaultOnly, selected)
	}

	if modelProvider(sess.Model) != modelProvider(selected) {
		// "default set for new sessions" is only true where this process's
		// config write reaches the thing that creates them, so the
		// daemon-attached wording drops that clause outright rather than
		// qualifying it — a caveat can be truncated off an 80-column status
		// line, and an overclaim that survives truncation is exactly the bug.
		//
		// Warn either way: the default WAS written, but the thing the user was
		// most likely looking at — the running session — did not move.
		if a.commandEnv.DaemonBacked {
			a.setStatus(sevWarn, a.withDefaultReach(
				"Live model swap needs the same provider — this session keeps its model.",
				"Provider differs — session keeps its model; default saved."))
		} else {
			a.setStatus(sevWarn, "Live model swap needs the same provider — default set for new sessions; this session keeps its model.")
		}
		return a, a.probeDaemonDefaultCmd(outcomeProviderMismatch, selected)
	}

	// The live swap dispatched below applies on either backend — over the wire
	// on the daemon path, in-process on the local one — so this half of the
	// message is unconditional. Only the DEFAULT's reach differs.
	a.setStatus(a.defaultReachSeverity(sevOK), a.withDefaultReach(
		"Model set to "+modelmeta.DisplayName(selected)+".",
		"Model set for this session; daemon adopts the default unless pinned."))
	sessionID, sup := sess.ID, a.sup
	probe := a.probeDaemonDefaultCmd(outcomeLiveSwap, selected)
	return a, func() tea.Msg {
		// A defensive backstop, not the primary guard: the client-side
		// provider check above already keeps this call same-provider on the
		// common path. [Supervisor.SetModel]'s own cross-provider rejection
		// (its doc: the concrete error type does not cross the daemon wire)
		// still surfaces cleanly here — opDoneMsg's existing error handling
		// (App.Update) turns any error into the same transient status note
		// rather than a crash.
		err := sup.SetModel(context.Background(), sessionID, selected)
		if err != nil || probe == nil {
			// A failed swap is the whole story; the probe would only talk over
			// it. opDoneMsg stays the ONLY route to danger for an op result.
			return opDoneMsg{err: err}
		}
		// Sequenced INSIDE this command rather than tea.Batch'd alongside it on
		// purpose: both are RPCs to the same daemon, the probe is only
		// meaningful once the swap has settled, and a Batch would fan out two
		// messages where the second must win.
		return probe()
	}
}

// modelSelectOutcome records what a committed /model select did to the SESSION,
// so [App.applyDaemonDefault] can pick a status note that stays true about both
// halves of the action once the daemon's answer about the DEFAULT arrives.
type modelSelectOutcome int

const (
	// outcomeDefaultOnly is a select made from the overview: no session was
	// attached or peeked, so only the default was written.
	outcomeDefaultOnly modelSelectOutcome = iota
	// outcomeProviderMismatch is a select whose provider differs from the
	// attached session's, so the running session kept its model.
	outcomeProviderMismatch
	// outcomeLiveSwap is a same-provider select that also hot-swapped the
	// attached session's live model.
	outcomeLiveSwap
)

// daemonDefaultProbedMsg carries the attached daemon's CURRENT default model,
// read back off gofer/hello after a /model write ([App.probeDaemonDefaultCmd]).
// daemonDefault is "" whenever the answer is UNKNOWN — the probe failed, the
// daemon predates gofer/hello, or it resolved no default at all — which is a
// distinct state from "the daemon reports a different model", and the two must
// not be conflated: unknown keeps the hedged note, different means pinned.
type daemonDefaultProbedMsg struct {
	outcome       modelSelectOutcome
	selected      string
	daemonDefault string
}

// daemonProbeTimeout bounds the post-write gofer/hello probe. It is a package
// constant rather than a config knob because the probe is a single handshake
// RPC to an already-open connection — there is no deployment where a user would
// reasonably want to wait longer, and the cost of giving up is only that the
// note stays hedged (see [daemonDefaultProbedMsg]). Mirrors
// [modelcatalog.DefaultDiscoveryTimeout]'s shape for the same reason.
const daemonProbeTimeout = 3 * time.Second

// probeDaemonDefaultCmd asks the attached daemon what its default model
// CURRENTLY is, so the header and the status note can both stop guessing
// (issue #162). It returns nil — no probe, nothing to refine — on the local
// backend, or when the process wiring supplied no probe
// ([CommandEnv.DaemonDefaultModel] is nil, which is also how a daemon
// predating gofer/hello is reported; see cmd/gofer's selectTUIBackend).
//
// It is a tea.Cmd, NOT part of the synchronous select handling, because it is a
// network call: running it inline would block the Update loop for as long as
// the daemon takes to answer.
func (a App) probeDaemonDefaultCmd(outcome modelSelectOutcome, selected string) tea.Cmd {
	if !a.commandEnv.DaemonBacked || a.commandEnv.DaemonDefaultModel == nil {
		return nil
	}
	probe := a.commandEnv.DaemonDefaultModel
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), daemonProbeTimeout)
		defer cancel()
		model, err := probe(ctx)
		if err != nil {
			// Every failure collapses to "unknown", which leaves the hedged
			// note handleModelSelect already set standing. A header detail must
			// never turn into an error the user has to act on.
			model = ""
		}
		return daemonDefaultProbedMsg{outcome: outcome, selected: selected, daemonDefault: model}
	}
}

// applyDaemonDefault folds the probed daemon default back into the app: it
// refreshes the roster header from the DAEMON's own answer — the value every
// session that daemon creates without an explicit model will use — and replaces
// the hedged "adopts it unless pinned" note with a definitive one.
//
// The two cases the pre-probe wording could not tell apart (see
// [App.withDefaultReach]):
//
//   - ADOPTED — the daemon now reports exactly what was just written, so the
//     write reached it and the next session it creates uses it.
//   - PINNED — the daemon reports something else (it was started with an
//     explicit --model, which stays authoritative for its lifetime), so the
//     write reaches only future daemons. The header still moves, to the pinned
//     value: that is the truth about what the daemon runs, and showing the
//     never-to-be-used selected id there would be the same overclaim in a
//     second place.
//
// An unknown answer (daemonDefault == "") changes nothing at all.
//
// Every note here is STANDALONE rather than a base note plus a suffix, and none
// interpolates a model id: the status line is truncated to the terminal width
// (App.render), so a qualification that gets cut off leaves exactly the
// unqualified overclaim behind, and an arbitrarily long model id is precisely
// what pushes one over the edge. TestDaemonDefaultNotesFitTheWidthFloor pins
// this.
func (a App) applyDaemonDefault(msg daemonDefaultProbedMsg) App {
	if msg.daemonDefault == "" {
		return a
	}
	a.over = a.over.WithDefaultModel(msg.daemonDefault)

	adopted := msg.daemonDefault == msg.selected
	switch msg.outcome {
	case outcomeProviderMismatch:
		// Warn regardless: the running session did not move either way.
		if adopted {
			a.setStatus(sevWarn, "Provider differs — session keeps its model; daemon took the default.")
		} else {
			a.setStatus(sevWarn, "Provider differs — session keeps its model; daemon is pinned.")
		}
	case outcomeLiveSwap:
		if adopted {
			a.setStatus(sevOK, "Model set for this session; the daemon took the new default.")
		} else {
			a.setStatus(sevWarn, "Model set for this session; the daemon is pinned to another default.")
		}
	default: // outcomeDefaultOnly
		if adopted {
			a.setStatus(sevOK, "Default model saved; the attached daemon adopted it.")
		} else {
			a.setStatus(sevWarn, "Default saved; the attached daemon is pinned to another model.")
		}
	}
	return a
}

// defaultReachSeverity downgrades an otherwise-unqualified success to a caveat
// on the daemon path, where [App.withDefaultReach]'s wording is hedged by
// construction ("adopts it unless pinned" is a statement the TUI cannot yet
// back up). Once the probe lands, [App.applyDaemonDefault] replaces both the
// note and this severity with a definitive one.
func (a App) defaultReachSeverity(local statusSeverity) statusSeverity {
	if a.commandEnv.DaemonBacked {
		return sevWarn
	}
	return local
}

// withDefaultReach appends to note whatever qualification the persisted
// session.model default needs to stop overclaiming on THIS backend. It is the
// single decision point for "did writing the default actually change what runs
// next", and the only place that answer is phrased.
//
// Local backend: the write took effect. [Adapter.Create] resolves the default
// per create (see internal/tuibridge), so the very next session started from
// this TUI uses it — nothing to qualify, and note is returned unchanged.
//
// Daemon-attached: it did NOT take effect for the daemon on the other end. That
// daemon now RE-READS its default per session/new (issue #156's daemon half),
// so an unpinned daemon does adopt this write on its next session, with no
// restart. A daemon started with an explicit --model is pinned: that flag stays
// authoritative for its lifetime and the write reaches only future daemons.
//
// SYNCHRONOUSLY, this TUI cannot tell those two apart, so the daemon-attached
// wording is deliberately chosen to be TRUE UNDER BOTH: "adopts it unless
// pinned" claims no effect that might not have occurred, and concedes none
// that did. Asserting "unchanged until restart" (the pre-fix wording) is now
// simply false for the unpinned case, which is the common one.
//
// It is no longer the LAST word, though. This function stays pure and
// synchronous — it is called on the Update loop — and the pinned/unpinned
// question is settled asynchronously by [App.probeDaemonDefaultCmd], whose
// gofer/hello answer [App.applyDaemonDefault] turns into a definitive note
// (issue #162). What this function returns is therefore the note the user reads
// for the few milliseconds before that lands, and the note that STAYS if the
// probe cannot answer.
//
// Each branch returns a STANDALONE string rather than composing note+suffix.
// The status line is truncated to the terminal width (App.render), and a
// qualification that gets cut off leaves exactly the unqualified overclaim
// behind. The base notes run to 71 columns, leaving 9 for a caveat at the
// 80-column floor the golden tests pin — no meaningful caveat fits as a
// suffix, which is why composition was abandoned. The daemon strings name no
// model for the same reason: it is already on screen in the header directly
// above, and interpolating an arbitrarily long id would put them back over
// the edge.
func (a App) withDefaultReach(local, daemonAttached string) string {
	if !a.commandEnv.DaemonBacked {
		return local
	}
	return daemonAttached
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
