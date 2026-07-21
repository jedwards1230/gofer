package tui

// effortpicker_test.go covers the /thinking command-panel tab
// (effortpicker.go): the four-level list, the ✓ mark's precedence (session
// level > session.effort config default > off), the cursor seeded on the
// active row, the reasoning-capability gate that replaces the list with a
// refusal, the argument parser behind `/thinking <level>`, and the
// small/zero-height render. White-box (package tui) because effortPickerView
// and the model-registry seam are unexported; the App-level "/thinking applies
// it" behavior is covered in effort_select_test.go (package tui_test) and,
// for the capability refusal, by TestThinkingArgNonReasoningModelRefuses below
// — which needs the seam and so has to live here.

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// nonReasoningModel is the id [withNonReasoningRegistry] teaches the registry
// to report as reasoning-incapable. It is deliberately a REGISTERED-looking
// anthropic id: the whole point of the rule under test is that only positive
// registry evidence refuses, so a fixture the registry has never heard of
// would prove the opposite thing.
const nonReasoningModel = "claude-no-think-1"

// withNonReasoningRegistry swaps the package's [modelRegistry] seam for one
// that reports nonReasoningModel as registered-and-not-reasoning, restoring it
// on cleanup.
//
// The seam exists because every model in the SDK's compiled-in registry today
// carries Reasoning:true, so the refusal branch is otherwise unreachable from a
// test — see modelRegistry's doc. Everything else falls through to the real
// [provider.Lookup], so the capable cases in this file are still judged by the
// genuine registry.
func withNonReasoningRegistry(t *testing.T) {
	t.Helper()
	prev := modelRegistry
	t.Cleanup(func() { modelRegistry = prev })
	modelRegistry = func(id string) (provider.ModelInfo, bool) {
		if id == nonReasoningModel {
			return provider.ModelInfo{ID: id, Provider: "anthropic", Reasoning: false}, true
		}
		return prev(id)
	}
}

// effortTestEnv returns a CommandEnv fixed to GoldenCommandEnv's identity with
// Config answering cfg, so the ✓ mark's config rung is controllable without
// touching a real file.
func effortTestEnv(cfg config.Config) CommandEnv {
	env := GoldenCommandEnv()
	env.Config = func() (config.Config, error) { return cfg, nil }
	return env
}

func renderEffort(t *testing.T, name string, v effortPickerView) {
	t.Helper()
	testkit.AssertGolden(t, name, testkit.Render(v, testkit.Width, testkit.Height))
}

// TestGoldenEffortDefault covers the opening state on a reasoning model with
// nothing set: every level listed, the ✓ on "off", and the highlight seeded on
// that same row.
func TestGoldenEffortDefault(t *testing.T) {
	v := newEffortPickerView(theme.Test(), effortTestEnv(config.Config{}), nil, "claude-sonnet-5")
	renderEffort(t, "effort_default", v)
}

// TestGoldenEffortSessionLevel covers an attached session already running at a
// level: the ✓ and the seeded highlight both follow the SESSION, not the
// config default.
func TestGoldenEffortSessionLevel(t *testing.T) {
	sess := &SessionInfo{ID: "0192a1b2-eff0-7000-8000-000000000001", Model: "claude-sonnet-5", Effort: provider.EffortHigh}
	cfg := config.Config{Session: config.Session{Effort: provider.EffortLow}}
	renderEffort(t, "effort_session_level", newEffortPickerView(theme.Test(), effortTestEnv(cfg), sess, "claude-sonnet-5"))
}

// TestGoldenEffortNonReasoningModel covers the capability refusal: the list is
// replaced by one warning line naming the remedy, so the picker is never a menu
// of levels the runner will reject.
func TestGoldenEffortNonReasoningModel(t *testing.T) {
	withNonReasoningRegistry(t)
	v := newEffortPickerView(theme.Test(), effortTestEnv(config.Config{}), nil, nonReasoningModel)
	renderEffort(t, "effort_non_reasoning", v)
}

// TestGoldenEffortNonReasoningModelStyled is its color-state counterpart: the
// refusal must render in WarnStyle rather than as an ordinary row, a
// distinction the Ascii golden above is blind to.
func TestGoldenEffortNonReasoningModelStyled(t *testing.T) {
	withNonReasoningRegistry(t)
	v := newEffortPickerView(testkit.ColorTheme(), effortTestEnv(config.Config{}), nil, nonReasoningModel)
	testkit.AssertGoldenStyled(t, "effort_non_reasoning", testkit.Render(v, testkit.Width, testkit.Height))
}

