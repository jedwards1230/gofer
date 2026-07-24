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
// The one thing it does think about is CAPABILITY, and it mirrors
// [runner.Runner.SetEffort]'s rule in BOTH directions — neither laxer nor
// stricter:
//
//   - A NON-EMPTY level is refused only on POSITIVE registry evidence that the
//     session's current model cannot reason; an unregistered model is UNKNOWN,
//     not "no", so it passes and the runner has the final word
//     ([effortCapable]). Being laxer would offer rows that always error.
//   - CLEARING ("") is refused never. The SDK's capability branch sits inside
//     `if effort != ""`, so `SetEffort("")` is legal for any model at all.
//     Being stricter here is not a harmless extra safety check: a session that
//     carried a level into a non-reasoning model still has one, and a picker
//     that refuses the clear leaves that level visible nowhere and removable
//     only through a command this screen never mentions. See
//     [effortPickerView.offOnly].
//
// NOTE the level set here does not yet reach a provider request at all — see
// docs/TUI.md's "Reasoning effort does not reach the provider yet (SDK gap)".
// That is a gap below this file, not in it.
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

// effortCapable reports whether a NON-EMPTY reasoning effort may be offered or
// applied for model. It reads the registry through [CommandEnv.ModelInfo],
// which is [provider.Lookup] in every production wiring (see that field for
// why the indirection exists, and why it is not a package-level var).
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
// CLEARING THE LEVEL ("") IS ALWAYS ALLOWED and must never be routed through
// here. The SDK admits `SetEffort("")` unconditionally — its capability branch
// sits inside `if effort != ""` — because asking for no reasoning is a
// coherent request for any model whatsoever. Every caller is responsible for
// checking `effort != ""` before consulting this function; a caller that
// forgets makes gofer STRICTER than the runner and turns this surface into a
// dead end for a session carrying a stale level (see [effortPickerView.offOnly]
// and [App.applyEffortSelection]).
func effortCapable(env CommandEnv, model string) bool {
	lookup := env.ModelInfo
	if lookup == nil {
		lookup = provider.Lookup
	}
	info, ok := lookup(model)
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
	// Clamp the seed into the selectable range: on a non-reasoning model only
	// the clear row can be chosen, and seeding onto a level row there would
	// open the picker with the highlight parked on something Enter refuses.
	if max := len(v.selectableLevels()) - 1; v.cursor > max {
		v.cursor = max
	}
	return v
}

// offOnly reports whether this picker may offer ONLY the leading clear row:
// the active model is one the registry KNOWS cannot reason, so no non-empty
// level applies to it.
//
// The clear row survives that refusal deliberately, and it is the whole point
// of this method existing rather than the view simply rendering a warning and
// nothing else. `SetEffort("")` is legal for ANY model — the SDK's capability
// branch sits inside `if effort != ""` — and a session that was carrying a
// level before the user switched to a non-reasoning model still has one. If
// this surface offered nothing at all, that stale level would be visible
// nowhere and clearable only via `/thinking off`, a command the very screen
// refusing to help does not mention. Offering the clear keeps the picker's
// gating exactly as strict as the runner's and no stricter.
func (v effortPickerView) offOnly() bool {
	return !effortCapable(v.env, v.activeModel())
}

