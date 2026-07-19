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
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/modelcatalog"
	"github.com/jedwards1230/gofer/internal/modelmeta"
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
//
// It must also be absent from [modelmeta]'s display-name table, since these
// tests assert the raw id stands in for a name gofer has no label for. That is
// a second, easily-missed condition: this fixture was previously a real Codex
// id, which stopped satisfying it the moment gofer learned a label for that
// family. TestUnregisteredIDHasNoDisplayName guards it.
const unregisteredID = "gpt-9-unreleased"

// TestUnregisteredIDHasNoDisplayName locks the second half of unregisteredID's
// contract (see its doc): every test using it expects modelDescriptionLine to
// fall back to the raw id, which silently stops being true if the fixture ever
// gains a modelmeta label. Without this guard that failure surfaces as a
// confusing golden diff in an unrelated change.
func TestUnregisteredIDHasNoDisplayName(t *testing.T) {
	if got := modelmeta.DisplayName(unregisteredID); got != unregisteredID {
		t.Fatalf("modelmeta.DisplayName(%q) = %q; the picker fixture must have no display name — pick an id modelmeta does not label", unregisteredID, got)
	}
}

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

// TestModelRowsFollowCredentialKind is the feature at the heart of issue #156:
// the list must be what THIS credential can reach, not what the provider sells
// in general. OpenAI routes an OAuth (subscription) credential to a different
// backend than an API key, and the two serve different model families — the
// SDK registry only carries the API-key one. Listing the registry to an OAuth
// user offered ids that backend rejects outright while hiding every id it does
// serve, which is why switching between two Codex models meant typing raw ids.
//
// Both directions are asserted, because "show the OAuth family" is only half a
// fix — regressing the API-key user to the Codex family would be the identical
// bug pointed the other way.
func TestModelRowsFollowCredentialKind(t *testing.T) {
	t.Run("oauth lists the codex family, not the api-key family", func(t *testing.T) {
		auths := []ProviderAuth{{Provider: "openai", Kind: KindOAuth}}
		v := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), nil, "")

		var got []string
		for _, r := range v.rows() {
			got = append(got, r.id)
		}
		want := []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.3-codex-spark"}
		if !slices.Equal(got, want) {
			t.Fatalf("rows() for an OAuth openai credential = %v, want the Codex family %v", got, want)
		}
		// gpt-5 is precisely the id that backend answers with HTTP 400.
		if slices.Contains(got, "gpt-5") {
			t.Errorf("rows() = %v; must not offer gpt-5 to an OAuth credential — the Codex backend rejects it", got)
		}
	})

	t.Run("api key keeps the registry family", func(t *testing.T) {
		auths := []ProviderAuth{{Provider: "openai", Kind: KindAPIKey}}
		v := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), nil, "")

		var got []string
		for _, r := range v.rows() {
			got = append(got, r.id)
		}
		if !slices.Contains(got, "gpt-5") {
			t.Errorf("rows() for an API-key openai credential = %v, want it to still carry gpt-5", got)
		}
		for _, codexOnly := range []string{"gpt-5.6-sol", "gpt-5.6-terra"} {
			if slices.Contains(got, codexOnly) {
				t.Errorf("rows() = %v; an API-key credential must not be offered the OAuth-only id %q", got, codexOnly)
			}
		}
	})
}

// TestModelRowsRenderCodexFamilyForOAuth is the rendered counterpart: the
// user's stated want is picking between sol and terra from the list instead of
// typing ids, so the display names have to be on screen, not just the ids in
// rows().
func TestModelRowsRenderCodexFamilyForOAuth(t *testing.T) {
	auths := []ProviderAuth{{Provider: "openai", Kind: KindOAuth}}
	v := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), nil, "gpt-5.6-terra")

	got := v.View(testkit.Width, testkit.Height)
	for _, want := range []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected the picker to list %q for an OAuth credential, got:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "✓ "+modelmeta.DisplayName("gpt-5.6-terra")) {
		t.Fatalf("expected the active mark on the resolved default, got:\n%s", got)
	}
}