// TestEffortSmallAndZeroHeightRenders is the first-frame guard. A zero-height
// render is not hypothetical: the panel host hands the body whatever rows are
// left after its chrome (which can be none on a short terminal), and a frame
// arriving before the first WindowSizeMsg has height 0. This TUI has panicked
// on exactly that before, so every size from 0 up through the full list is
// exercised, at a degenerate width too.
func TestEffortSmallAndZeroHeightRenders(t *testing.T) {
	views := map[string]effortPickerView{
		"reasoning": newEffortPickerView(theme.Test(), effortTestEnv(config.Config{}), nil, "claude-sonnet-5"),
	}
	withNonReasoningRegistry(t)
	views["non-reasoning"] = newEffortPickerView(theme.Test(), effortTestEnv(config.Config{}), nil, nonReasoningModel)

	for name, v := range views {
		t.Run(name, func(t *testing.T) {
			for _, size := range []struct{ w, h int }{{0, 0}, {1, 0}, {0, 1}, {1, 1}, {80, 0}, {80, 1}, {80, 3}, {80, 24}} {
				got := v.View(size.w, size.h)
				if size.h == 0 && got != "" {
					t.Errorf("View(%d, 0) = %q, want the empty string — zero rows means render nothing", size.w, got)
				}
				if lines := strings.Count(got, "\n") + 1; got != "" && size.h > 0 && lines > size.h {
					t.Errorf("View(%d, %d) rendered %d lines, want at most %d", size.w, size.h, lines, size.h)
				}
			}
		})
	}
}

// TestEffortActiveLevelPrecedence covers all three rungs of the ✓ mark's
// precedence in one table, including the case that makes the order matter: a
// session level and a config default that disagree.
func TestEffortActiveLevelPrecedence(t *testing.T) {
	sessAt := func(effort string) *SessionInfo {
		return &SessionInfo{ID: "s", Model: "claude-sonnet-5", Effort: effort}
	}
	cases := []struct {
		name string
		sess *SessionInfo
		cfg  config.Config
		want string
	}{
		{"nothing set", nil, config.Config{}, ""},
		{"config default", nil, config.Config{Session: config.Session{Effort: provider.EffortLow}}, provider.EffortLow},
		{"session beats config", sessAt(provider.EffortHigh), config.Config{Session: config.Session{Effort: provider.EffortLow}}, provider.EffortHigh},
		{"unset session falls back to config", sessAt(""), config.Config{Session: config.Session{Effort: provider.EffortMedium}}, provider.EffortMedium},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := newEffortPickerView(theme.Test(), effortTestEnv(tc.cfg), tc.sess, "claude-sonnet-5")
			if got := v.activeEffort(); got != tc.want {
				t.Fatalf("activeEffort() = %q, want %q", got, tc.want)
			}
			// The seeded cursor must agree with the ✓, or ↑/↓ would move
			// relative to a row the user is not looking at.
			if got, want := v.cursor, indexOfEffort(tc.want); got != want {
				t.Errorf("seeded cursor = %d, want %d (the active row)", got, want)
			}
			if got := v.View(testkit.Width, testkit.Height); !strings.Contains(got, "✓ "+effortLabel(tc.want)) {
				t.Errorf("expected the ✓ on %q, got:\n%s", effortLabel(tc.want), got)
			}
		})
	}
}

