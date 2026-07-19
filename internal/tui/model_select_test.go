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
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/modelcatalog"
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
	const typed = "gpt-9-unreleased"
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
	const typed = "gpt-9-unreleased"
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

// TestModelSelectUpdatesOverviewHeader is the visible half of issue #156's TUI
// fix. The roster header's model is seeded once at NewApp time from a value
// cmd/gofer resolved at startup, so before this it kept displaying the OLD
// default after /model set a new one — the status line claimed the default was
// set while the line directly above it said otherwise, and only a restart
// reconciled them. The header must move with the selection, with no restart.
func TestModelSelectUpdatesOverviewHeader(t *testing.T) {
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

	// GoldenMeta seeds the header with claude-sonnet-5.
	if got := content(m); !strings.Contains(got, "claude-sonnet-5 ·") {
		t.Fatalf("test premise broken: expected the header to start on claude-sonnet-5, got:\n%s", got)
	}

	m = dispatchSlash(t, m, "/model")
	m = pressDown(t, m, pressesToHaiku)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	got := content(m)
	if !strings.Contains(got, "claude-haiku-4-5 ·") {
		t.Fatalf("expected the header to show the newly selected default without a restart, got:\n%s", got)
	}
	if strings.Contains(got, "claude-sonnet-5 ·") {
		t.Fatalf("expected the stale default to be gone from the header, got:\n%s", got)
	}
}

// daemonModelSelectEnv is modelSelectEnv with the backend marked
// daemon-attached — the state a TUI is in whenever a `gofer daemon` is
// reachable (cmd/gofer's selectTUIBackend).
func daemonModelSelectEnv(saved *[]config.Config) tui.CommandEnv {
	env := modelSelectEnv(saved)
	env.DaemonBacked = true
	return env
}

// TestModelSelectDaemonAttachedDoesNotClaimTheDefaultTookEffect covers the
// SECOND staleness layer (issue #156's follow-up comment), the one this TUI
// cannot fix and must therefore not paper over. A running daemon resolves its
// default model exactly once at its own startup and never re-reads config.json,
// so from a daemon-attached client the config write reaches nothing the daemon
// will do. "Default model set to X." full stop would assert an effect that did
// not occur.
//
// The write itself still happens — it is what a future daemon will read — so
// this is about the CLAIM, not about skipping the save.
func TestModelSelectDaemonAttachedDoesNotClaimTheDefaultTookEffect(t *testing.T) {
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, daemonModelSelectEnv(&saved))

	m = dispatchSlash(t, m, "/model")
	m = pressDown(t, m, pressesToHaiku)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if len(saved) != 1 || saved[0].Session.Model != "claude-haiku-4-5" {
		t.Fatalf("SaveConfig calls = %v; want the default still persisted for future daemons", saved)
	}
	got := content(m)
	// Asserted against the RENDERED, width-truncated view on purpose: a caveat
	// that only exists off the right edge of an 80-column screen is not a
	// caveat, and the unqualified claim is what the user would actually read.
	// The daemon now re-reads its default per session/new, so an UNPINNED
	// daemon does adopt this write — "unchanged until restart" (the pre-fix
	// wording) would now be false. The TUI cannot yet tell pinned from
	// unpinned, so the wording must be true under both.
	if !strings.Contains(got, "Default saved; attached daemon adopts it unless pinned.") {
		t.Fatalf("expected the status note to state the default's reach without over- or under-claiming, got:\n%s", got)
	}
	assertStatusFitsWidth(t, got, "Default saved; attached daemon adopts it unless pinned.")
	// The header renders the DAEMON's own default (off gofer/hello). The
	// daemon did not change, so neither may the header — updating it would be
	// the same overclaim in a second place.
	if !strings.Contains(got, "claude-sonnet-5 ·") {
		t.Fatalf("expected the header to keep showing the daemon's unchanged default, got:\n%s", got)
	}
	if strings.Contains(got, "claude-haiku-4-5 ·") {
		t.Fatalf("expected the header NOT to adopt a default the attached daemon never saw, got:\n%s", got)
	}
}

