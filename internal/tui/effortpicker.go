package tui

// effortpicker.go implements the /thinking command-panel tab: a four-value
// picker over the SDK's unified reasoning-effort vocabulary
// ([provider.ValidEffort] — "low", "medium", "high", plus "" to clear back to
// the provider's own default, spelled "off" on screen).
//
// It is the effort-axis parallel of modelpicker.go and deliberately much
// smaller, because effort is a FIXED ENUM rather than a vendor catalog: there
// is nothing to discover, so this tab issues no request when it opens (compare
// [App.discoverModelsCmd], which only /model pays for), keeps no free-text
// entry (there is no id to type), and needs no floor/live distinction.
//
// The one thing it does think about is CAPABILITY. [runner.Runner.SetEffort]
// rejects a non-empty level when the registry has POSITIVE evidence the
// session's current model cannot reason — an unregistered model is UNKNOWN,
// not "no", so it passes. This view applies that identical rule to what it
// OFFERS ([effortCapable]), so the picker never presents four values the
// runner is going to refuse. Any drift between the two would show up as a
// selectable row that always errors.
//
// Like statusView/modelPickerView it is a pure value: every method returns an
// updated copy, so a fixed key sequence replays to the same rendered output in
// every golden test. Enter is a no-op here — the commit
// ([Supervisor.SetEffort] + the session.effort config write) needs IO a pure
// value has no seam for, so the panel host intercepts it (see
// [App.handleEffortSelect] in panel.go), reading
// [effortPickerView.selectedEffort] for what to commit.

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/modelmeta"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// effortLevel is one row of the picker: the value committed to
// [Supervisor.SetEffort] and the persisted session.effort default, plus how it
// is spelled and explained on screen.
type effortLevel struct {
	// value is the SDK-facing level. "" is a real, valid value (clear the
	// level), NOT a "nothing selected" marker — see [provider.ValidEffort].
	value string
	label string
	blurb string
}

// effortLevels is the picker's fixed row order, ascending. "off" leads because
// it is the state a session starts in (the runner sends no explicit level
// until something sets one), so the list reads as a ramp from the default
// upward rather than as an arbitrary vocabulary dump.
var effortLevels = []effortLevel{
	{"", "off", "no explicit level; the provider decides"},
	{provider.EffortLow, "low", "least reasoning, fastest turns"},
	{provider.EffortMedium, "medium", "balanced reasoning"},
	{provider.EffortHigh, "high", "most reasoning, slowest and priciest turns"},
}

// effortAliases maps the spellings a user may type at `/thinking <value>` onto
// the SDK level they mean. "off"/"none"/"default" all name the SAME thing —
// the empty level, i.e. "clear it and let the provider choose" — because there
// is no separate "unset" state to distinguish them from (see
// [config.Session.Effort]). Offering three words for it is not indecision: it
// is the one value whose obvious name differs per user, and guessing wrong
// costs a rejected command for a level the user spelled reasonably.
var effortAliases = map[string]string{
	"off":     "",
	"none":    "",
	"default": "",
	"low":     provider.EffortLow,
	"medium":  provider.EffortMedium,
	"high":    provider.EffortHigh,
}

// parseEffortArg resolves one `/thinking <value>` argument to an SDK level,
// case-insensitively. ok is false for anything the vocabulary does not carry —
// the caller reports that by name rather than silently opening the picker (the
// issue #165 rule, guarded generally by TestArgHintCommandsConsumeArgs).
//
// An empty/whitespace argument cannot reach here: parseSlash splits on
// whitespace and drops empty fields, so `/thinking` with nothing after it
// arrives as zero args (the bare, picker-opening form).
func parseEffortArg(arg string) (string, bool) {
	level, ok := effortAliases[strings.ToLower(strings.TrimSpace(arg))]
	return level, ok
}

// effortLabel renders a level for a status note or a settings row: the empty
// level reads "off", never as an empty string that would leave a sentence
// dangling.
func effortLabel(effort string) string {
	for _, l := range effortLevels {
		if l.value == effort {
			return l.label
		}
	}
	return effort
}