// selectableLevels returns the rows Enter may commit: every level normally,
// just the leading clear row when [effortPickerView.offOnly]. It is the single
// definition the cursor clamp, the row rendering, and
// [effortPickerView.selectedEffort] all read, so "shown as available" and
// "actually committable" cannot drift apart.
//
// It relies on the clear being effortLevels[0]; that ordering is asserted by
// TestEffortLevelsLeadWithTheClear.
func (v effortPickerView) selectableLevels() []effortLevel {
	if v.offOnly() {
		return effortLevels[:1]
	}
	return effortLevels
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
// session's OWN level, else "" (off).
//
// It deliberately does NOT fall back to the persisted session.effort config
// default, which is where [modelPickerView.activeModel]'s analogous precedence
// has a middle rung. The ✓ is a claim about what is IN FORCE, and unlike
// session.model — which `resolveRunModel` genuinely feeds into every new
// session (cmd/gofer/run.go) — nothing yet feeds session.effort into a
// session's construction params. So with `session.effort: high` saved, a fresh
// session's runner sits at "" while a config rung here would render `✓ high`:
// a level no session ever received, asserted as current. Omitting what we
// cannot answer honestly beats blank-filling it (the rule status.go states).
//
// When the config default IS wired into session creation, the right change is
// to restore the rung — at which point it will be true.
func (v effortPickerView) activeEffort() string {
	if v.sess == nil {
		return ""
	}
	return v.sess.Effort
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
// level. On a model the registry KNOWS cannot reason the header becomes a
// warning and the level rows render muted-but-visible, with only the clear row
// still selectable.
//
// Keeping the unavailable rows ON SCREEN rather than dropping them is what lets
// the ✓ stay visible: a session that carried `high` into a non-reasoning model
// still has it, and a user cannot decide to clear a level this screen refuses
// to show them. The warning says what is wrong, the ✓ says what the session
// currently has, and the clear row is the way out — none of which is available
// if the tab collapses to a single complaint.
func (v effortPickerView) lines() []string {
	model := v.activeModel()

	var out []string
	if v.offOnly() {
		// Kept short on purpose: this line is truncated to the panel width, and
		// a remedy that falls off the right edge leaves only the complaint.
		out = append(out, v.theme.WarnStyle().Render(
			modelmeta.DisplayName(model)+" doesn't support reasoning effort — switch with /model."))
	} else {
		header := "Reasoning effort:"
		if model != "" {
			header = "Reasoning effort for " + modelmeta.DisplayName(model) + ":"
		}
		out = append(out, v.theme.MutedStyle().Render(header))
	}

	active := v.activeEffort()
	selectable := len(v.selectableLevels())
	for i, l := range effortLevels {
		out = append(out, v.rowLine(i, l, l.value == active, i < selectable))
	}
	return out
}

// rowLine renders one level's marker, active-mark, label, and blurb,
// accent-styling the highlighted row (i == cursor) — the same row vocabulary
// modelpicker.go's rowLine uses, so the two tabs read alike.
//
// An UNSELECTABLE row (a non-empty level on a non-reasoning model) renders
// muted and never carries the highlight marker, so "available" and "shown" are
// distinguishable at a glance. It still renders its ✓ when active: that is
// precisely the stale level the user needs to see in order to decide to clear
// it.
func (v effortPickerView) rowLine(i int, l effortLevel, isActive, selectable bool) string {
	marker := "  "
	if i == v.cursor && selectable {
		marker = "▸ "
	}
	check := "  "
	if isActive {
		check = "✓ "
	}
	line := marker + check + l.label + " — " + l.blurb
	switch {
	case !selectable:
		return v.theme.MutedStyle().Render(line)
	case i == v.cursor:
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
	if v.cursor < len(v.selectableLevels())-1 {
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
// commit at all. ok=false means no SELECTABLE row is highlighted — which on a
// non-reasoning model is any row past the clear
// ([effortPickerView.selectableLevels]), and otherwise only the no-highlight
// state.
//
// Note what is NOT a refusal: a non-reasoning model by itself. Clearing stays
// committable there, because the SDK admits `SetEffort("")` for any model at
// all. Gating the whole method on capability — as this did before — made gofer
// stricter than the runner and left a stale level unclearable from this tab.
//
// The (value, ok) pair is required rather than a bare string because "" is a
// LEGAL selection here — the clear row — so it cannot double as "nothing
// selected" the way [modelPickerView.selectedModel]'s empty return does.
func (v effortPickerView) selectedEffort() (string, bool) {
	selectable := v.selectableLevels()
	if v.cursor < 0 || v.cursor >= len(selectable) {
		return "", false
	}
	return selectable[v.cursor].value, true
}
