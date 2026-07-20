package tui_test

// model_arg_test.go covers `/model <id>` — the string form of the /model
// command (issue #165). Its contract is that it lands on the SAME commit path
// as picking a row in the picker (App.applyModelSelection, panel.go), so each
// test here deliberately mirrors its picker-driven twin in model_select_test.go
// and asserts the identical observable result: the same config write, the same
// Supervisor.SetModel call (or absence of one), and the same status note.
// Divergence between the two files is the bug this file exists to catch.
//
// It reuses model_select_test.go's fixtures (modelSelectRoster,
// modelSelectEnv, newModelSelectApp) and command_test.go's dispatchSlash.

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/config"
)

// TestModelArgAttachedSameProviderHotSwaps is the string-form twin of
// TestModelSelectAttachedSameProviderHotSwaps: `/model claude-haiku-4-5` on an
// attached anthropic session must persist the default AND hot-swap the running
// session, with the same note, without ever opening the picker.
func TestModelArgAttachedSameProviderHotSwaps(t *testing.T) {
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach the selected (sonnet) session
	m = dispatchSlash(t, m, "/model claude-haiku-4-5")

	if len(saved) != 1 || saved[0].Session.Model != "claude-haiku-4-5" {
		t.Fatalf("SaveConfig calls = %v; want one entry with Session.Model claude-haiku-4-5", saved)
	}
	wantOp := "set-model:0192a1b2-app0-7000-8000-000000000001:claude-haiku-4-5"
	if len(sup.ops) != 1 || sup.ops[0] != wantOp {
		t.Fatalf("sup.ops = %v; want one entry %q", sup.ops, wantOp)
	}
	got := content(m)
	if strings.Contains(got, "[Model]") {
		t.Fatalf("/model <id> must apply directly, never open the picker, got:\n%s", got)
	}
	if !strings.Contains(got, "Model set to Haiku 4.5") {
		t.Fatalf("expected the same hot-swap status note the picker produces, got:\n%s", got)
	}
}

// TestModelArgAttachedCrossProviderWarnsOnly is the string-form twin of
// TestModelSelectAttachedCrossProviderWarnsOnly.
func TestModelArgAttachedCrossProviderWarnsOnly(t *testing.T) {
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach the selected (sonnet) session
	m = dispatchSlash(t, m, "/model gpt-5")

	if len(saved) != 1 || saved[0].Session.Model != "gpt-5" {
		t.Fatalf("SaveConfig calls = %v; want one entry with Session.Model gpt-5", saved)
	}
	if len(sup.ops) != 0 {
		t.Fatalf("sup.ops = %v; want none — a cross-provider id must not call SetModel", sup.ops)
	}
	if got := content(m); !strings.Contains(got, "Live model swap needs the same provider") {
		t.Fatalf("expected the same cross-provider warning the picker produces, got:\n%s", got)
	}
}

// TestModelArgOverviewSetsDefaultOnly covers `/model <id>` typed with no
// attached or peeked session: only the default is written, SetModel is never
// called.
func TestModelArgOverviewSetsDefaultOnly(t *testing.T) {
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

	m = dispatchSlash(t, m, "/model claude-haiku-4-5")

	if len(saved) != 1 || saved[0].Session.Model != "claude-haiku-4-5" {
		t.Fatalf("SaveConfig calls = %v; want one entry with Session.Model claude-haiku-4-5", saved)
	}
	if len(sup.ops) != 0 {
		t.Fatalf("sup.ops = %v; want none — there is no session to swap from the overview", sup.ops)
	}
	if got := content(m); !strings.Contains(got, "Default model set to Haiku 4.5") {
		t.Fatalf("expected the default-only status note, got:\n%s", got)
	}
}