// modelRegistry is [provider.Lookup] behind a package variable, for ONE
// reason: every model in the SDK's compiled-in registry today carries
// Reasoning:true, so [effortCapable]'s refusal branch — the load-bearing half
// of this file — is unreachable from any test that can only name real ids, and
// an unreachable branch is an untested one that rots the first time the
// registry gains a non-reasoning model. Tests substitute a registry that does
// say no (see effortpicker_test.go's withNonReasoningRegistry); nothing in production
// ever assigns to it.
var modelRegistry = provider.Lookup

// effortCapable reports whether a non-empty reasoning effort may be offered or
// applied for model.
//
// This MIRRORS [runner.Runner.SetEffort]'s own check by design, and the
// asymmetry is the whole point: it rejects ONLY on positive registry evidence
// (Lookup found the model AND says it does not reason). A model the registry
// has never heard of — anything newer than this binary — is UNKNOWN, not
// incapable, so it passes and the runner gets the final word. Gating on
// registry membership instead would silently refuse the effort control for
// exactly the newest models, which is the same class of bug the picker's
// Resolve-not-Lookup admission rule exists to avoid (see modelpicker.go).
//
// Clearing the level ("") is always allowed and never reaches here — model
// capability is moot when asking for no reasoning at all.
func effortCapable(model string) bool {
	info, ok := modelRegistry(model)
	return !ok || info.Reasoning
}

// effortPickerView renders the Thinking tab.
type effortPickerView struct {
	theme theme.Theme
	env   CommandEnv
	sess  *SessionInfo // nil on the overview — no active session

	// defaultModel is the roster header's resolved credential-driven default,
	// the last rung of [activeModelFor]'s precedence — this view reads it only
	// to decide WHICH MODEL's reasoning capability gates the list.
	defaultModel string

	// cursor is the highlighted row index into [effortLevels]; -1 = none. It is
	// seeded on the ACTIVE level rather than at -1 (the Model tab's initial
	// state), because this list is four known values with a known current one:
	// starting on it means ↑/↓ move relative to where the session actually is,
	// and an immediate Enter re-commits the level already in force, which is
	// idempotent. The Model tab cannot do that — it opens onto a long catalog
	// beside a free-text entry, where a pre-seeded highlight would out-rank
	// whatever the user starts typing.
	cursor int
}

// newEffortPickerView returns a Thinking tab reading through env, with sess and
// defaultModel captured at open time the same way the other tabs capture them
// (command.go's openPanel).
func newEffortPickerView(th theme.Theme, env CommandEnv, sess *SessionInfo, defaultModel string) effortPickerView {
	v := effortPickerView{theme: th, env: env, sess: sess, defaultModel: defaultModel}
	v.cursor = indexOfEffort(v.activeEffort())
	return v
}

// indexOfEffort returns effort's row index in [effortLevels], or -1 for a level
// the vocabulary does not carry (only reachable from a hand-edited config.json).
func indexOfEffort(effort string) int {
	for i, l := range effortLevels {
		if l.value == effort {
			return i
		}
	}
	return -1
}

// activeModel resolves the model whose reasoning capability gates this tab —
// the same precedence the Model tab's ✓ mark follows, from the same helper, so
// the two tabs can never disagree about which model is active.
func (v effortPickerView) activeModel() string {
	return activeModelFor(v.env, v.sess, v.defaultModel)
}

// activeEffort resolves the level the ✓ mark applies to: the attached/peeked
// session's own level, else the persisted session.effort config default, else
// "" (off). It mirrors [modelPickerView.activeModel]'s shape one rung shorter —
// there is no credential-derived fallback for effort, since no provider
// advertises one.
func (v effortPickerView) activeEffort() string {
	if v.sess != nil && v.sess.Effort != "" {
		return v.sess.Effort
	}
	if v.env.Config != nil {
		if cfg, err := v.env.Config(); err == nil && cfg.Session.Effort != "" {
			return cfg.Session.Effort
		}
	}
	return ""
}

