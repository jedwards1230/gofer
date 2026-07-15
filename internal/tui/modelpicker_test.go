package tui

// modelpicker_test.go covers the /model command-panel tab (modelpicker.go,
// M4 step 4): the catalog list filtered to authenticated providers, the
// active (✓) mark's precedence (session override > session.model config
// default > resolved roster default), the description-line formatting, the
// empty+warn state with zero providers authenticated (§4c), row-highlight
// navigation, and the auth-independence contract (a nil/erroring Auth or
// Config never blocks the view, and Enter is a no-op stub — see the TODO on
// handleKey). White-box (package tui) because modelPickerView is unexported —
// the App-level "/model opens the panel" behavior is covered separately in
// command_test.go (package tui_test).

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// modelTestEnv returns a CommandEnv fixed to GoldenCommandEnv's
// version/cwd/root identity, with Auth swapped for a closure a test controls
// directly — no real files touched, so the golden renders stay deterministic.
func modelTestEnv(auths []ProviderAuth, authErr error) CommandEnv {
	env := GoldenCommandEnv()
	env.Auth = func() ([]ProviderAuth, error) { return auths, authErr }
	return env
}

// fixtureModelSession is the SessionInfo the attached/peeked-session tests
// render against — deliberately different from the default model so the
// active-mark's "session override beats the default" precedence is visible.
func fixtureModelSession() *SessionInfo {
	return &SessionInfo{
		ID:    "0192a1b2-mdl0-7000-8000-000000000001",
		Title: "pick a model",
		Model: "claude-haiku-4-5",
	}
}

func renderModel(t *testing.T, name string, v modelPickerView) {
	t.Helper()
	testkit.AssertGolden(t, name, testkit.Render(v, testkit.Width, testkit.Height))
}

func renderModelStyled(t *testing.T, name string, v modelPickerView) {
	t.Helper()
	testkit.AssertGoldenStyled(t, name, testkit.Render(v, testkit.Width, testkit.Height))
}

// TestGoldenModelZeroAuth covers the auth-independence default (§4c, §5): no
// providers authenticated collapses to the empty-list warning line, never a
// blank list.
func TestGoldenModelZeroAuth(t *testing.T) {
	v := newModelPickerView(theme.Test(), modelTestEnv(nil, nil), nil, "")
	renderModel(t, "model_zero_auth", v)
}

// TestGoldenModelZeroAuthStyled is TestGoldenModelZeroAuth's color-state
// counterpart: the warning renders in WarnStyle, invisible under
// theme.Test()'s forced Ascii profile.
func TestGoldenModelZeroAuthStyled(t *testing.T) {
	v := newModelPickerView(testkit.ColorTheme(), modelTestEnv(nil, nil), nil, "")
	renderModelStyled(t, "model_zero_auth", v)
}

// TestGoldenModelSingleProviderAuthed covers one authenticated provider: only
// its models list, with the ✓ mark on defaultModel.
func TestGoldenModelSingleProviderAuthed(t *testing.T) {
	auths := []ProviderAuth{{Provider: "anthropic", Kind: KindOAuth}}
	v := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), nil, "claude-sonnet-5")
	renderModel(t, "model_single_provider", v)
}

// TestGoldenModelMultipleProvidersAuthed covers gofer's two-provider ceiling
// (runner.SupportedProviders): both providers' models render, grouped and
// sorted by provider name, given out of alphabetical order to prove the
// grouping doesn't depend on Auth's return order.
func TestGoldenModelMultipleProvidersAuthed(t *testing.T) {
	auths := []ProviderAuth{
		{Provider: "openai", Kind: KindAPIKey},
		{Provider: "anthropic", Kind: KindOAuth},
	}
	v := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), nil, "gpt-5")
	renderModel(t, "model_multiple_providers", v)
}

// TestGoldenModelOverviewDefault covers the no-active-session case: the ✓
// mark falls back to defaultModel (the roster header's resolved default)
// since no session and no session.model config default override it.
func TestGoldenModelOverviewDefault(t *testing.T) {
	auths := []ProviderAuth{{Provider: "anthropic", Kind: KindOAuth}}
	v := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), nil, "claude-haiku-4-5")
	renderModel(t, "model_overview_default", v)
}

// TestGoldenModelAttachedOverride covers the peeked/attached case: the ✓ mark
// follows the session's own model override rather than falling back to
// defaultModel.
func TestGoldenModelAttachedOverride(t *testing.T) {
	auths := []ProviderAuth{{Provider: "anthropic", Kind: KindOAuth}}
	v := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), fixtureModelSession(), "claude-sonnet-5")
	renderModel(t, "model_attached_override", v)
}

// TestModelActiveModelPrefersConfigOverDefault covers the middle rung of the
// precedence order: with no attached session, a persisted session.model
// config default outranks defaultModel.
func TestModelActiveModelPrefersConfigOverDefault(t *testing.T) {
	env := GoldenCommandEnv()
	env.Auth = func() ([]ProviderAuth, error) {
		return []ProviderAuth{{Provider: "anthropic", Kind: KindOAuth}}, nil
	}
	env.Config = func() (config.Config, error) {
		return config.Config{Session: config.Session{Model: "claude-haiku-4-5"}}, nil
	}
	v := newModelPickerView(theme.Test(), env, nil, "claude-sonnet-5")

	got := v.View(testkit.Width, testkit.Height)
	if !strings.Contains(got, "✓ Haiku 4.5") {
		t.Fatalf("expected the session.model config default to win over defaultModel, got:\n%s", got)
	}
	if strings.Contains(got, "✓ Sonnet 5") {
		t.Fatalf("expected defaultModel to be overridden, got:\n%s", got)
	}
}

