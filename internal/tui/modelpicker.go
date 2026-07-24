package tui

// modelpicker.go implements the /model command-panel tab (M4 step 4): a
// picker over [modelcatalog], grouped by provider and listing, per
// authenticated provider, the family that provider's CREDENTIAL KIND can
// actually reach — read from [CommandEnv.Auth], the same auth seam status.go
// already reads, never a new credential path. Each row marks (✓) the active
// model and shows a one-line description (context window + pricing) derived
// from provider.Lookup through a small gofer-side display-name table the SDK
// doesn't carry. With zero providers authenticated the list is empty and a
// warning line tells the user how to sign in — the picker still opens
// (auth-independence, docs/projects/gofer-m4-command-views-plan.md §5).
//
// The catalog is compiled in, so it is always at most as new as this binary:
// a model released after the build exists for the provider but not for the
// list. Since SDK v0.12.0 the registry is no longer an admission gate
// ([provider.Resolve] runs an unregistered id by inferring its backend from
// the id's shape), so the picker carries a free-text entry line as the escape
// hatch: type any model id and Enter commits it, registered or not, with no
// network call and no cache. Typed ids are NOT added to the list — the list
// stays exactly "what this binary knows about", and the entry line is "what
// you can also ask for".
//
// An unregistered id has NO trustworthy metadata (only ID and Provider — see
// [provider.ModelInfo.Unregistered]), so every metadata segment of a
// description line is rendered as "unknown" rather than as its zero value.
// Showing a synthesized "$0/$0 per Mtok · 0 context" as fact would be a
// fabricated price and a fabricated limit, which the SDK's own field docs
// explicitly forbid.
//
// Selecting a model (Enter) couples [Supervisor.SetModel] (a mid-session
// swap) with persisting the selected id as the session.model config default
// via env.SaveConfig — see [App.handleModelSelect] (panel.go), which
// intercepts Enter ahead of this pure value's own handleKey, since a value
// type has no IO seam to make the daemon/config calls itself.
// [modelPickerView.selectedModel] is the seam between them: the highlighted
// row's id at Enter time.
//
// Reasoning effort is NOT adjusted here. It landed as its own Thinking tab
// (effortpicker.go, /thinking) rather than as a ←/→ modifier on this one: ←/→
// are claimed by the panel host for tab switching (panel.go's
// commandPanel.handleKey), and effort is an orthogonal axis with its own
// capability rule — which that tab reads off the model THIS tab reports as
// active (see [activeModelFor], shared by both).
import (
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/modelcatalog"
	"github.com/jedwards1230/gofer/internal/modelmeta"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// modelRow is one flattened row in the picker's provider-grouped list.
type modelRow struct {
	provider string
	id       string
}

// modelPickerView renders the Model tab. Like statusView/configView it is a
// pure value: every method returns an updated copy, so a fixed key sequence
// replays to the same rendered output in every golden test.
type modelPickerView struct {
	theme theme.Theme
	env   CommandEnv
	sess  *SessionInfo // nil on the overview — no active session

	// defaultModel is the roster header's resolved credential-driven default
	// (Overview.DefaultModel, threaded in the same way status.go's
	// defaultModel is) — the active-mark's last-resort fallback, below the
	// attached session's own override and the persisted session.model
	// config default.
	defaultModel string

	// models is the picker's list, RESOLVED ONCE and cached for the panel's
	// lifetime — never per render. It is seeded at construction with the
	// compiled-in floor ([modelcatalog.CatalogForKind], pure and instant, so
	// the picker opens with a usable list on the very first frame) and
	// replaced wholesale by [modelPickerView.withCatalog] when the live
	// listing arrives from the background load.
	//
	// Caching is not an optimization here, it is a correctness constraint: a
	// live listing costs a bounded-but-real vendor round trip, and rows() is
	// read several times per keystroke (lines, View, Height, selectedModel).
	// Resolving there would either freeze the TUI or issue a request per
	// keypress.
	models []modelcatalog.Model

	// live records that models came from a completed load rather than from the
	// seeded floor. Nothing renders differently on it today; it exists so the
	// panel host can tell "not loaded yet" from "loaded, and this is what
	// there is" without comparing slices.
	live bool

	cursor int // highlighted row index into rows(); -1 = none highlighted

	// entry is the free-text model-id buffer: the escape hatch for a model
	// this binary's compiled-in catalog doesn't list. Typing any character
	// appends here AND drops the row highlight (cursor = -1), so the typed id
	// — not a stale highlight from an earlier ↓ — is what Enter commits (see
	// [modelPickerView.selectedModel]). Backspace edits it; Esc clears it
	// ([modelPickerView.handleEscape]). It is a plain string rather than an
	// [inputBuffer] because ←/→ are claimed by the panel host for tab
	// switching (panel.go), leaving a cursor-aware buffer's mid-text
	// positioning unreachable — the same reason configView.editBuf is a
	// plain string.
	entry string
}

// newModelPickerView returns a Model tab reading through env, with sess and
// defaultModel captured at open time the same way status.go's statusView
// captures them (command.go's openPanel).
//
// The model list is seeded here from the OFFLINE floor so the panel renders
// immediately. The live listing is fetched off the Update loop by the panel
// host ([App.discoverModelsCmd]) and applied later via
// [modelPickerView.withCatalog]; see the models field for why it must not be
// resolved on the render path.
func newModelPickerView(th theme.Theme, env CommandEnv, sess *SessionInfo, defaultModel string) modelPickerView {
	return modelPickerView{
		theme:        th,
		env:          env,
		sess:         sess,
		defaultModel: defaultModel,
		models:       floorCatalog(env),
		cursor:       -1,
	}
}

// floorCatalog builds the offline model list: each authenticated provider's
// compiled-in family for that credential's KIND, provider blocks ascending and
// each block in its catalog's own display order. Pure and instant — no network,
// no store read beyond env.Auth.
func floorCatalog(env CommandEnv) []modelcatalog.Model {
	var out []modelcatalog.Model
	for _, a := range authedProviders(env) {
		out = append(out, modelcatalog.CatalogForKind(a.Provider, modelcatalog.Kind(a.Kind))...)
	}
	return out
}

// withCatalog returns a copy of the view listing models, replacing whatever it
// was seeded or last loaded with.
//
// An EMPTY result is ignored on purpose. [modelcatalog.Catalog] is contracted
// never to return an empty list on a discovery failure — it falls back to the
// floor internally — so an empty slice here means the caller had nothing to
// offer at all (a nil env.Models closure, or an auth.json read error). Adopting
// it would replace a working list with a blank picker, which is precisely the
// outcome the floor exists to prevent.
//
// The row highlight is carried across by MODEL ID, not by index. The index is
// meaningless across the swap — a live listing can be shorter, longer, or
// differently ordered than the floor, so keeping the number would silently
// move the highlight onto a different model than the one the user was looking
// at when they pressed ↓. Dropping it outright is not the answer either: a load
// landing a second after the panel opened would yank the selection out from
// under a user mid-keypress, and a background upgrade must not undo the user's
// own navigation. Re-finding the same id gives the only reading that is true
// under both lists — the user was pointing at THAT model, and still is. A
// highlighted model absent from the live listing has genuinely gone away, so
// the highlight drops to none rather than sliding to an unrelated neighbor.
//
// A typed entry is untouched — it is text the user wrote, not a position in
// this data.
func (v modelPickerView) withCatalog(models []modelcatalog.Model) modelPickerView {
	if len(models) == 0 {
		return v
	}
	// Read the highlighted id BEFORE the list is replaced, since cursor
	// indexes the OLD rows.
	highlighted := ""
	if rows := v.rows(); v.cursor >= 0 && v.cursor < len(rows) {
		highlighted = rows[v.cursor].id
	}
	v.models = models
	v.live = true
	v.cursor = indexOfModel(models, highlighted)
	return v
}

// indexOfModel returns models' index for id, or -1 when id is empty or the
// list no longer carries it. The result indexes rows() too — rows() is a
// 1:1 flattening of models, in order.
func indexOfModel(models []modelcatalog.Model, id string) int {
	if id == "" {
		return -1
	}
	for i, m := range models {
		if m.ID == id {
			return i
		}
	}
	return -1
}

// View renders the view's rows, width-truncated and capped to height — the
// same Renderable contract every other panel/screen component follows
// (testkit.Renderable).
func (v modelPickerView) View(width, height int) string {
	lines := v.lines()
	if height >= 0 && len(lines) > height {
		lines = lines[:height]
	}
	for i, l := range lines {
		lines[i] = truncate(l, width)
	}
	return strings.Join(lines, "\n")
}

// lines builds the free-text entry line, the typed id's candidate line (only
// while something is typed), and the provider-grouped row list — or the
// empty-list warning in place of the rows when no provider is authenticated
// (§4c). The entry line renders in the zero-auth state too: committing a
// model id only writes the session.model config default, which is possible
// with no provider authenticated at all (auth-independence, §5).
func (v modelPickerView) lines() []string {
	out := []string{v.entryLine()}
	if line, ok := v.candidateLine(); ok {
		out = append(out, line)
	}
	rows := v.rows()
	if len(rows) == 0 {
		return append(out, v.theme.WarnStyle().Render("No providers authenticated. Run /login (or 'gofer login <anthropic|openai>')."))
	}
	active := v.activeModel()
	lastProvider := ""
	for i, r := range rows {
		if r.provider != lastProvider {
			out = append(out, r.provider+":")
			lastProvider = r.provider
		}
		out = append(out, v.rowLine(i, r, r.id == active))
	}
	return out
}

// entryLine renders the free-text model-id box: a muted prompt when empty,
// else the typed text with the same "▏" edit caret configView's in-progress
// string edit uses.
func (v modelPickerView) entryLine() string {
	if v.entry == "" {
		return v.theme.MutedStyle().Render("Model id: (type any id, listed or not)")
	}
	return "Model id: " + v.entry + "▏"
}

// candidateLine renders what Enter would commit for the typed entry, or the
// reason it can't. It reports ok=false when nothing is typed, so the line
// only ever costs a row while the entry is in use.
//
// A typed id whose backend [provider.Resolve] cannot infer is not committable
// at all (there is no adapter to route it to), so the SDK's own error message
// is surfaced verbatim rather than restating the recognized id families here —
// gofer would only drift out of sync with the SDK's list. This mirrors
// [modelPickerView.selectedModel], which returns "" for exactly this case, so
// the "why is Enter doing nothing" answer is on screen before Enter is
// pressed.
func (v modelPickerView) candidateLine() (string, bool) {
	id := strings.TrimSpace(v.entry)
	if id == "" {
		return "", false
	}
	if _, err := provider.Resolve(id); err != nil {
		return v.theme.WarnStyle().Render("  " + err.Error()), true
	}
	// The marker matches a highlighted row's: with nothing highlighted
	// (cursor < 0) the typed id IS what Enter commits, so it carries the
	// same "this one" affordance the row list uses. Once ↓ moves onto a real
	// row that row wins the commit, so the candidate drops back to unmarked.
	if v.cursor < 0 {
		return v.theme.AccentStyle().Render("▸ " + modelDescriptionLine(id)), true
	}
	return "  " + modelDescriptionLine(id), true
}

// rowLine renders one model's marker, active-mark, and description,
// accent-styling the highlighted row (i == cursor).
func (v modelPickerView) rowLine(i int, r modelRow, isActive bool) string {
	marker := "  "
	if i == v.cursor {
		marker = "▸ "
	}
	check := "  "
	if isActive {
		check = "✓ "
	}
	line := marker + check + v.rowDescription(r.id)
	if i == v.cursor {
		return v.theme.AccentStyle().Render(line)
	}
	return line
}

// rowDescription renders a listed row's description line, preferring the
// catalog entry's own metadata over anything compiled in. For a discovered
// model that means the VENDOR's current display name and context window rather
// than gofer's table and the SDK registry, which are both only ever as fresh as
// this binary — the point of fetching a live listing is to believe it.
//
// Pricing is deliberately not part of that preference, because there is nothing
// to prefer: [modelcatalog.Model] carries no pricing field at all, since the
// listing carries none (a subscription model has no per-token price to quote).
// It therefore keeps coming from the registry via [pricingSegment], which
// renders "pricing unknown" for exactly the unregistered ids discovery returns.
// A discovered model must never show $0 — that would be a fabricated price, not
// a free one.
func (v modelPickerView) rowDescription(id string) string {
	for _, m := range v.models {
		if m.ID == id {
			return catalogDescriptionLine(m)
		}
	}
	return modelDescriptionLine(id)
}

// catalogDescriptionLine renders one catalog entry's display line, using its
// Label and ContextWindow where it has them and falling back to the compiled-in
// answer where it does not. An id whose backend cannot be inferred at all has no
// pricing story to tell, so it renders name + whatever context the catalog knew.
func catalogDescriptionLine(m modelcatalog.Model) string {
	name := m.Label
	if name == "" {
		name = modelmeta.DisplayName(m.ID)
	}
	head := fmt.Sprintf("%s (%s)", name, m.ID)

	info, err := provider.Resolve(m.ID)
	if err != nil {
		if m.ContextWindow > 0 {
			return fmt.Sprintf("%s · %s context · pricing unknown", head, formatContextWindow(m.ContextWindow))
		}
		return head
	}
	// A live context window outranks the registry's: it is what the backend
	// currently serves. Zero means the catalog didn't know, NOT "no context"
	// (see modelcatalog.codexFloor), so it defers rather than reporting 0.
	ctxSeg := contextSegment(info)
	if m.ContextWindow > 0 {
		ctxSeg = formatContextWindow(m.ContextWindow) + " context"
	}
	return fmt.Sprintf("%s · %s · %s", head, ctxSeg, pricingSegment(info))
}

// modelDescriptionLine renders one model's display line: its short name (or
// raw id, see [modelmeta.DisplayName]) plus context window and per-Mtok
// pricing, e.g. "Sonnet 5 (claude-sonnet-5) · 1M context · $3/$15 per Mtok".
//
// Every metadata segment is rendered from a KNOWN value or as "unknown" —
// never as a zero value dressed up as fact. This matters because the entry
// line admits ids the compiled-in registry does not carry: for those,
// [provider.Lookup] misses entirely, and even [provider.Resolve]'s synthesized
// record carries zeroes that mean "unknown", not "free" and not "no context"
// (see [provider.ModelInfo.Unregistered], which states that only ID and
// Provider are trustworthy on such a record). Rendering "$0/$0 per Mtok · 0
// context" for a model that in reality costs money and has a large context
// window would be a fabricated price and a fabricated limit presented as
// fact, so both are suppressed. An id whose backend cannot be inferred at all
// resolves to nothing and renders as the bare display name.
func modelDescriptionLine(id string) string {
	name := modelmeta.DisplayName(id)
	head := fmt.Sprintf("%s (%s)", name, id)
	info, err := provider.Resolve(id)
	if err != nil {
		return head
	}
	return fmt.Sprintf("%s · %s · %s", head, contextSegment(info), pricingSegment(info))
}

// contextSegment renders info's context window, or "context unknown" when
// there is no trustworthy value: an Unregistered record (whose every metadata
// field is a placeholder) or a registered record whose ContextWindow is 0 —
// which the SDK documents as "unknown", explicitly NOT "no context" — so an
// incomplete registry row can't fabricate a limit either.
func contextSegment(info provider.ModelInfo) string {
	if info.Unregistered || info.ContextWindow == 0 {
		return "context unknown"
	}
	return formatContextWindow(info.ContextWindow) + " context"
}

// pricingSegment renders info's per-Mtok input/output rates, or "pricing
// unknown" when either is untrustworthy: an Unregistered record (whose
// Pricing is the zero value, which the SDK documents is NOT a price of zero)
// or a registered record missing either rate. A genuinely free model would
// also report unknown here — a zero rate is indistinguishable from an unset
// one in this struct, and claiming "free" wrongly is the worse error of the
// two.
func pricingSegment(info provider.ModelInfo) string {
	if info.Unregistered || info.Pricing.Input == 0 || info.Pricing.Output == 0 {
		return "pricing unknown"
	}
	return fmt.Sprintf("$%s/$%s per Mtok", formatPrice(info.Pricing.Input), formatPrice(info.Pricing.Output))
}

// formatContextWindow renders a token count as a compact "1M"/"400K" style
// magnitude.
func formatContextWindow(n int) string {
	switch {
	case n >= 1_000_000 && n%1_000_000 == 0:
		return fmt.Sprintf("%dM", n/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%dK", n/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// formatPrice renders a per-Mtok USD rate, dropping a trailing ".00" (e.g.
// 3 -> "3", 1.25 -> "1.25").
func formatPrice(usd float64) string {
	s := fmt.Sprintf("%.2f", usd)
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}

// rows returns the flattened list of models the authenticated providers'
// CREDENTIALS can actually reach, provider blocks in ascending provider order
// and each block in its catalog's own display order (§4a). Zero providers
// authenticated yields an empty list, never an error.
//
// It is a pure read of the cached [modelPickerView.models] — no store read, no
// network, nothing that can block a keystroke. That cache is the whole shape of
// this view's data flow; see the field's doc.
//
// The list is per-credential-KIND, not per-provider (issue #156): OpenAI routes
// an OAuth (subscription) credential to a different backend than an API key,
// serving a different model family, and the SDK registry only carries the
// API-key one. Listing the registry to an OAuth user offered ids that backend
// rejects outright while hiding every id it does serve, so the models the user
// could actually run were reachable only by typing them. [modelcatalog] owns
// that mapping; this view only asks it, per authenticated credential.
//
// [modelcatalog.CatalogForKind] is the pure, IO-free entry point, which is what
// this render path needs — rows() runs several times per keystroke, and the
// kind is already in hand from env.Auth. Its root-reading sibling
// modelcatalog.Catalog is the one that would gain live vendor discovery; if
// that lands, the upgrade is to thread it in through a CommandEnv closure
// (cmd/gofer owning root and ctx, as it does for Auth/Config) rather than to
// re-derive any of this here.
func (v modelPickerView) rows() []modelRow {
	rows := make([]modelRow, 0, len(v.models))
	for _, m := range v.models {
		rows = append(rows, modelRow{provider: m.Provider, id: m.ID})
	}
	return rows
}

// authedProviders reads env.Auth and returns one entry per authenticated
// provider, ascending by provider name so the row list's grouping never
// depends on Auth's return order. It mirrors statusView.authList's non-fatal
// contract: a nil closure (the zero CommandEnv) or a read error is treated
// identically to "no providers authenticated" — never a reason to block the
// view. This is a lock-free local read (see CommandEnv.Auth's doc) — it
// never resolves a credential or hits a provider network call.
//
// A provider reported twice keeps its first entry, so a malformed auth.json
// can't double every row of that provider's block.
//
// It is a package function rather than a method because both the view's own
// seeding ([floorCatalog]) and the panel host's background load
// ([App.discoverModelsCmd], which runs off the Update loop with no view in
// hand) need the same answer from the same env.
func authedProviders(env CommandEnv) []ProviderAuth {
	if env.Auth == nil {
		return nil
	}
	auths, err := env.Auth()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	out := make([]ProviderAuth, 0, len(auths))
	for _, a := range auths {
		if seen[a.Provider] {
			continue
		}
		seen[a.Provider] = true
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out
}

// activeModel resolves the model the ✓ mark applies to — see [activeModelFor],
// which owns the precedence rule.
func (v modelPickerView) activeModel() string {
	return activeModelFor(v.env, v.sess, v.defaultModel)
}

// activeModelFor resolves WHICH MODEL a command-panel tab is talking about:
// the attached/peeked session's own override, else the persisted session.model
// config default (env.Config, read the same lazy non-fatal way statusView's
// settingSourcesLine reads it), else the roster header's resolved default
// (defaultModel — the overview's resolveOverviewModel result, threaded in at
// open time).
//
// It is a package function rather than a method because two tabs need the same
// answer from the same three inputs: the Model tab's ✓ mark, and the Thinking
// tab's reasoning-capability gate (effortpicker.go), which must judge the model
// the runner will actually use. A second copy of this precedence would let the
// two disagree about which model is active — and the Thinking tab disagreeing
// is precisely how it would come to offer levels [runner.Runner.SetEffort]
// rejects.
func activeModelFor(env CommandEnv, sess *SessionInfo, defaultModel string) string {
	if sess != nil && sess.Model != "" {
		return sess.Model
	}
	if env.Config != nil {
		if cfg, err := env.Config(); err == nil && cfg.Session.Model != "" {
			return cfg.Session.Model
		}
	}
	return defaultModel
}

// handleKey applies one key press: ↓/↑ move the row highlight; Backspace
// edits the free-text entry; any other text key types into it (dropping the
// row highlight, so what is on screen as the entry is what Enter commits).
// Enter is a no-op HERE — this is a pure value with no IO seam. The real
// coupled select (SetModel + config.Save) lives one level up in
// [App.handleModelSelect] (panel.go), which intercepts Enter ahead of this
// method whenever the Model tab is active (see App.handlePanelKey);
// [modelPickerView.selectedModel] tells it what was selected. Esc never
// reaches here either — the panel host routes it to
// [modelPickerView.handleEscape] (see [commandPanel.handleEscape]).
func (v modelPickerView) handleKey(msg tea.KeyPressMsg) modelPickerView {
	key := msg.Key()
	switch key.Code {
	case tea.KeyDown:
		return v.selectDown()
	case tea.KeyUp:
		return v.selectUp()
	case tea.KeyEnter:
		return v
	case tea.KeyBackspace:
		return v.backspaceEntry()
	}
	if key.Text != "" {
		return v.typeEntry(key.Text)
	}
	return v
}

// typeEntry appends text to the free-text model-id buffer and drops the row
// highlight, for the same reason [configView.typeFilter] does: once the user
// is typing, an earlier highlight is no longer what they mean, and
// [modelPickerView.selectedModel]'s row-first precedence would otherwise
// commit a row the user has visibly moved away from.
func (v modelPickerView) typeEntry(text string) modelPickerView {
	v.entry += text
	v.cursor = -1
	return v
}

// backspaceEntry removes the last rune from the entry buffer, if any, and
// drops the row highlight for the same reason [modelPickerView.typeEntry]
// does.
func (v modelPickerView) backspaceEntry() modelPickerView {
	if v.entry == "" {
		return v
	}
	r := []rune(v.entry)
	v.entry = string(r[:len(r)-1])
	v.cursor = -1
	return v
}

// handleEscape applies one Esc press, reporting whether it was consumed here
// (true, the panel stays open) or should bubble to the panel host to close it
// (false) — the same two-stage contract [configView.handleEscape] has, so a
// half-typed model id is discarded by the first Esc rather than taking the
// whole panel down with it.
func (v modelPickerView) handleEscape() (modelPickerView, bool) {
	if v.entry != "" {
		v.entry = ""
		v.cursor = -1
		return v, true
	}
	return v, false
}

// selectedModel returns what Enter commits: the highlighted row's id
// (v.cursor), else the free-text entry. This is the value
// [App.handleModelSelect] reads on Enter.
//
// The typed id is admitted whether or not the registry carries it —
// [provider.Resolve], not registry membership, is what decides whether a model
// can run (SDK v0.12.0), and this picker's catalog is only ever as new as the
// binary. The one id it refuses is one Resolve cannot route to any backend at
// all: there is no adapter for it, so committing it would persist a
// session.model default that fails at the next run rather than here. That case
// returns "" — App.handleModelSelect's existing no-op — with
// [modelPickerView.candidateLine] already showing why on screen.
//
// It returns "" when nothing is selected at all: no row highlighted (cursor <
// 0 — the initial state before any ↓/↑) or an empty row list (zero providers
// authenticated, §4c), with an empty or whitespace-only entry. An empty entry
// must never commit.
func (v modelPickerView) selectedModel() string {
	rows := v.rows()
	if v.cursor >= 0 && v.cursor < len(rows) {
		return rows[v.cursor].id
	}
	id := strings.TrimSpace(v.entry)
	if id == "" {
		return ""
	}
	if _, err := provider.Resolve(id); err != nil {
		return ""
	}
	return id
}

// selectDown moves the highlight onto the first row (from no highlight) or
// one row further down, clamped at the last row.
func (v modelPickerView) selectDown() modelPickerView {
	rows := v.rows()
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

// selectUp moves the highlight up one row, clamped at the first row (no
// "back to the tab bar" state here — the Model tab has no filter box for a
// dropped highlight to mean anything, unlike configView.selectUp).
func (v modelPickerView) selectUp() modelPickerView {
	if v.cursor > 0 {
		v.cursor--
	}
	return v
}
