package tui_test

// model_select_test.go covers the /model picker's coupled Enter/select
// action — App.handleModelSelect (panel.go), M4 step 4's final piece: the
// session.model config default is always persisted, and a running session's
// live model is swapped via Supervisor.SetModel only when the selected model
// shares the attached/peeked session's provider (decided client-side against
// the SDK's static catalog); a cross-provider pick leaves the running
// session on its current model and only the status note explains why.
// Exercised entirely through App's exported Update/View surface, reusing
// app_test.go's fakeSup/press/content/type_ helpers and command_test.go's
// dispatchSlash.

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// modelSelectRoster mirrors GoldenRoster's first (working, most-recently
// active — selected by default) session but pins its Model to
// claude-sonnet-5, so the coupled-select tests below have a real provider to
// compare the picked model against.
func modelSelectRoster() []tui.SessionInfo {
	roster := tui.GoldenRoster()
	roster[0].Model = "claude-sonnet-5"
	return roster
}

// modelSelectEnv returns a CommandEnv with GoldenCommandEnv's identity
// fields, both providers reported authenticated (so the picker lists both
// claude-* and gpt-* rows), and SaveConfig recording into saved.
func modelSelectEnv(saved *[]config.Config) tui.CommandEnv {
	env := tui.GoldenCommandEnv()
	env.Auth = func() ([]tui.ProviderAuth, error) {
		return []tui.ProviderAuth{
			{Provider: "anthropic", Kind: tui.KindOAuth},
			{Provider: "openai", Kind: tui.KindAPIKey},
		}, nil
	}
	env.SaveConfig = func(c config.Config) error {
		*saved = append(*saved, c)
		return nil
	}
	return env
}

// newModelSelectApp builds an App over sup/env through theme.Test(), sized
// and with the first roster fetch resolved — the same construction
// TestPanelConfigEditPersists (command_test.go) uses when a test needs a
// custom CommandEnv rather than newTestApp's GoldenCommandEnv.
func newModelSelectApp(t *testing.T, sup tui.Supervisor, env tui.CommandEnv) tea.Model {
	t.Helper()
	return newModelSelectAppWithTheme(t, theme.Test(), sup, env)
}

// newModelSelectAppWithTheme is [newModelSelectApp] with the rendering theme
// parameterized, for the styled-golden layer ([testkit.ColorTheme]).
func newModelSelectAppWithTheme(t *testing.T, th theme.Theme, sup tui.Supervisor, env tui.CommandEnv) tea.Model {
	t.Helper()
	var m tea.Model = tui.NewApp(th, sup, tui.GoldenMeta(), env)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m, _ = m.Update(m.Init()())
	return m
}

// Both providers authenticated sorts the catalog to: claude-fable-5(0),
// claude-haiku-4-5(1), claude-opus-4-8(2), claude-sonnet-5(3), gpt-5(4),
// gpt-5-mini(5), gpt-5-nano(6), o4-mini(7) — see modelpicker.go's rows()
// (provider, then id, ascending). The session's model is pinned to
// claude-sonnet-5 (anthropic). selectDown's first ↓ lands on row 0 (from no
// highlight), so N ↓ presses highlights row N-1 — these constants are press
// counts, not row indices.
const (
	pressesToHaiku = 2 // ↓↓ highlights claude-haiku-4-5 (row 1): same provider, different model
	pressesToGPT5  = 5 // ↓×5 highlights gpt-5 (row 4): a different provider than the session
)