// TestModelRowsAreNotAnAdmissionGate is the non-gate proof at this layer. The
// per-kind catalog decides what is LISTED; it must never decide what is
// RUNNABLE. The sharpest case is an id the kind-aware list deliberately omits:
// gpt-5 is absent from an OAuth user's rows precisely because that backend
// rejects it — and yet typing it must still commit, because provider.Resolve,
// not this list, is the admission decision, and the list is only ever as
// correct as the day this binary was built.
func TestModelRowsAreNotAnAdmissionGate(t *testing.T) {
	auths := []ProviderAuth{{Provider: "openai", Kind: KindOAuth}}
	v := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), nil, "")

	var listed []string
	for _, r := range v.rows() {
		listed = append(listed, r.id)
	}
	for _, unlisted := range []string{"gpt-5", unregisteredID} {
		if slices.Contains(listed, unlisted) {
			t.Fatalf("test premise broken: %q is listed for an OAuth credential (%v)", unlisted, listed)
		}
		if got := typeModel(v, unlisted).selectedModel(); got != unlisted {
			t.Errorf("selectedModel() after typing the unlisted id %q = %q; the catalog must not gate what can be committed", unlisted, got)
		}
	}
}

// TestModelRowsMixedKindsGroupPerCredential covers the two-provider ceiling
// with DIFFERENT credential kinds at once — the arrangement that catches a
// per-provider (rather than per-credential) implementation, which would have to
// pick one kind for the whole list.
func TestModelRowsMixedKindsGroupPerCredential(t *testing.T) {
	auths := []ProviderAuth{
		{Provider: "openai", Kind: KindOAuth},
		{Provider: "anthropic", Kind: KindOAuth},
	}
	v := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), nil, "")

	rows := v.rows()
	var providers []string
	for _, r := range rows {
		if len(providers) == 0 || providers[len(providers)-1] != r.provider {
			providers = append(providers, r.provider)
		}
	}
	if !slices.Equal(providers, []string{"anthropic", "openai"}) {
		t.Fatalf("row provider blocks = %v, want one ascending block per provider", providers)
	}

	var ids []string
	for _, r := range rows {
		ids = append(ids, r.id)
	}
	// Anthropic serves one family on both kinds, so it keeps the registry;
	// openai's OAuth credential must still get the Codex family beside it.
	if !slices.Contains(ids, "claude-sonnet-5") {
		t.Errorf("rows() = %v, want the anthropic registry family unaffected", ids)
	}
	if !slices.Contains(ids, "gpt-5.6-terra") {
		t.Errorf("rows() = %v, want openai's OAuth credential to still list the Codex family", ids)
	}
}

// TestModelRowsDeduplicateProviders guards the one input this view does not
// control: env.Auth is a file read, and a duplicated provider entry would
// otherwise double that provider's whole block, silently misaligning every
// cursor index below it.
func TestModelRowsDeduplicateProviders(t *testing.T) {
	auths := []ProviderAuth{
		{Provider: "openai", Kind: KindOAuth},
		{Provider: "openai", Kind: KindOAuth},
	}
	v := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), nil, "")

	single := newModelPickerView(theme.Test(), modelTestEnv(auths[:1], nil), nil, "")
	if got, want := len(v.rows()), len(single.rows()); got != want {
		t.Fatalf("rows() with a duplicated provider = %d rows, want %d", got, want)
	}
}

// TestAuthKindMirrorsModelcatalogKind locks the one unchecked conversion
// rows() performs. [AuthKind] is a local mirror of the SDK's auth.CredKind
// (see env.go) and [modelcatalog.Kind] is a second, independent mirror of the
// same thing, so rows() crossing between them with a plain string conversion is
// only correct while the two agree. A rename on either side would compile
// fine and silently downgrade every OAuth user back to the API-key catalog —
// which is the original bug, restored, with no test failing anywhere else.
func TestAuthKindMirrorsModelcatalogKind(t *testing.T) {
	if got, want := string(KindOAuth), string(modelcatalog.KindOAuth); got != want {
		t.Errorf("KindOAuth = %q, want modelcatalog.KindOAuth (%q)", got, want)
	}
	if got, want := string(KindAPIKey), string(modelcatalog.KindAPIKey); got != want {
		t.Errorf("KindAPIKey = %q, want modelcatalog.KindAPIKey (%q)", got, want)
	}
}