// TestModelSelectDaemonAttachedHotSwapStillReportsDefaultReach covers the
// attached-session variant: the live swap DOES cross the wire and take effect,
// so that half of the note is unqualified — but the default's reach is still
// the daemon's, and the note must say so in the same breath rather than
// letting "Model set to X." stand in for both.
func TestModelSelectDaemonAttachedHotSwapStillReportsDefaultReach(t *testing.T) {
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, daemonModelSelectEnv(&saved))

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach the selected (sonnet) session
	m = dispatchSlash(t, m, "/model")
	m = pressDown(t, m, pressesToHaiku)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	wantOp := "set-model:0192a1b2-app0-7000-8000-000000000001:claude-haiku-4-5"
	if len(sup.ops) != 1 || sup.ops[0] != wantOp {
		t.Fatalf("sup.ops = %v; want one entry %q — the live swap crosses the wire on the daemon path too", sup.ops, wantOp)
	}
	got := content(m)
	// Standalone on the daemon path, not note+suffix: the base note plus any
	// meaningful caveat busts the 80-column floor, and a caveat truncated off
	// the right edge leaves exactly the unqualified overclaim behind.
	if !strings.Contains(got, "Model set for this session; daemon adopts the default unless pinned.") {
		t.Fatalf("expected the note to report the live swap AND the default's reach, got:\n%s", got)
	}
	assertStatusFitsWidth(t, got, "Model set for this session; daemon adopts the default unless pinned.")
}

// assertStatusFitsWidth pins the property that made the pre-fix wording a bug
// twice over: a status note is truncated to the terminal width, so any part of
// it that does not fit is not merely invisible — it silently converts a
// qualified statement back into the unqualified overclaim. want must therefore
// survive INTACT in the rendered, already-truncated view.
func assertStatusFitsWidth(t *testing.T, rendered, want string) {
	t.Helper()
	if len([]rune(want)) > 80 {
		t.Fatalf("status note is %d columns, over the 80-column floor the golden tests pin: %q", len([]rune(want)), want)
	}
	if !strings.Contains(rendered, want) {
		t.Fatalf("status note did not survive truncation intact; want %q in:\n%s", want, rendered)
	}
}

// TestModelSelectLocalBackendMakesNoDaemonClaim is
// TestModelSelectDaemonAttachedDoesNotClaimTheDefaultTookEffect's twin, and the
// reason the qualification is conditional rather than always-on: on the local
// backend the write genuinely does take effect (the bridge re-resolves the
// default per create), so there is nothing to caveat and a daemon caveat would
// itself be the false statement.
func TestModelSelectLocalBackendMakesNoDaemonClaim(t *testing.T) {
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

	m = dispatchSlash(t, m, "/model")
	m = pressDown(t, m, pressesToHaiku)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if got := content(m); strings.Contains(got, "daemon") {
		t.Fatalf("expected no daemon caveat on the local backend, got:\n%s", got)
	}
}

// discoveryEnv returns a CommandEnv whose Models closure records which
// providers were asked for and answers with a live-only catalog, so a test can
// tell "the panel host fetched" from "the picker rendered its floor".
func discoveryEnv(asked *[]string) tui.CommandEnv {
	env := tui.GoldenCommandEnv()
	env.Auth = func() ([]tui.ProviderAuth, error) {
		return []tui.ProviderAuth{{Provider: "openai", Kind: tui.KindOAuth}}, nil
	}
	env.Models = func(_ context.Context, providerID string) ([]modelcatalog.Model, error) {
		*asked = append(*asked, providerID)
		return []modelcatalog.Model{
			{ID: "gpt-5.9-nova", Provider: "openai", Label: "GPT-5.9 Nova", ContextWindow: 512_000},
		}, nil
	}
	return env
}

// dispatchSlashCmd is [dispatchSlash] that hands BACK the command rather than
// running it. dispatchSlash goes through press, which resolves any command
// immediately — fine for everything else, but it would hide the very thing
// these tests are about: that /model renders a usable panel BEFORE its catalog
// load resolves.
func dispatchSlashCmd(t *testing.T, m tea.Model, slash string) (tea.Model, tea.Cmd) {
	t.Helper()
	m = type_(t, m, slash)
	return m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
}

// runCmd executes a tea.Cmd and feeds its message back into the model, the way
// the bubbletea runtime does. Returns the model unchanged when cmd is nil.
func runCmd(t *testing.T, m tea.Model, cmd tea.Cmd) tea.Model {
	t.Helper()
	if cmd == nil {
		return m
	}
	msg := cmd()
	if msg == nil {
		return m
	}
	next, _ := m.Update(msg)
	return next
}