// pressDown presses ↓ n times.
func pressDown(t *testing.T, m tea.Model, n int) tea.Model {
	t.Helper()
	for i := 0; i < n; i++ {
		m = press(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	}
	return m
}

// TestModelSelectAttachedSameProviderHotSwaps verifies selecting a
// same-provider model on an attached session both persists the session.model
// default and hot-swaps the running session via Supervisor.SetModel.
func TestModelSelectAttachedSameProviderHotSwaps(t *testing.T) {
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach the selected (sonnet) session
	m = dispatchSlash(t, m, "/model")
	m = pressDown(t, m, pressesToHaiku)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if len(saved) != 1 || saved[0].Session.Model != "claude-haiku-4-5" {
		t.Fatalf("SaveConfig calls = %v; want one entry with Session.Model claude-haiku-4-5", saved)
	}
	wantOp := "set-model:0192a1b2-app0-7000-8000-000000000001:claude-haiku-4-5"
	if len(sup.ops) != 1 || sup.ops[0] != wantOp {
		t.Fatalf("sup.ops = %v; want one entry %q", sup.ops, wantOp)
	}
	got := content(m)
	if strings.Contains(got, "[Model]") {
		t.Fatalf("expected the panel to close after a committed select, got:\n%s", got)
	}
	if !strings.Contains(got, "Model set to Haiku 4.5") {
		t.Fatalf("expected the hot-swap status note, got:\n%s", got)
	}
}

// TestModelSelectAttachedCrossProviderWarnsOnly verifies selecting a
// cross-provider model on an attached session persists the session.model
// default but does NOT call Supervisor.SetModel — the running session keeps
// its model, and the status note says why.
func TestModelSelectAttachedCrossProviderWarnsOnly(t *testing.T) {
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach the selected (sonnet) session
	m = dispatchSlash(t, m, "/model")
	m = pressDown(t, m, pressesToGPT5)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if len(saved) != 1 || saved[0].Session.Model != "gpt-5" {
		t.Fatalf("SaveConfig calls = %v; want one entry with Session.Model gpt-5", saved)
	}
	if len(sup.ops) != 0 {
		t.Fatalf("sup.ops = %v; want none — a cross-provider select must not call SetModel", sup.ops)
	}
	got := content(m)
	if !strings.Contains(got, "Live model swap needs the same provider") {
		t.Fatalf("expected the cross-provider warning status note, got:\n%s", got)
	}
}

// TestModelSelectOverviewSetsDefaultOnly verifies selecting a model with no
// attached/peeked session (the overview) persists only the session.model
// default and never calls Supervisor.SetModel.
func TestModelSelectOverviewSetsDefaultOnly(t *testing.T) {
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

	m = dispatchSlash(t, m, "/model")
	m = pressDown(t, m, pressesToHaiku)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if len(saved) != 1 || saved[0].Session.Model != "claude-haiku-4-5" {
		t.Fatalf("SaveConfig calls = %v; want one entry with Session.Model claude-haiku-4-5", saved)
	}
	if len(sup.ops) != 0 {
		t.Fatalf("sup.ops = %v; want none — no active session to swap", sup.ops)
	}
	got := content(m)
	if !strings.Contains(got, "Default model set to Haiku 4.5") {
		t.Fatalf("expected the overview default-set status note, got:\n%s", got)
	}
}

// TestModelSelectNoRowHighlightedIsNoOp verifies pressing Enter before any
// ↓/↑ (no row highlighted) is a pure no-op: nothing saved, no status note,
// the panel stays open.
func TestModelSelectNoRowHighlightedIsNoOp(t *testing.T) {
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

	m = dispatchSlash(t, m, "/model")
	before := content(m)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if got := content(m); got != before {
		t.Fatalf("expected Enter with no row highlighted to be a no-op;\ngot:\n%s\nwant:\n%s", content(m), before)
	}
	if len(saved) != 0 {
		t.Fatalf("SaveConfig calls = %v; want none", saved)
	}
	if len(sup.ops) != 0 {
		t.Fatalf("sup.ops = %v; want none", sup.ops)
	}
}

// TestModelSelectSaveConfigErrorStopsBeforeSetModel verifies a SaveConfig
// failure surfaces as the status note and short-circuits before any
// Supervisor.SetModel call, even on an attached same-provider pick.
func TestModelSelectSaveConfigErrorStopsBeforeSetModel(t *testing.T) {
	sup := newFakeSup(modelSelectRoster())
	env := tui.GoldenCommandEnv()
	env.Auth = func() ([]tui.ProviderAuth, error) {
		return []tui.ProviderAuth{{Provider: "anthropic", Kind: tui.KindOAuth}}, nil
	}
	env.SaveConfig = func(config.Config) error { return errors.New("disk full") }

	m := newModelSelectApp(t, sup, env)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach the selected (sonnet) session
	m = dispatchSlash(t, m, "/model")
	m = pressDown(t, m, pressesToHaiku) // claude-haiku-4-5: anthropic-only list, row 1 = haiku
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if len(sup.ops) != 0 {
		t.Fatalf("sup.ops = %v; want none — a SaveConfig error must stop before SetModel", sup.ops)
	}
	got := content(m)
	if !strings.Contains(got, "couldn't save default model") {
		t.Fatalf("expected the SaveConfig error status note, got:\n%s", got)
	}
}

// TestModelSelectConfigReadErrorAbortsBeforeSave guards against silent data
// loss: when the config read fails, handleModelSelect must NOT save a
// zero-value config over config.json (which would drop the user's
// permissions/telemetry) — it surfaces the error and aborts before any
// SaveConfig or SetModel call, preserving the on-disk state.
func TestModelSelectConfigReadErrorAbortsBeforeSave(t *testing.T) {
	sup := newFakeSup(modelSelectRoster())
	env := tui.GoldenCommandEnv()
	env.Auth = func() ([]tui.ProviderAuth, error) {
		return []tui.ProviderAuth{{Provider: "anthropic", Kind: tui.KindOAuth}}, nil
	}
	env.Config = func() (config.Config, error) {
		return config.Config{}, errors.New("read fail")
	}
	var saved []config.Config
	env.SaveConfig = func(c config.Config) error { saved = append(saved, c); return nil }

	m := newModelSelectApp(t, sup, env)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach the selected (sonnet) session
	m = dispatchSlash(t, m, "/model")
	m = pressDown(t, m, pressesToHaiku)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if len(saved) != 0 {
		t.Fatalf("SaveConfig calls = %v; want none — a config read error must abort before save (no data loss)", saved)
	}
	if len(sup.ops) != 0 {
		t.Fatalf("sup.ops = %v; want none — the read error must short-circuit before SetModel", sup.ops)
	}
	got := content(m)
	if !strings.Contains(got, "couldn't load config") {
		t.Fatalf("expected the config-load error status note, got:\n%s", got)
	}
}

// TestModelSelectTypedUnregisteredIDSetsDefault verifies the free-text entry
// reaches the coupled select end to end from the overview: an id the
// compiled-in catalog does not list is persisted as the session.model default
// exactly like a listed one, with the raw id standing in for a display name
// gofer has no label for yet.
func TestModelSelectTypedUnregisteredIDSetsDefault(t *testing.T) {
	const typed = "gpt-5.6-sol"
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

	m = dispatchSlash(t, m, "/model")
	m = type_(t, m, typed)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if len(saved) != 1 || saved[0].Session.Model != typed {
		t.Fatalf("SaveConfig calls = %v; want one entry with Session.Model %q", saved, typed)
	}
	if len(sup.ops) != 0 {
		t.Fatalf("sup.ops = %v; want none — no active session to swap", sup.ops)
	}
	if got := content(m); !strings.Contains(got, "Default model set to "+typed) {
		t.Fatalf("expected the default-set status note for the typed id, got:\n%s", got)
	}
}

// TestModelSelectTypedUnregisteredIDHotSwapsSameProvider is the regression
// guard for how handleModelSelect compares providers. An unregistered id is
// absent from the registry, so a membership-based comparison reports no
// provider for it and misclassifies a same-provider pick as cross-provider —
// silently declining a live swap that is perfectly legal. The comparison goes
// through provider.Resolve (panel.go's modelProvider), which infers the
// backend from the id's shape, so a typed anthropic model still hot-swaps an
// attached anthropic session.
func TestModelSelectTypedUnregisteredIDHotSwapsSameProvider(t *testing.T) {
	const typed = "claude-sonnet-5-9" // unregistered, but unmistakably anthropic
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach the selected (sonnet) session
	m = dispatchSlash(t, m, "/model")
	m = type_(t, m, typed)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	wantOp := "set-model:0192a1b2-app0-7000-8000-000000000001:" + typed
	if len(sup.ops) != 1 || sup.ops[0] != wantOp {
		t.Fatalf("sup.ops = %v; want one entry %q — an unregistered same-provider id must still hot-swap", sup.ops, wantOp)
	}
	if len(saved) != 1 || saved[0].Session.Model != typed {
		t.Fatalf("SaveConfig calls = %v; want one entry with Session.Model %q", saved, typed)
	}
	got := content(m)
	if strings.Contains(got, "Live model swap needs the same provider") {
		t.Fatalf("expected no cross-provider warning for a same-provider typed id, got:\n%s", got)
	}
	if !strings.Contains(got, "Model set to "+typed) {
		t.Fatalf("expected the hot-swap status note for the typed id, got:\n%s", got)
	}
}

// TestModelSelectTypedUnroutableIDIsNoOp verifies an id no provider family
// matches never reaches config or the daemon: there is no adapter to run it,
// so committing it would persist a default that fails at the next run. Enter
// is a no-op and the panel stays open with the reason on screen.
func TestModelSelectTypedUnroutableIDIsNoOp(t *testing.T) {
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

	m = dispatchSlash(t, m, "/model")
	m = type_(t, m, "not-a-real-family")
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if len(saved) != 0 {
		t.Fatalf("SaveConfig calls = %v; want none — an unroutable id must not be persisted", saved)
	}
	if len(sup.ops) != 0 {
		t.Fatalf("sup.ops = %v; want none", sup.ops)
	}
	got := content(m)
	if !strings.Contains(got, "[Model]") {
		t.Fatalf("expected the panel to stay open after a refused select, got:\n%s", got)
	}
	if !strings.Contains(got, "cannot determine which provider") {
		t.Fatalf("expected the refusal reason to stay on screen, got:\n%s", got)
	}
}

// TestModelSelectTypedIDShowsNoFabricatedPricing is the App-level counterpart
// to the picker's unknown-metadata guard: the id is unregistered, so the
// panel must not put a $0 price or a 0-token context window on screen for it.
// The assertion is scoped to the typed id's own line — the registered rows
// below it legitimately carry sub-dollar prices like "$0.25/$2".
func TestModelSelectTypedIDShowsNoFabricatedPricing(t *testing.T) {
	const typed = "gpt-5.6-sol"
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

	m = dispatchSlash(t, m, "/model")
	m = type_(t, m, typed)

	var line string
	for _, l := range strings.Split(content(m), "\n") {
		if strings.Contains(l, typed+" ("+typed+")") {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("expected a description line for the typed id, got:\n%s", content(m))
	}
	if !strings.Contains(line, "context unknown") || !strings.Contains(line, "pricing unknown") {
		t.Fatalf("expected the typed id's metadata to render as unknown, got %q", line)
	}
	for _, fabricated := range []string{"$0", "0 context", "per Mtok"} {
		if strings.Contains(line, fabricated) {
			t.Fatalf("expected no fabricated pricing or context window, got %q (contains %q)", line, fabricated)
		}
	}
}

// TestGoldenModelSelectHotSwap covers the full post-select rendered state
// for a same-provider attached select: the panel has closed back to the
// attach screen underneath it, with the confirmation note as the visible
// transient status line.
func TestGoldenModelSelectHotSwap(t *testing.T) {
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach the selected (sonnet) session
	m = dispatchSlash(t, m, "/model")
	m = pressDown(t, m, pressesToHaiku)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	testkit.AssertGolden(t, "app_model_select_hot_swap", content(m))
}

// TestGoldenModelSelectCrossProviderWarn covers the full post-select
// rendered state for a cross-provider attached select: the panel has closed,
// and the client-side warning note (not an error from the daemon) is the
// visible transient status line.
func TestGoldenModelSelectCrossProviderWarn(t *testing.T) {
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach the selected (sonnet) session
	m = dispatchSlash(t, m, "/model")
	m = pressDown(t, m, pressesToGPT5)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	testkit.AssertGolden(t, "app_model_select_cross_provider_warn", content(m))
}

// TestGoldenModelSelectCrossProviderWarnStyled is
// TestGoldenModelSelectCrossProviderWarn's color-state counterpart: the
// warning note renders in DangerStyle — the same status-line style every
// other transient a.status note uses (app.go's render) — invisible under
// theme.Test()'s forced Ascii profile.
func TestGoldenModelSelectCrossProviderWarnStyled(t *testing.T) {
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectAppWithTheme(t, testkit.ColorTheme(), sup, modelSelectEnv(&saved))

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach the selected (sonnet) session
	m = dispatchSlash(t, m, "/model")
	m = pressDown(t, m, pressesToGPT5)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	testkit.AssertGoldenStyled(t, "app_model_select_cross_provider_warn", content(m))
}