// liveCodexCatalog is a discovery result standing in for what the Codex
// listing returns: ids absent from every compiled-in list, a vendor display
// name that disagrees with modelmeta, and real context windows. Nothing here
// can appear in a rendered picker unless a live load actually landed.
func liveCodexCatalog() []modelcatalog.Model {
	return []modelcatalog.Model{
		{ID: "gpt-5.9-nova", Provider: "openai", Label: "GPT-5.9 Nova", ContextWindow: 512_000},
		{ID: "gpt-5.6-terra", Provider: "openai", Label: "Terra (live name)", ContextWindow: 272_000},
	}
}

// TestModelPickerOpensOnFloorBeforeDiscovery is the anti-freeze contract. The
// picker must be complete and usable on its FIRST frame, before any live load
// resolves — discovery is bounded at 3s, and a blocking resolve would stall the
// panel for exactly as long as the vendor is slow.
//
// env.Models here never returns at all. If the view ever waits on it, this test
// hangs rather than fails, which is the honest signal for "the render path
// blocks on the network".
func TestModelPickerOpensOnFloorBeforeDiscovery(t *testing.T) {
	env := modelTestEnv([]ProviderAuth{{Provider: "openai", Kind: KindOAuth}}, nil)
	env.Models = func(ctx context.Context, _ string) ([]modelcatalog.Model, error) {
		<-ctx.Done() // never resolves on its own
		return nil, ctx.Err()
	}

	v := newModelPickerView(theme.Test(), env, nil, "")

	got := v.View(testkit.Width, testkit.Height)
	if !strings.Contains(got, "gpt-5.6-terra") {
		t.Fatalf("expected the offline Codex floor on the first frame, got:\n%s", got)
	}
	if len(v.rows()) == 0 {
		t.Fatal("expected a usable row list before discovery resolves")
	}
	if v.live {
		t.Error("expected live=false before any load landed")
	}
}

// TestModelPickerNoIOOnTheRenderPath is the cache's reason for existing. rows()
// is read several times per keystroke, so resolving the catalog there would
// issue a vendor request per keypress. The catalog closure must be called ZERO
// times by rendering, navigating, and typing — the panel host calls it once,
// off the Update loop.
func TestModelPickerNoIOOnTheRenderPath(t *testing.T) {
	var modelCalls, authCalls int
	env := GoldenCommandEnv()
	env.Auth = func() ([]ProviderAuth, error) {
		authCalls++
		return []ProviderAuth{{Provider: "openai", Kind: KindOAuth}}, nil
	}
	env.Models = func(context.Context, string) ([]modelcatalog.Model, error) {
		modelCalls++
		return liveCodexCatalog(), nil
	}

	v := newModelPickerView(theme.Test(), env, nil, "")
	seedAuthCalls := authCalls

	for range 5 {
		v = v.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
		_ = v.View(testkit.Width, testkit.Height)
	}
	v = typeModel(v, "gpt-5.6-luna")
	_ = v.View(testkit.Width, testkit.Height)
	_ = v.selectedModel()

	if modelCalls != 0 {
		t.Errorf("env.Models called %d times from the render path, want 0 — the catalog must be resolved once by the panel host, not per keystroke", modelCalls)
	}
	if got := authCalls - seedAuthCalls; got != 0 {
		t.Errorf("env.Auth called %d times from the render path, want 0 — the row list must read the cached catalog", got)
	}
}