// View renders the view's rows, width-truncated and capped to height — the
// same Renderable contract every other panel/screen component follows
// (testkit.Renderable). A zero or negative height renders nothing at all rather
// than one row: the panel host can legitimately ask for zero body rows on a
// short terminal (see commandPanel.View's bodyRows), and a first frame arriving
// before the WindowSizeMsg has height 0.
func (v effortPickerView) View(width, height int) string {
	lines := v.lines()
	if height >= 0 && len(lines) > height {
		lines = lines[:height]
	}
	for i, l := range lines {
		lines[i] = truncate(l, width)
	}
	return strings.Join(lines, "\n")
}

// lines builds the header naming the model under discussion plus one row per
// level — or, when that model is one the registry KNOWS cannot reason, a single
// warning line in place of the rows. Refusing outright is the honest rendering:
// offering four selectable levels the runner will reject would make the picker
// a menu of errors, and the remedy (switch models first) is what the user
// actually needs told.
func (v effortPickerView) lines() []string {
	model := v.activeModel()
	if !effortCapable(model) {
		// Kept short on purpose: this line is truncated to the panel width, and
		// a remedy that falls off the right edge leaves only the complaint.
		return []string{v.theme.WarnStyle().Render(
			modelmeta.DisplayName(model) + " doesn't support reasoning effort — switch with /model.")}
	}

	header := "Reasoning effort:"
	if model != "" {
		header = "Reasoning effort for " + modelmeta.DisplayName(model) + ":"
	}
	out := []string{v.theme.MutedStyle().Render(header)}

	active := v.activeEffort()
	for i, l := range effortLevels {
		out = append(out, v.rowLine(i, l, l.value == active))
	}
	return out
}

// rowLine renders one level's marker, active-mark, label, and blurb,
// accent-styling the highlighted row (i == cursor) — the same row vocabulary
// modelpicker.go's rowLine uses, so the two tabs read alike.
func (v effortPickerView) rowLine(i int, l effortLevel, isActive bool) string {
	marker := "  "
	if i == v.cursor {
		marker = "▸ "
	}
	check := "  "
	if isActive {
		check = "✓ "
	}
	line := marker + check + l.label + " — " + l.blurb
	if i == v.cursor {
		return v.theme.AccentStyle().Render(line)
	}
	return line
}

// handleKey applies one key press: ↓/↑ move the row highlight. Enter is a
// no-op HERE — this is a pure value with no IO seam; the real coupled select
// (SetEffort + config.Save) lives one level up in [App.handleEffortSelect]
// (panel.go), which intercepts Enter ahead of this method. There is no typing
// affordance at all: the vocabulary is closed, so free text has nothing to
// name.
func (v effortPickerView) handleKey(msg tea.KeyPressMsg) effortPickerView {
	switch msg.Key().Code {
	case tea.KeyDown:
		return v.selectDown()
	case tea.KeyUp:
		return v.selectUp()
	}
	return v
}

// selectDown moves the highlight onto the first row (from no highlight) or one
// row further down, clamped at the last row — [modelPickerView.selectDown]'s
// contract, over a fixed list.
func (v effortPickerView) selectDown() effortPickerView {
	if v.cursor < 0 {
		v.cursor = 0
		return v
	}
	if v.cursor < len(effortLevels)-1 {
		v.cursor++
	}
	return v
}

// selectUp moves the highlight up one row, clamped at the first row.
func (v effortPickerView) selectUp() effortPickerView {
	if v.cursor > 0 {
		v.cursor--
	}
	return v
}

// selectedEffort returns what Enter commits and whether there is anything to
// commit at all. ok=false covers both refusals: a model that cannot reason (no
// rows are offered, so nothing is selectable) and no row highlighted.
//
// The (value, ok) pair is required rather than a bare string because "" is a
// LEGAL selection here — the off row, which clears the level back to the
// provider's default — so it cannot double as "nothing selected" the way
// [modelPickerView.selectedModel]'s empty return does.
func (v effortPickerView) selectedEffort() (string, bool) {
	if !effortCapable(v.activeModel()) {
		return "", false
	}
	if v.cursor < 0 || v.cursor >= len(effortLevels) {
		return "", false
	}
	return effortLevels[v.cursor].value, true
}
