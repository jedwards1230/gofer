package tui

// modelpicker_test.go covers the /model command-panel tab (modelpicker.go,
// M4 step 4): the catalog list filtered to authenticated providers, the
// active (✓) mark's precedence (session override > session.model config
// default > resolved roster default), the description-line formatting, the
// empty+warn state with zero providers authenticated (§4c), row-highlight
// navigation, the free-text model-id entry (typing/backspace/Esc and its
// precedence against a row highlight), the "never render unknown metadata as
// a zero value" guard both segments carry, and the auth-independence contract
// (a nil/erroring Auth or Config never blocks the view, and Enter is a no-op
// at this layer). White-box (package tui) because modelPickerView is unexported —
// the App-level "/model opens the panel" behavior is covered separately in
// command_test.go (package tui_test).

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

// TestModelEnterIsNoOpStub covers modelPickerView.handleKey's own Enter
// handling in isolation: it is deliberately a no-op at this pure-value
// layer — the real SetModel + config.Save coupling lives one level up in
// App.handleModelSelect (panel.go), which intercepts Enter before it ever
// reaches this method (see model_select_test.go for that end-to-end
// behavior).
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
// name, raw id, context window, and per-Mtok pricing derived from the
// registry.
func TestModelDescriptionLineFormat(t *testing.T) {
	got := modelDescriptionLine("claude-sonnet-5")
	want := "Sonnet 5 (claude-sonnet-5) · 1M context · $3/$15 per Mtok"
	if got != want {
		t.Fatalf("modelDescriptionLine(%q) = %q, want %q", "claude-sonnet-5", got, want)
	}
}

// typeModel feeds each rune of s through handleKey, mirroring how the app
// root delivers keystrokes one at a time (configView's typeText equivalent).
func typeModel(v modelPickerView, s string) modelPickerView {
	for _, r := range s {
		v = v.handleKey(tea.KeyPressMsg{Text: string(r)})
	}
	return v
}

// unregisteredID is a model id the compiled-in registry does not carry but
// whose backend provider.Resolve can still infer from its "gpt-" prefix —
// i.e. a model newer than this binary, the exact case the free-text entry
// exists for.
const unregisteredID = "gpt-5.6-sol"

// TestModelDescriptionLineUnregisteredShowsNoFabricatedMetadata is the
// regression guard for the highest-risk line in the free-text entry change:
// an unregistered record carries a zero ContextWindow and zero Pricing that
// mean "unknown", NOT "no context" and NOT "free"
// (provider.ModelInfo.Unregistered's doc is explicit that only ID and
// Provider are trustworthy). Rendering those zeroes would put a fabricated
// price and a fabricated limit on screen as fact.
func TestModelDescriptionLineUnregisteredShowsNoFabricatedMetadata(t *testing.T) {
	got := modelDescriptionLine(unregisteredID)

	for _, fabricated := range []string{"$0", "0 context", "per Mtok"} {
		if strings.Contains(got, fabricated) {
			t.Fatalf("modelDescriptionLine(%q) = %q; must not present %q as fact for an unregistered model",
				unregisteredID, got, fabricated)
		}
	}
	for _, want := range []string{unregisteredID, "context unknown", "pricing unknown"} {
		if !strings.Contains(got, want) {
			t.Fatalf("modelDescriptionLine(%q) = %q, want it to contain %q", unregisteredID, got, want)
		}
	}
}

// TestModelDescriptionLineUnroutableID covers an id no provider family
// matches: nothing about it is knowable, so it renders as the bare display
// name with no metadata segments at all.
func TestModelDescriptionLineUnroutableID(t *testing.T) {
	got := modelDescriptionLine("not-a-real-family")
	want := "not-a-real-family (not-a-real-family)"
	if got != want {
		t.Fatalf("modelDescriptionLine(unroutable) = %q, want %q", got, want)
	}
}