// TestEffortCursorMovesAndClamps covers the row navigation: ↓ advances and
// stops at the last row, ↑ walks back and stops at the first.
func TestEffortCursorMovesAndClamps(t *testing.T) {
	v := newEffortPickerView(theme.Test(), effortTestEnv(config.Config{}), nil, "claude-sonnet-5")
	if v.cursor != 0 {
		t.Fatalf("test premise broken: expected the off row seeded, cursor=%d", v.cursor)
	}

	for range len(effortLevels) + 3 {
		v = v.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	if want := len(effortLevels) - 1; v.cursor != want {
		t.Fatalf("cursor after running off the bottom = %d, want the clamp at %d", v.cursor, want)
	}
	if got, _ := v.selectedEffort(); got != provider.EffortHigh {
		t.Errorf("selectedEffort() at the last row = %q, want %q", got, provider.EffortHigh)
	}

	for range len(effortLevels) + 3 {
		v = v.handleKey(tea.KeyPressMsg{Code: tea.KeyUp})
	}
	if v.cursor != 0 {
		t.Fatalf("cursor after running off the top = %d, want the clamp at 0", v.cursor)
	}
}

// TestEffortSelectedClearIsALegitimateSelection is why selectedEffort returns
// a (value, ok) pair rather than a bare string the way the model picker's does:
// "" is a REAL selection here (clear the level), so it cannot double as
// "nothing selected".
func TestEffortSelectedClearIsALegitimateSelection(t *testing.T) {
	cfg := config.Config{Session: config.Session{Effort: provider.EffortHigh}}
	v := newEffortPickerView(theme.Test(), effortTestEnv(cfg), nil, "claude-sonnet-5")

	// Walk up to the off row from the seeded high row.
	for range len(effortLevels) {
		v = v.handleKey(tea.KeyPressMsg{Code: tea.KeyUp})
	}

	got, ok := v.selectedEffort()
	if !ok {
		t.Fatal("selectedEffort() reported nothing selectable on the off row; the clear must be committable")
	}
	if got != "" {
		t.Fatalf("selectedEffort() on the off row = %q, want \"\"", got)
	}
}

// TestEffortNonReasoningModelOffersNothing is the picker half of the capability
// gate: with a model the registry says cannot reason, no level is selectable at
// all, so Enter cannot commit one.
func TestEffortNonReasoningModelOffersNothing(t *testing.T) {
	withNonReasoningRegistry(t)
	v := newEffortPickerView(theme.Test(), effortTestEnv(config.Config{}), nil, nonReasoningModel)

	if _, ok := v.selectedEffort(); ok {
		t.Fatal("selectedEffort() reported something selectable for a non-reasoning model")
	}
	got := v.View(testkit.Width, testkit.Height)
	if !strings.Contains(got, "doesn't support reasoning effort") {
		t.Fatalf("expected the refusal on screen, got:\n%s", got)
	}
	for _, level := range []string{"low", "medium", "high"} {
		if strings.Contains(got, "— ") && strings.Contains(got, "  "+level+" —") {
			t.Fatalf("expected no selectable level rows for a non-reasoning model, got:\n%s", got)
		}
	}
}

// TestEffortUnregisteredModelIsOffered is the other half of the same rule, and
// the one a stricter check would get wrong: an id the registry has never heard
// of — anything newer than this binary — is UNKNOWN, not incapable, so the
// picker offers the levels and lets the runner have the final word. This
// mirrors [runner.Runner.SetEffort] exactly.
func TestEffortUnregisteredModelIsOffered(t *testing.T) {
	const unregistered = "claude-sonnet-9-future"
	if _, ok := provider.Lookup(unregistered); ok {
		t.Fatalf("test premise broken: %q is now registered", unregistered)
	}
	v := newEffortPickerView(theme.Test(), effortTestEnv(config.Config{}), nil, unregistered)

	if _, ok := v.selectedEffort(); !ok {
		t.Fatal("selectedEffort() refused an UNREGISTERED model; only positive evidence of no reasoning may refuse")
	}
	if got := v.View(testkit.Width, testkit.Height); strings.Contains(got, "doesn't support reasoning effort") {
		t.Fatalf("expected the levels offered for an unregistered model, got:\n%s", got)
	}
}

// TestEffortCapableMirrorsTheSDKRule pins [effortCapable]'s three-way verdict
// directly, including the branch the SDK registry cannot currently produce.
func TestEffortCapableMirrorsTheSDKRule(t *testing.T) {
	withNonReasoningRegistry(t)
	cases := map[string]bool{
		"claude-sonnet-5":        true,  // registered, reasoning
		nonReasoningModel:        false, // registered, explicitly not reasoning
		"claude-sonnet-9-future": true,  // unregistered — unknown, not "no"
		"":                       true,  // no model resolved at all
	}
	for model, want := range cases {
		if got := effortCapable(model); got != want {
			t.Errorf("effortCapable(%q) = %v, want %v", model, got, want)
		}
	}
}

// TestEffortZeroCommandEnvDoesNotPanic covers the zero CommandEnv (nil
// Config closure) — the state a caller gets if it forgets to wire one —
// rendering the level list rather than panicking, matching the
// auth-independence contract every panel view honors.
func TestEffortZeroCommandEnvDoesNotPanic(t *testing.T) {
	v := effortPickerView{theme: theme.Test()}
	got := v.View(testkit.Width, testkit.Height)
	if !strings.Contains(got, "off — ") {
		t.Fatalf("expected the level list under a zero CommandEnv, got:\n%s", got)
	}
}

// TestEffortConfigErrorTreatedAsUnset covers the non-fatal contract: a read
// error from env.Config resolves the active level to "off" rather than
// blocking the view.
func TestEffortConfigErrorTreatedAsUnset(t *testing.T) {
	env := GoldenCommandEnv()
	env.Config = func() (config.Config, error) { return config.Config{}, errors.New("boom") }
	v := newEffortPickerView(theme.Test(), env, nil, "claude-sonnet-5")

	if got := v.activeEffort(); got != "" {
		t.Fatalf("activeEffort() with a failing Config = %q, want \"\"", got)
	}
}

// TestParseEffortArg pins the `/thinking <level>` vocabulary, including the
// three accepted spellings of the clear and the case-insensitivity.
func TestParseEffortArg(t *testing.T) {
	accepted := map[string]string{
		"low": provider.EffortLow, "LOW": provider.EffortLow,
		"medium": provider.EffortMedium, "High": provider.EffortHigh,
		"off": "", "none": "", "default": "", " off ": "",
	}
	for arg, want := range accepted {
		got, ok := parseEffortArg(arg)
		if !ok {
			t.Errorf("parseEffortArg(%q) rejected a level it must accept", arg)
			continue
		}
		if got != want {
			t.Errorf("parseEffortArg(%q) = %q, want %q", arg, got, want)
		}
	}
	for _, arg := range []string{"ultra", "max", "true", "1", "lo", "no-such-vendor/no-such-model-165"} {
		if _, ok := parseEffortArg(arg); ok {
			t.Errorf("parseEffortArg(%q) accepted a value outside the vocabulary", arg)
		}
	}
}

// TestThinkingArgNonReasoningModelRefuses is the capability gate on the STRING
// path, and the assertion the issue names explicitly: `/thinking high` on a
// model the registry says cannot reason must refuse by name and reach neither
// the config write nor Supervisor.SetEffort.
//
// It lives in this white-box file rather than beside its siblings in
// effort_select_test.go because it needs the [modelRegistry] seam. "SetEffort
// was not called" is asserted through the returned tea.Cmd being nil: that
// command is the ONLY route from here to the supervisor (see
// applyEffortSelection), so a nil one is proof no call was dispatched.
func TestThinkingArgNonReasoningModelRefuses(t *testing.T) {
	withNonReasoningRegistry(t)

	var saved []config.Config
	env := GoldenCommandEnv()
	env.Config = func() (config.Config, error) { return config.Config{}, nil }
	env.SaveConfig = func(c config.Config) error { saved = append(saved, c); return nil }

	a := NewApp(theme.Test(), newInternalFakeSup(GoldenRoster()), GoldenMeta(), env)
	a.over = a.over.WithDefaultModel(nonReasoningModel)

	got, cmd := runThinking(a, []string{"high"})

	if cmd != nil {
		t.Error("expected NO follow-on command — a refused level must not reach Supervisor.SetEffort")
	}
	if len(saved) != 0 {
		t.Errorf("SaveConfig calls = %v; want none — a level the model can't use must not be persisted either", saved)
	}
	if got.statusSev != sevDanger || got.status == "" {
		t.Fatalf("status = %q (severity %v); want a non-empty sevDanger refusal", got.status, got.statusSev)
	}
	if !strings.Contains(got.status, "reasoning effort") {
		t.Errorf("status = %q, want it to say why the level was refused", got.status)
	}
	if got.panel != nil {
		t.Error("expected no command panel opened by a refusal")
	}
}

// TestThinkingArgClearIsAllowedOnANonReasoningModel is the refusal's boundary:
// clearing the level asks for NO reasoning, so model capability is moot and the
// SDK allows it unconditionally. A gate that refused every /thinking on a
// non-reasoning model would strand a session that already had a level set.
func TestThinkingArgClearIsAllowedOnANonReasoningModel(t *testing.T) {
	withNonReasoningRegistry(t)

	var saved []config.Config
	env := GoldenCommandEnv()
	env.Config = func() (config.Config, error) {
		return config.Config{Session: config.Session{Effort: provider.EffortHigh}}, nil
	}
	env.SaveConfig = func(c config.Config) error { saved = append(saved, c); return nil }

	a := NewApp(theme.Test(), newInternalFakeSup(GoldenRoster()), GoldenMeta(), env)
	a.over = a.over.WithDefaultModel(nonReasoningModel)

	got, _ := runThinking(a, []string{"off"})

	if len(saved) != 1 || saved[0].Session.Effort != "" {
		t.Fatalf("SaveConfig calls = %v; want one entry clearing Session.Effort", saved)
	}
	if got.statusSev == sevDanger {
		t.Errorf("status = %q (danger); clearing the level must not be refused", got.status)
	}
}