// TestModelPickerWithCatalogUpgradesRows covers the load landing: the live list
// replaces the floor wholesale, and the row highlight is dropped because it was
// an index into the OLD list and the two need not agree in length or order.
func TestModelPickerWithCatalogUpgradesRows(t *testing.T) {
	auths := []ProviderAuth{{Provider: "openai", Kind: KindOAuth}}
	v := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), nil, "")
	v = v.handleKey(tea.KeyPressMsg{Code: tea.KeyDown}) // highlight a floor row
	if v.cursor != 0 {
		t.Fatalf("test premise broken: expected a highlighted floor row, cursor=%d", v.cursor)
	}

	v = v.withCatalog(liveCodexCatalog())

	var got []string
	for _, r := range v.rows() {
		got = append(got, r.id)
	}
	if !slices.Equal(got, []string{"gpt-5.9-nova", "gpt-5.6-terra"}) {
		t.Fatalf("rows() after the load = %v, want the live listing", got)
	}
	if !v.live {
		t.Error("expected live=true after a load landed")
	}
	// The highlighted floor row (gpt-5.6-sol) is absent from the live listing,
	// so it has genuinely gone away and the highlight drops to none rather
	// than sliding onto an unrelated neighbor at the same index.
	if v.cursor != -1 {
		t.Errorf("cursor = %d after the highlighted model vanished from the list, want -1", v.cursor)
	}
}

// TestModelPickerLiveCarriesHighlightByID covers withCatalog's other branch,
// and the one a naive implementation gets wrong. The cursor is an INDEX, but
// what the user means by it is a MODEL. A background load landing a second
// after the panel opened must not yank the selection somewhere else, so a
// highlighted model still present in the live listing keeps the highlight —
// at whatever index it now occupies, which need not be the old one.
func TestModelPickerLiveCarriesHighlightByID(t *testing.T) {
	auths := []ProviderAuth{{Provider: "openai", Kind: KindOAuth}}
	v := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), nil, "")

	// Walk onto gpt-5.6-terra in the floor, which sits at a DIFFERENT index in
	// the live listing (floor index 1, live index 1... so move to a floor row
	// whose live index differs to make the test meaningful).
	var floorIdx int
	for i, r := range v.rows() {
		if r.id == "gpt-5.6-terra" {
			floorIdx = i
			break
		}
	}
	for range floorIdx + 1 {
		v = v.handleKey(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	if got := v.rows()[v.cursor].id; got != "gpt-5.6-terra" {
		t.Fatalf("test premise broken: highlighted %q, want gpt-5.6-terra", got)
	}

	v = v.withCatalog(liveCodexCatalog())

	if v.cursor < 0 {
		t.Fatal("expected the highlight to survive a load that still carries the highlighted model")
	}
	if got := v.rows()[v.cursor].id; got != "gpt-5.6-terra" {
		t.Fatalf("highlighted %q after the load, want the same model gpt-5.6-terra the user was on", got)
	}
	if got := v.selectedModel(); got != "gpt-5.6-terra" {
		t.Fatalf("selectedModel() = %q after the load, want gpt-5.6-terra", got)
	}
}

// TestModelPickerLivePreservesTypedEntry is withCatalog's other half: the row
// highlight is a position in data the load replaced, but a typed id is text the
// user wrote. A background refresh must not eat it mid-keystroke.
func TestModelPickerLivePreservesTypedEntry(t *testing.T) {
	auths := []ProviderAuth{{Provider: "openai", Kind: KindOAuth}}
	v := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), nil, "")
	v = typeModel(v, "gpt-5.6-luna")

	v = v.withCatalog(liveCodexCatalog())

	if got := v.selectedModel(); got != "gpt-5.6-luna" {
		t.Fatalf("selectedModel() after a background load = %q, want the typed id to survive", got)
	}
}