// TestModelMetadataSegmentsGuardZeroValues covers the second half of the
// guard: a REGISTERED but incomplete record must not fabricate either segment
// from a zero field. Registry rows are complete today, so this is the test
// that keeps the guard honest if one ever isn't — the flag alone is not
// enough.
func TestModelMetadataSegmentsGuardZeroValues(t *testing.T) {
	full := provider.Pricing{Input: 3, Output: 15}

	contextCases := []struct {
		name string
		info provider.ModelInfo
		want string
	}{
		{"registered known", provider.ModelInfo{ContextWindow: 200_000, Pricing: full}, "200K context"},
		{"registered zero window", provider.ModelInfo{ContextWindow: 0, Pricing: full}, "context unknown"},
		{"unregistered", provider.ModelInfo{ContextWindow: 0, Unregistered: true}, "context unknown"},
	}
	for _, tc := range contextCases {
		t.Run("context/"+tc.name, func(t *testing.T) {
			if got := contextSegment(tc.info); got != tc.want {
				t.Fatalf("contextSegment(%+v) = %q, want %q", tc.info, got, tc.want)
			}
		})
	}

	pricingCases := []struct {
		name string
		info provider.ModelInfo
		want string
	}{
		{"registered known", provider.ModelInfo{ContextWindow: 200_000, Pricing: full}, "$3/$15 per Mtok"},
		{"registered zero input", provider.ModelInfo{ContextWindow: 200_000, Pricing: provider.Pricing{Output: 15}}, "pricing unknown"},
		{"registered zero output", provider.ModelInfo{ContextWindow: 200_000, Pricing: provider.Pricing{Input: 3}}, "pricing unknown"},
		{"unregistered", provider.ModelInfo{Unregistered: true}, "pricing unknown"},
	}
	for _, tc := range pricingCases {
		t.Run("pricing/"+tc.name, func(t *testing.T) {
			if got := pricingSegment(tc.info); got != tc.want {
				t.Fatalf("pricingSegment(%+v) = %q, want %q", tc.info, got, tc.want)
			}
		})
	}
}

// TestModelTypedEntryCommitsUnregisteredID is the free-text escape hatch's
// core contract: an id the compiled-in catalog doesn't list is still
// selectable, because provider.Resolve — not registry membership — decides
// what can run.
func TestModelTypedEntryCommitsUnregisteredID(t *testing.T) {
	auths := []ProviderAuth{{Provider: "openai", Kind: KindAPIKey}}
	v := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), nil, "gpt-5")

	v = typeModel(v, unregisteredID)

	if got := v.selectedModel(); got != unregisteredID {
		t.Fatalf("selectedModel() = %q, want the typed id %q", got, unregisteredID)
	}
	if got := v.View(testkit.Width, testkit.Height); !strings.Contains(got, unregisteredID) {
		t.Fatalf("expected the typed id to render as the commit candidate, got:\n%s", got)
	}
}

// TestModelEmptyEntryNeverCommits covers both empty states: nothing typed at
// all, and a whitespace-only buffer. Neither is a model id, so neither may
// reach App.handleModelSelect as a selection.
func TestModelEmptyEntryNeverCommits(t *testing.T) {
	auths := []ProviderAuth{{Provider: "anthropic", Kind: KindOAuth}}
	base := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), nil, "claude-sonnet-5")

	if got := base.selectedModel(); got != "" {
		t.Fatalf("selectedModel() with nothing typed = %q, want \"\"", got)
	}
	if got := typeModel(base, "   ").selectedModel(); got != "" {
		t.Fatalf("selectedModel() with a whitespace-only entry = %q, want \"\"", got)
	}
}

// TestModelUnroutableEntryDoesNotCommit covers the one id the entry refuses:
// no provider family matches it, so there is no adapter to run it and
// committing it would only persist a default that fails at the next run. The
// view says so rather than failing silently.
func TestModelUnroutableEntryDoesNotCommit(t *testing.T) {
	auths := []ProviderAuth{{Provider: "anthropic", Kind: KindOAuth}}
	v := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), nil, "claude-sonnet-5")

	v = typeModel(v, "not-a-real-family")

	if got := v.selectedModel(); got != "" {
		t.Fatalf("selectedModel() for an unroutable id = %q, want \"\" (nothing committable)", got)
	}
	if got := v.View(testkit.Width, testkit.Height); !strings.Contains(got, "cannot determine which provider") {
		t.Fatalf("expected the view to explain why the typed id is not committable, got:\n%s", got)
	}
}

// TestModelTypedEntryOutranksStaleHighlight covers the two-way precedence
// between the row list and the entry: typing drops an earlier row highlight
// (the typed id wins), and moving back onto a row afterwards makes the row
// win again.
func TestModelTypedEntryOutranksStaleHighlight(t *testing.T) {
	auths := []ProviderAuth{{Provider: "anthropic", Kind: KindOAuth}}
	v := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), nil, "claude-sonnet-5")

	v = v.handleKey(tea.KeyPressMsg{Code: tea.KeyDown}) // highlight row 0
	rowID := v.selectedModel()
	if rowID == "" {
		t.Fatal("expected a highlighted row to be selectable")
	}

	v = typeModel(v, unregisteredID)
	if got := v.selectedModel(); got != unregisteredID {
		t.Fatalf("selectedModel() after typing = %q, want the typed id %q (the highlight is stale)", got, unregisteredID)
	}

	v = v.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	if got := v.selectedModel(); got != rowID {
		t.Fatalf("selectedModel() after moving back onto a row = %q, want the row id %q", got, rowID)
	}
}