// TestModelActiveModelPrefersSessionOverConfig covers the top rung: an
// attached session's own model override outranks both the config default and
// defaultModel.
func TestModelActiveModelPrefersSessionOverConfig(t *testing.T) {
	env := GoldenCommandEnv()
	env.Auth = func() ([]ProviderAuth, error) {
		return []ProviderAuth{{Provider: "anthropic", Kind: KindOAuth}}, nil
	}
	env.Config = func() (config.Config, error) {
		return config.Config{Session: config.Session{Model: "claude-opus-4-8"}}, nil
	}
	v := newModelPickerView(theme.Test(), env, fixtureModelSession(), "claude-sonnet-5") // sess.Model: "claude-haiku-4-5"

	got := v.View(testkit.Width, testkit.Height)
	if !strings.Contains(got, "✓ Haiku 4.5") {
		t.Fatalf("expected the attached session's model to win, got:\n%s", got)
	}
}

// TestModelZeroCommandEnvDoesNotPanic covers the zero CommandEnv (nil
// Auth/Config closures) — the state a caller gets if it forgets to wire one
// — rendering the empty-list warning rather than panicking.
func TestModelZeroCommandEnvDoesNotPanic(t *testing.T) {
	v := modelPickerView{theme: theme.Test()}
	got := v.View(testkit.Width, testkit.Height)
	if !strings.Contains(got, "No providers authenticated") {
		t.Fatalf("expected the zero CommandEnv to render the empty-list warning, got:\n%s", got)
	}
}

// TestModelAuthErrorTreatedAsNoProviders covers auth-independence's
// non-fatal contract: a read error from env.Auth renders exactly like zero
// providers, never blocking the view.
func TestModelAuthErrorTreatedAsNoProviders(t *testing.T) {
	v := modelPickerView{theme: theme.Test(), env: modelTestEnv(nil, errors.New("boom"))}
	got := v.View(testkit.Width, testkit.Height)
	if !strings.Contains(got, "No providers authenticated") {
		t.Fatalf("expected an Auth error to render as no providers, got:\n%s", got)
	}
}

// TestModelCursorMovesDownAndUp covers the row-highlight navigation: ↓ from
// no highlight lands on the first row and advances, ↑ walks back up.
func TestModelCursorMovesDownAndUp(t *testing.T) {
	auths := []ProviderAuth{{Provider: "anthropic", Kind: KindOAuth}}
	v := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), nil, "claude-sonnet-5")
	if v.cursor != -1 {
		t.Fatalf("expected no row highlighted initially, got cursor=%d", v.cursor)
	}

	v = v.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	if v.cursor != 0 {
		t.Fatalf("expected the first down-press to highlight row 0, got cursor=%d", v.cursor)
	}

	v = v.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	if v.cursor != 1 {
		t.Fatalf("expected a second down-press to move to row 1, got cursor=%d", v.cursor)
	}

	v = v.handleKey(tea.KeyPressMsg{Code: tea.KeyUp})
	if v.cursor != 0 {
		t.Fatalf("expected up to move back to row 0, got cursor=%d", v.cursor)
	}
}

// TestModelCursorClampsAtBounds covers both ends of the row list: ↓ stops at
// the last row instead of running off the end, and ↑ stops at the first row
// rather than dropping the highlight (unlike configView.selectUp — the Model
// tab has no filter box for a dropped highlight to mean anything).
func TestModelCursorClampsAtBounds(t *testing.T) {
	auths := []ProviderAuth{{Provider: "anthropic", Kind: KindOAuth}}
	v := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), nil, "claude-sonnet-5")
	want := len(v.rows()) - 1

	for i := 0; i < len(v.rows())+3; i++ {
		v = v.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	if v.cursor != want {
		t.Fatalf("expected down to clamp at the last row (%d), got cursor=%d", want, v.cursor)
	}

	for i := 0; i < len(v.rows())+3; i++ {
		v = v.handleKey(tea.KeyPressMsg{Code: tea.KeyUp})
	}
	if v.cursor != 0 {
		t.Fatalf("expected up to clamp at row 0, got cursor=%d", v.cursor)
	}
}

// TestModelEnterIsNoOpStub covers the deliberately-held Enter/select coupling
// (see modelpicker.go's TODO): pressing Enter must not change the rendered
// view — the real SetModel + config.Save behavior lands once the parallel
// plumbing does.
func TestModelEnterIsNoOpStub(t *testing.T) {
	auths := []ProviderAuth{{Provider: "anthropic", Kind: KindOAuth}}
	v := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), nil, "claude-sonnet-5")
	v = v.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})

	before := v.View(testkit.Width, testkit.Height)
	after := v.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	got := after.View(testkit.Width, testkit.Height)

	if got != before {
		t.Fatalf("expected Enter to be a no-op stub;\nbefore:\n%s\nafter:\n%s", before, got)
	}
}

// TestModelDescriptionLineFormat locks the description format (§4a): display
// name, raw id, context window, and per-Mtok pricing derived from
// provider.Lookup.
func TestModelDescriptionLineFormat(t *testing.T) {
	got := modelDescriptionLine("claude-sonnet-5")
	want := "Sonnet 5 (claude-sonnet-5) · 1M context · $3/$15 per Mtok"
	if got != want {
		t.Fatalf("modelDescriptionLine(%q) = %q, want %q", "claude-sonnet-5", got, want)
	}
}

// TestModelDisplayNameFallsBackToID covers a model id absent from
// modelDisplayNames (a newly registered SDK model gofer hasn't labeled yet):
// it falls back to the raw id rather than an empty name.
func TestModelDisplayNameFallsBackToID(t *testing.T) {
	if got := modelDisplayName("some-future-model"); got != "some-future-model" {
		t.Fatalf("modelDisplayName(unregistered) = %q, want the raw id", got)
	}
}