// TestModelPickerEmptyCatalogKeepsFloor covers the failure path the picker
// depends on. modelcatalog.Catalog degrades every discovery failure to the
// floor internally, so an EMPTY result means the caller had nothing at all (a
// nil closure, a broken auth.json). Adopting it would blank a working picker —
// the exact outcome the floor exists to prevent.
func TestModelPickerEmptyCatalogKeepsFloor(t *testing.T) {
	auths := []ProviderAuth{{Provider: "openai", Kind: KindOAuth}}
	v := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), nil, "")
	before := len(v.rows())
	if before == 0 {
		t.Fatal("test premise broken: expected a non-empty floor")
	}

	v = v.withCatalog(nil)

	if got := len(v.rows()); got != before {
		t.Fatalf("rows() after an empty load = %d, want the floor's %d rows kept", got, before)
	}
	if v.live {
		t.Error("expected live=false — an empty result is not a completed load")
	}
}

// TestModelPickerRendersLiveMetadata covers requirement 4: a discovered model
// shows the VENDOR's display name and context window, since believing the live
// listing is the point of fetching it. Pricing stays unknown — modelcatalog.Model
// carries no pricing field because the listing carries none, and a subscription
// model has no per-token price to quote.
func TestModelPickerRendersLiveMetadata(t *testing.T) {
	auths := []ProviderAuth{{Provider: "openai", Kind: KindOAuth}}
	v := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), nil, "")
	v = v.withCatalog(liveCodexCatalog())

	got := v.View(testkit.Width, testkit.Height)

	if !strings.Contains(got, "GPT-5.9 Nova (gpt-5.9-nova)") {
		t.Errorf("expected the live display name, got:\n%s", got)
	}
	if !strings.Contains(got, "512K context") {
		t.Errorf("expected the live context window, got:\n%s", got)
	}
	// The compiled-in label for this id must lose to the vendor's.
	if strings.Contains(got, modelmeta.DisplayName("gpt-5.6-terra")+" (gpt-5.6-terra)") {
		t.Errorf("expected the live display_name to beat gofer's compiled-in label, got:\n%s", got)
	}
	if !strings.Contains(got, "Terra (live name)") {
		t.Errorf("expected the live label for gpt-5.6-terra, got:\n%s", got)
	}
}

// TestModelPickerNeverRendersFabricatedPricing is the standing guard, extended
// to discovered models. A live entry has a real context window and NO price, so
// the price must read "unknown" — never "$0", which would state as fact that a
// model the user pays a subscription for is free.
func TestModelPickerNeverRendersFabricatedPricing(t *testing.T) {
	auths := []ProviderAuth{{Provider: "openai", Kind: KindOAuth}}
	v := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), nil, "")
	v = v.withCatalog(liveCodexCatalog())

	for _, line := range strings.Split(v.View(testkit.Width, testkit.Height), "\n") {
		if !strings.Contains(line, "gpt-5.9-nova") && !strings.Contains(line, "gpt-5.6-terra") {
			continue
		}
		if !strings.Contains(line, "pricing unknown") {
			t.Errorf("discovered model line %q must report pricing as unknown", line)
		}
		for _, fabricated := range []string{"$0", "per Mtok"} {
			if strings.Contains(line, fabricated) {
				t.Errorf("discovered model line %q must not present %q as fact", line, fabricated)
			}
		}
	}
}

// TestModelPickerLiveCatalogIsStillNotAnAdmissionGate carries the non-gate
// property across the upgrade. A live list is a better list, not a permission
// list — an id absent from it must still commit, because provider.Resolve makes
// that decision and the vendor's picker inventory is not the same question as
// what the backend will run.
func TestModelPickerLiveCatalogIsStillNotAnAdmissionGate(t *testing.T) {
	auths := []ProviderAuth{{Provider: "openai", Kind: KindOAuth}}
	v := newModelPickerView(theme.Test(), modelTestEnv(auths, nil), nil, "")
	v = v.withCatalog(liveCodexCatalog())

	var listed []string
	for _, r := range v.rows() {
		listed = append(listed, r.id)
	}
	if slices.Contains(listed, unregisteredID) {
		t.Fatalf("test premise broken: %q is in the live list %v", unregisteredID, listed)
	}
	if got := typeModel(v, unregisteredID).selectedModel(); got != unregisteredID {
		t.Errorf("selectedModel() for an id absent from the live list = %q, want %q — a live catalog must not become a gate", got, unregisteredID)
	}
}