// TestModelBackspaceEditsEntry covers the entry's only editing key
// (←/→ are claimed by the panel host for tab switching, so there is no
// mid-text cursor to move): Backspace drops the last rune, and is a no-op on
// an empty buffer.
func TestModelBackspaceEditsEntry(t *testing.T) {
	auths := []ProviderAuth{{Provider: "openai", Kind: KindAPIKey}}
	v := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), nil, "gpt-5")

	v = typeModel(v, unregisteredID+"x")
	v = v.handleKey(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if got := v.selectedModel(); got != unregisteredID {
		t.Fatalf("selectedModel() after backspace = %q, want %q", got, unregisteredID)
	}

	empty := v
	for range len(unregisteredID) + 3 { // more presses than there are runes
		empty = empty.handleKey(tea.KeyPressMsg{Code: tea.KeyBackspace})
	}
	if got := empty.selectedModel(); got != "" {
		t.Fatalf("selectedModel() after clearing the entry = %q, want \"\"", got)
	}
}

// TestModelEscapeClearsEntryThenBubbles covers the two-stage Esc contract
// (configView.handleEscape's, mirrored): the first Esc discards a half-typed
// id and keeps the panel open, and a second one bubbles up to close it.
func TestModelEscapeClearsEntryThenBubbles(t *testing.T) {
	auths := []ProviderAuth{{Provider: "openai", Kind: KindAPIKey}}
	v := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), nil, "gpt-5")
	v = typeModel(v, unregisteredID)

	v, consumed := v.handleEscape()
	if !consumed {
		t.Fatal("expected the first Esc to be consumed clearing the typed entry, not to close the panel")
	}
	if got := v.selectedModel(); got != "" {
		t.Fatalf("expected Esc to clear the entry, selectedModel() = %q", got)
	}

	if _, consumed = v.handleEscape(); consumed {
		t.Fatal("expected a second Esc to bubble up and close the panel")
	}
}

// TestGoldenModelTypedUnregisteredID is the rendered counterpart to
// TestModelDescriptionLineUnregisteredShowsNoFabricatedMetadata: the typed
// candidate line sits above the compiled-in list, reporting its context
// window and pricing as unknown rather than as zero.
func TestGoldenModelTypedUnregisteredID(t *testing.T) {
	auths := []ProviderAuth{{Provider: "openai", Kind: KindAPIKey}}
	v := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), nil, "gpt-5")
	renderModel(t, "model_typed_unregistered", typeModel(v, unregisteredID))
}

// TestGoldenModelTypedUnregisteredIDStyled is its color-state counterpart:
// the typed candidate carries AccentStyle — the same "this is the one Enter
// acts on" signal a highlighted row carries — which the Ascii golden above
// cannot assert.
func TestGoldenModelTypedUnregisteredIDStyled(t *testing.T) {
	auths := []ProviderAuth{{Provider: "openai", Kind: KindAPIKey}}
	v := newModelPickerView(testkit.ColorTheme(), modelTestEnv(auths, nil), nil, "gpt-5")
	renderModelStyled(t, "model_typed_unregistered", typeModel(v, unregisteredID))
}

// TestGoldenModelTypedUnroutableID covers the refused-id render: the SDK's
// own reason replaces the candidate line, so "Enter does nothing" is
// explained before Enter is pressed.
func TestGoldenModelTypedUnroutableID(t *testing.T) {
	auths := []ProviderAuth{{Provider: "openai", Kind: KindAPIKey}}
	v := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), nil, "gpt-5")
	renderModel(t, "model_typed_unroutable", typeModel(v, "not-a-real-family"))
}

// TestGoldenModelTypedUnroutableIDStyled is its color-state counterpart: the
// refusal renders in WarnStyle rather than as an ordinary candidate line —
// the distinction between "this is committable" and "this is not" is carried
// by color, so only a colored render can assert it.
func TestGoldenModelTypedUnroutableIDStyled(t *testing.T) {
	auths := []ProviderAuth{{Provider: "openai", Kind: KindAPIKey}}
	v := newModelPickerView(testkit.ColorTheme(), modelTestEnv(auths, nil), nil, "gpt-5")
	renderModelStyled(t, "model_typed_unroutable", typeModel(v, "not-a-real-family"))
}