// TestOpenModelPanelFetchesAndUpgrades is the end-to-end path for the live
// picker: /model opens on the offline floor, returns a command, and the
// command's result upgrades the visible list — all without the open itself
// blocking on the fetch.
func TestOpenModelPanelFetchesAndUpgrades(t *testing.T) {
	var asked []string
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, discoveryEnv(&asked))

	m, cmd := dispatchSlashCmd(t, m, "/model")

	// The panel is already usable before the fetch resolves.
	if got := content(m); !strings.Contains(got, "gpt-5.6-terra") {
		t.Fatalf("expected the offline Codex floor on open, got:\n%s", got)
	}
	if cmd == nil {
		t.Fatal("expected /model to return a catalog-load command")
	}

	m = runCmd(t, m, cmd)

	if !slices.Equal(asked, []string{"openai"}) {
		t.Fatalf("env.Models asked for %v, want [openai]", asked)
	}
	got := content(m)
	if !strings.Contains(got, "GPT-5.9 Nova") {
		t.Fatalf("expected the live listing to replace the floor, got:\n%s", got)
	}
	if strings.Contains(got, "gpt-5.6-terra") {
		t.Fatalf("expected the floor to be gone once the live list landed, got:\n%s", got)
	}
}

// TestOpenStatusPanelDoesNotFetchModels pins the cost boundary: only /model
// pays for a vendor listing. Opening /status or /config must not issue a
// request the user never asked for.
func TestOpenStatusPanelDoesNotFetchModels(t *testing.T) {
	var asked []string
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, discoveryEnv(&asked))

	m, cmd := dispatchSlashCmd(t, m, "/status")
	_ = runCmd(t, m, cmd)

	if len(asked) != 0 {
		t.Fatalf("env.Models called for %v on /status, want no vendor request from a tab the user did not open", asked)
	}
}

// TestTabbingIntoModelFetches covers the other route to the picker: the panel
// opens on whichever tab the slash command named, but ←/→ reach all three, so
// arriving at Model by tabbing must load the catalog too.
func TestTabbingIntoModelFetches(t *testing.T) {
	var asked []string
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, discoveryEnv(&asked))

	m, _ = dispatchSlashCmd(t, m, "/status")
	if len(asked) != 0 {
		t.Fatalf("test premise broken: /status already fetched %v", asked)
	}

	// Status -> Config -> Model.
	var cmd tea.Cmd
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m = runCmd(t, m, cmd)

	if !slices.Equal(asked, []string{"openai"}) {
		t.Fatalf("env.Models asked for %v after tabbing into Model, want [openai]", asked)
	}
	if got := content(m); !strings.Contains(got, "GPT-5.9 Nova") {
		t.Fatalf("expected the live listing after tabbing into Model, got:\n%s", got)
	}
}

// TestModelsLoadedAfterPanelClosedIsDropped covers the in-flight-then-dismissed
// race: a load resolving after the user closed the panel must not resurrect it.
func TestModelsLoadedAfterPanelClosedIsDropped(t *testing.T) {
	var asked []string
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, discoveryEnv(&asked))

	m, cmd := dispatchSlashCmd(t, m, "/model")
	msg := cmd() // resolve the load, but do NOT deliver it yet

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEscape}) // close the panel
	if strings.Contains(content(m), "[Model]") {
		t.Fatal("test premise broken: expected Esc to close the panel")
	}

	m, _ = m.Update(msg)

	if got := content(m); strings.Contains(got, "[Model]") {
		t.Fatalf("a catalog load landing after the panel closed must not reopen it, got:\n%s", got)
	}
}

// TestModelSelectCommitsALiveOnlyID is the payoff, stated as behavior: an id
// that exists ONLY in the live listing — absent from the SDK registry, the
// static Codex floor, and modelmeta — is selectable from the list and persisted
// as the default. Before discovery there was no way to reach such a model
// except by typing its id from memory.
func TestModelSelectCommitsALiveOnlyID(t *testing.T) {
	var asked []string
	var saved []config.Config
	env := discoveryEnv(&asked)
	env.SaveConfig = func(c config.Config) error { saved = append(saved, c); return nil }

	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, env)

	m, cmd := dispatchSlashCmd(t, m, "/model")
	m = runCmd(t, m, cmd)
	m = pressDown(t, m, 1) // the live list's only row
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if len(saved) != 1 || saved[0].Session.Model != "gpt-5.9-nova" {
		t.Fatalf("SaveConfig calls = %v; want one entry with the live-only id gpt-5.9-nova", saved)
	}
	// The panel commits and closes, same as any other select — asserted so the
	// final press is observed rather than left as a dead assignment.
	if got := content(m); strings.Contains(got, "[Model]") {
		t.Fatalf("expected the panel to close after committing a live-only id, got:\n%s", got)
	}
}
