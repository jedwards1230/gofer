package tui

// modelpicker.go implements the /model command-panel tab (M4 step 4): a
// read-only picker over the SDK's static model catalog (provider.Models() /
// provider.Lookup), grouped by provider and filtered to the providers
// [CommandEnv.Auth] reports authenticated — the same auth seam status.go
// already reads, never a new credential path. Each row marks (✓) the active
// model and shows a one-line description (context window + pricing) derived
// from provider.Lookup through a small gofer-side display-name table the SDK
// doesn't carry. With zero providers authenticated the list is empty and a
// warning line tells the user how to sign in — the picker still opens
// (auth-independence, docs/projects/gofer-m4-command-views-plan.md §5).
//
// Selecting a model (Enter) couples Supervisor.SetModel (a mid-session swap)
// with persisting the session.model default via env.SaveConfig — plumbing a
// parallel M4 step lands separately (see the TODO on
// [modelPickerView.handleKey]). This step ships the list, the active mark,
// and the empty/warn state only; Enter is a stub that leaves the view
// unchanged. Effort-adjust (←/→) is deferred — the SDK carries no per-model
// effort levels ([provider.ModelInfo.Reasoning] is a bool) — and ←/→ are
// already claimed by the panel host for tab switching (panel.go's
// commandPanel.handleKey), so there is no room to bind them here regardless.
import (
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// modelDisplayNames is gofer's own short display name per model id, keyed by
// the SDK catalog's id — provider.Lookup carries limits/pricing but no
// friendly name (docs/projects/gofer-m4-command-views-plan.md §4a). A model
// id absent from this table (a newly registered SDK model gofer hasn't
// labeled yet) falls back to the raw id, never a blank name.
var modelDisplayNames = map[string]string{
	"claude-fable-5":   "Fable 5",
	"claude-opus-4-8":  "Opus 4.8",
	"claude-sonnet-5":  "Sonnet 5",
	"claude-haiku-4-5": "Haiku 4.5",
	"gpt-5":            "GPT-5",
	"gpt-5-mini":       "GPT-5 mini",
	"gpt-5-nano":       "GPT-5 nano",
	"o4-mini":          "o4-mini",
}

// modelDisplayName returns id's short display name, falling back to id
// itself when the model isn't in [modelDisplayNames].
func modelDisplayName(id string) string {
	if name, ok := modelDisplayNames[id]; ok {
		return name
	}
	return id
}

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

	cursor int // highlighted row index into rows(); -1 = none highlighted
}

// newModelPickerView returns a Model tab reading through env, with sess and
// defaultModel captured at open time the same way status.go's statusView
// captures them (command.go's openPanel).
func newModelPickerView(th theme.Theme, env CommandEnv, sess *SessionInfo, defaultModel string) modelPickerView {
	return modelPickerView{theme: th, env: env, sess: sess, defaultModel: defaultModel, cursor: -1}
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

// lines builds the provider-grouped row list, or the empty-list warning when
// no provider is authenticated (§4c).
func (v modelPickerView) lines() []string {
	rows := v.rows()
	if len(rows) == 0 {
		return []string{v.theme.WarnStyle().Render("No providers authenticated. Run /login (or 'gofer login <anthropic|openai>').")}
	}
	active := v.activeModel()
	var out []string
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
	line := marker + check + modelDescriptionLine(r.id)
	if i == v.cursor {
		return v.theme.AccentStyle().Render(line)
	}
	return line
}

// modelDescriptionLine renders one model's display line: its short name (or
// raw id, see [modelDisplayName]) plus context window and per-Mtok pricing
// derived from provider.Lookup, e.g. "Sonnet 5 (claude-sonnet-5) · 1M
// context · $3/$15 per Mtok". A model id the catalog doesn't recognize (not
// reachable via rows(), which only ever lists provider.Models() ids, but
// defensive regardless) renders as the bare display name.
func modelDescriptionLine(id string) string {
	name := modelDisplayName(id)
	info, ok := provider.Lookup(id)
	if !ok {
		return fmt.Sprintf("%s (%s)", name, id)
	}
	return fmt.Sprintf("%s (%s) · %s context · $%s/$%s per Mtok",
		name, id, formatContextWindow(info.ContextWindow), formatPrice(info.Pricing.Input), formatPrice(info.Pricing.Output))
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

// rows returns the flattened, provider-then-id-sorted list of catalog models
// whose provider is authenticated ([modelPickerView.authedProviders]) — the
// intersection of provider.Models() and CommandEnv.Auth()'s providers (§4a).
// Zero providers authenticated yields an empty list, never an error.
func (v modelPickerView) rows() []modelRow {
	authed := v.authedProviders()
	if len(authed) == 0 {
		return nil
	}
	ids := provider.Models()
	rows := make([]modelRow, 0, len(ids))
	for _, id := range ids {
		info, ok := provider.Lookup(id)
		if !ok || !authed[info.Provider] {
			continue
		}
		rows = append(rows, modelRow{provider: info.Provider, id: id})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].provider != rows[j].provider {
			return rows[i].provider < rows[j].provider
		}
		return rows[i].id < rows[j].id
	})
	return rows
}

// authedProviders reads env.Auth, mirroring statusView.authList's non-fatal
// contract: a nil closure (the zero CommandEnv) or a read error is treated
// identically to "no providers authenticated" — never a reason to block the
// view. This is a lock-free local read (see CommandEnv.Auth's doc) — it
// never resolves a credential or hits a provider network call.
func (v modelPickerView) authedProviders() map[string]bool {
	set := map[string]bool{}
	if v.env.Auth == nil {
		return set
	}
	auths, err := v.env.Auth()
	if err != nil {
		return set
	}
	for _, a := range auths {
		set[a.Provider] = true
	}
	return set
}

// activeModel resolves the model the ✓ mark applies to: the attached/peeked
// session's own override, else the persisted session.model config default
// (env.Config, read the same lazy non-fatal way statusView's
// settingSourcesLine reads it), else the roster header's resolved default
// (defaultModel — the overview's resolveOverviewModel result, threaded in at
// open time).
func (v modelPickerView) activeModel() string {
	if v.sess != nil && v.sess.Model != "" {
		return v.sess.Model
	}
	if v.env.Config != nil {
		if cfg, err := v.env.Config(); err == nil && cfg.Session.Model != "" {
			return cfg.Session.Model
		}
	}
	return v.defaultModel
}

// handleKey applies one key press: ↓/↑ move the row highlight; Enter is
// currently a no-op stub.
//
// TODO(m4 step4): couple SetModel + config.Save once plumbing lands. Enter
// should call Supervisor.SetModel on the attached session (if any) and
// persist the selected id as session.model via env.SaveConfig — see
// docs/projects/gofer-m4-command-views-plan.md §4b. That needs
// Supervisor.SetModel (internal/tui/supervisor.go's Supervisor interface),
// which a parallel M4 step adds; this step deliberately leaves Enter inert
// so the view compiles and renders without it.
func (v modelPickerView) handleKey(msg tea.KeyPressMsg) modelPickerView {
	switch msg.Key().Code {
	case tea.KeyDown:
		return v.selectDown()
	case tea.KeyUp:
		return v.selectUp()
	case tea.KeyEnter:
		return v // TODO(m4 step4): couple SetModel + config.Save once plumbing lands.
	}
	return v
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