// TestModelArgAppliesIdAbsentFromCatalog locks in design decision 1: an id
// [provider.Resolve] can route but the compiled-in catalog does not list
// APPLIES. The catalog is a vendor listing that goes stale, comes back empty,
// or is unreachable offline; gating on it would break the string override in
// exactly the situations it exists for. It is also the picker's own rule for a
// typed entry (modelpicker.go's selectedModel), so the two paths agree.
//
// The first half of the test proves the premise — that this id really is
// absent from the catalog the picker renders — so the test cannot quietly
// degrade into "applies an id that was listed all along".
func TestModelArgAppliesIdAbsentFromCatalog(t *testing.T) {
	const unlisted = "claude-sonnet-9-future" // routable by prefix, not in the catalog

	var savedProbe []config.Config
	probe := newModelSelectApp(t, newFakeSup(modelSelectRoster()), modelSelectEnv(&savedProbe))
	probe = dispatchSlash(t, probe, "/model")
	if listing := content(probe); strings.Contains(listing, unlisted) {
		t.Fatalf("premise broken: %q is now IN the rendered catalog, so this test no longer "+
			"exercises the absent-from-catalog rule:\n%s", unlisted, listing)
	}

	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach the selected (sonnet) session
	m = dispatchSlash(t, m, "/model "+unlisted)

	if len(saved) != 1 || saved[0].Session.Model != unlisted {
		t.Fatalf("SaveConfig calls = %v; want one entry with Session.Model %q", saved, unlisted)
	}
	wantOp := "set-model:0192a1b2-app0-7000-8000-000000000001:" + unlisted
	if len(sup.ops) != 1 || sup.ops[0] != wantOp {
		t.Fatalf("sup.ops = %v; want one entry %q — an unlisted but routable id is a live swap "+
			"like any other same-provider id", sup.ops, wantOp)
	}
	if got := content(m); strings.Contains(got, "[Model]") {
		t.Fatalf("an unlisted but routable id must apply directly, not open the picker, got:\n%s", got)
	}
}

// TestModelArgUnroutableReportsDanger covers the rejection path: an id no
// provider adapter can route must produce a danger note naming it, write
// nothing, and — the specific #165 failure — never silently open the picker.
func TestModelArgUnroutableReportsDanger(t *testing.T) {
	const bogus = "no-such-vendor/no-such-model-165"

	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

	m = dispatchSlash(t, m, "/model "+bogus)

	if len(saved) != 0 {
		t.Fatalf("SaveConfig calls = %v; want none — an unroutable id must not be persisted", saved)
	}
	if len(sup.ops) != 0 {
		t.Fatalf("sup.ops = %v; want none", sup.ops)
	}
	got := content(m)
	if strings.Contains(got, "[Model]") {
		t.Fatalf("an unroutable id must be reported, not answered with a silently-opened picker, got:\n%s", got)
	}
	if !strings.Contains(got, bogus) {
		t.Fatalf("expected a status note naming %q, got:\n%s", bogus, got)
	}
}

// TestModelArgMultipleArgsRejected covers the multi-word case. parseSlash
// splits on whitespace and no model id contains a space, so `/model a b` is
// always a mistake — and the one thing it must NOT do is quietly apply "a".
func TestModelArgMultipleArgsRejected(t *testing.T) {
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

	m = dispatchSlash(t, m, "/model claude-haiku-4-5 gpt-5")

	if len(saved) != 0 {
		t.Fatalf("SaveConfig calls = %v; want none — /model with two arguments must apply neither", saved)
	}
	if len(sup.ops) != 0 {
		t.Fatalf("sup.ops = %v; want none", sup.ops)
	}
	got := content(m)
	if strings.Contains(got, "[Model]") {
		t.Fatalf("expected a rejection note, not the picker, got:\n%s", got)
	}
	if !strings.Contains(got, "single model id") {
		t.Fatalf("expected a note explaining /model takes one id, got:\n%s", got)
	}
}

// TestModelBareStillOpensPicker guards the half of /model that must NOT
// change: with no argument it still opens the command panel on the Model tab.
func TestModelBareStillOpensPicker(t *testing.T) {
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

	m = dispatchSlash(t, m, "/model")

	if got := content(m); !strings.Contains(got, "[Model]") {
		t.Fatalf("bare /model must still open the picker, got:\n%s", got)
	}
	if len(saved) != 0 {
		t.Fatalf("SaveConfig calls = %v; want none — opening the picker writes nothing", saved)
	}
}
