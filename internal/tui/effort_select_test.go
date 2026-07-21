package tui_test

// effort_select_test.go covers /thinking end to end through App's exported
// Update/View surface: the picker's coupled Enter/select
// (App.handleEffortSelect) and the `/thinking <level>` string form, which must
// land on the SAME commit path (App.applyEffortSelection) — the same config
// write, the same Supervisor.SetEffort call (or absence of one), and the same
// status note. It is the effort-axis twin of model_select_test.go +
// model_arg_test.go, and divergence between the two paths is the bug it exists
// to catch.
//
// The capability-gate cases are NOT here: they need the unexported
// model-registry seam and live in effortpicker_test.go (package tui).
//
// It reuses model_select_test.go's fixtures (modelSelectRoster, modelSelectEnv,
// newModelSelectApp) and command_test.go's dispatchSlash.

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

// The failure fixtures the error-path tests inject, named so the assertions
// can look for the exact message rather than a substring that might match
// something else on screen.
var (
	errTestSaveFailed      = errors.New("disk full")
	errTestReadFailed      = errors.New("read fail")
	errTestSetEffortFailed = errors.New("daemon refused the effort")
)

// TestThinkingArgAttachedHotSwaps is the core string-form path: `/thinking
// high` on an attached session persists the default AND changes the running
// session, without ever opening the picker.
func TestThinkingArgAttachedHotSwaps(t *testing.T) {
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach the selected session
	m = dispatchSlash(t, m, "/thinking high")

	if len(saved) != 1 || saved[0].Session.Effort != "high" {
		t.Fatalf("SaveConfig calls = %v; want one entry with Session.Effort high", saved)
	}
	wantOp := "set-effort:" + attachedSessionID + ":high"
	if len(sup.ops) != 1 || sup.ops[0] != wantOp {
		t.Fatalf("sup.ops = %v; want one entry %q", sup.ops, wantOp)
	}
	got := content(m)
	if strings.Contains(got, "[Thinking]") {
		t.Fatalf("/thinking <level> must apply directly, never open the picker, got:\n%s", got)
	}
	if !strings.Contains(got, "Reasoning effort set to high for this session.") {
		t.Fatalf("expected the hot-swap status note, got:\n%s", got)
	}
}

// TestThinkingArgCrossProviderStillSwaps is the deliberate NON-mirror of
// TestModelArgAttachedCrossProviderWarnsOnly. /model refuses a live swap across
// providers because a session's provider client is fixed at creation; effort is
// provider-agnostic, so an openai level on an anthropic session (and any other
// pairing) is a plain, unqualified swap with no warning at all. A "symmetry"
// refactor that copies /model's provider branch across fails here.
func TestThinkingArgCrossProviderStillSwaps(t *testing.T) {
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster()) // row 0 runs claude-sonnet-5
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
	m = dispatchSlash(t, m, "/thinking medium")

	wantOp := "set-effort:" + attachedSessionID + ":medium"
	if len(sup.ops) != 1 || sup.ops[0] != wantOp {
		t.Fatalf("sup.ops = %v; want one entry %q — effort carries no provider constraint", sup.ops, wantOp)
	}
	if got := content(m); strings.Contains(got, "provider") {
		t.Fatalf("expected no provider caveat for an effort change, got:\n%s", got)
	}
}

// TestThinkingArgOffClearsTheLevel covers the empty level's user-facing
// spelling: `off` maps to "", which is a real request (clear back to the
// provider's default) that must be BOTH persisted and sent — not skipped as
// "nothing to do". The recorded op has an empty trailing field precisely so a
// clear is distinguishable from no call at all.
func TestThinkingArgOffClearsTheLevel(t *testing.T) {
	for _, spelling := range []string{"off", "none", "default"} {
		t.Run(spelling, func(t *testing.T) {
			var saved []config.Config
			sup := newFakeSup(modelSelectRoster())
			m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

			m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
			m = dispatchSlash(t, m, "/thinking "+spelling)

			if len(saved) != 1 || saved[0].Session.Effort != "" {
				t.Fatalf("SaveConfig calls = %v; want one entry clearing Session.Effort", saved)
			}
			wantOp := "set-effort:" + attachedSessionID + ":"
			if len(sup.ops) != 1 || sup.ops[0] != wantOp {
				t.Fatalf("sup.ops = %v; want one entry %q — the clear must reach the daemon", sup.ops, wantOp)
			}
			if got := content(m); !strings.Contains(got, "Reasoning effort set to off for this session.") {
				t.Fatalf("expected the clear's status note to name it \"off\", got:\n%s", got)
			}
		})
	}
}

// TestThinkingArgOverviewSetsDefaultOnly covers `/thinking <level>` typed with
// no attached or peeked session: only the default is written, SetEffort is
// never called, and the note claims nothing about sessions that do not exist.
func TestThinkingArgOverviewSetsDefaultOnly(t *testing.T) {
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

	m = dispatchSlash(t, m, "/thinking low")

	if len(saved) != 1 || saved[0].Session.Effort != "low" {
		t.Fatalf("SaveConfig calls = %v; want one entry with Session.Effort low", saved)
	}
	if len(sup.ops) != 0 {
		t.Fatalf("sup.ops = %v; want none — there is no session to change from the overview", sup.ops)
	}
	if got := content(m); !strings.Contains(got, "Default reasoning effort saved: low.") {
		t.Fatalf("expected the default-only status note, got:\n%s", got)
	}
}

// TestThinkingArgUnknownLevelReportsDanger covers the rejection path: a level
// outside the vocabulary must produce a danger note naming it, write nothing,
// and — the #165 failure this whole guard exists for — never silently open the
// picker instead.
func TestThinkingArgUnknownLevelReportsDanger(t *testing.T) {
	const bogus = "ultra"

	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

	m = dispatchSlash(t, m, "/thinking "+bogus)

	if len(saved) != 0 {
		t.Fatalf("SaveConfig calls = %v; want none — an unknown level must not be persisted", saved)
	}
	if len(sup.ops) != 0 {
		t.Fatalf("sup.ops = %v; want none", sup.ops)
	}
	got := content(m)
	if strings.Contains(got, "[Thinking]") {
		t.Fatalf("an unknown level must be reported, not answered with a silently-opened picker, got:\n%s", got)
	}
	if !strings.Contains(got, bogus) {
		t.Fatalf("expected a status note naming %q, got:\n%s", bogus, got)
	}
}

// TestThinkingArgMultipleArgsRejected covers the multi-word case: no level
// contains a space, so `/thinking low high` is always a mistake, and the one
// thing it must NOT do is quietly apply "low".
func TestThinkingArgMultipleArgsRejected(t *testing.T) {
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

	m = dispatchSlash(t, m, "/thinking low high")

	if len(saved) != 0 {
		t.Fatalf("SaveConfig calls = %v; want none — /thinking with two arguments must apply neither", saved)
	}
	if len(sup.ops) != 0 {
		t.Fatalf("sup.ops = %v; want none", sup.ops)
	}
	got := content(m)
	if strings.Contains(got, "[Thinking]") {
		t.Fatalf("expected a rejection note, not the picker, got:\n%s", got)
	}
	if !strings.Contains(got, "single level") {
		t.Fatalf("expected a note explaining /thinking takes one level, got:\n%s", got)
	}
}

// TestThinkingAliasDispatches covers the /effort alias reaching the same Run —
// registering an alias that nothing routes through would be an invisible defect.
func TestThinkingAliasDispatches(t *testing.T) {
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

	m = dispatchSlash(t, m, "/effort medium")

	if len(saved) != 1 || saved[0].Session.Effort != "medium" {
		t.Fatalf("SaveConfig calls = %v; want /effort to behave exactly like /thinking", saved)
	}
	if got := content(m); strings.Contains(got, "unknown command") {
		t.Fatalf("expected /effort to resolve, got:\n%s", got)
	}
}

// TestThinkingBareOpensPicker guards the other half of /thinking: with no
// argument it opens the command panel on the Thinking tab and writes nothing.
func TestThinkingBareOpensPicker(t *testing.T) {
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

	m = dispatchSlash(t, m, "/thinking")

	if got := content(m); !strings.Contains(got, "[Thinking]") {
		t.Fatalf("bare /thinking must open the picker, got:\n%s", got)
	}
	if len(saved) != 0 {
		t.Fatalf("SaveConfig calls = %v; want none — opening the picker writes nothing", saved)
	}
	if len(sup.ops) != 0 {
		t.Fatalf("sup.ops = %v; want none", sup.ops)
	}
}

// TestThinkingPanelDoesNotFetchModels pins the cost boundary from the other
// side of TestOpenStatusPanelDoesNotFetchModels: the level list is a fixed enum,
// so opening the Thinking tab must issue no vendor request at all.
func TestThinkingPanelDoesNotFetchModels(t *testing.T) {
	var asked []string
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, discoveryEnv(&asked))

	m, cmd := dispatchSlashCmd(t, m, "/thinking")
	_ = runCmd(t, m, cmd)

	if len(asked) != 0 {
		t.Fatalf("env.Models called for %v on /thinking, want no vendor request — the level list is a fixed enum", asked)
	}
}

// TestThinkingSelectAttachedCommits covers the PICKER path reaching the same
// place the string form does: ↓ to a level, Enter, and the panel closes having
// written the default and changed the running session.
func TestThinkingSelectAttachedCommits(t *testing.T) {
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach
	m = dispatchSlash(t, m, "/thinking")
	// The highlight opens on the active level (off, row 0), so two ↓ presses
	// land on medium.
	m = pressDown(t, m, 2)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if len(saved) != 1 || saved[0].Session.Effort != "medium" {
		t.Fatalf("SaveConfig calls = %v; want one entry with Session.Effort medium", saved)
	}
	wantOp := "set-effort:" + attachedSessionID + ":medium"
	if len(sup.ops) != 1 || sup.ops[0] != wantOp {
		t.Fatalf("sup.ops = %v; want one entry %q", sup.ops, wantOp)
	}
	got := content(m)
	if strings.Contains(got, "[Thinking]") {
		t.Fatalf("expected the panel to close after a committed select, got:\n%s", got)
	}
	if !strings.Contains(got, "Reasoning effort set to medium for this session.") {
		t.Fatalf("expected the same note the string form produces, got:\n%s", got)
	}
}

// TestThinkingSelectSaveConfigErrorStopsBeforeSetEffort verifies a SaveConfig
// failure surfaces as the status note and short-circuits before any
// Supervisor.SetEffort call — applyModelSelection's ordering, mirrored.
func TestThinkingSelectSaveConfigErrorStopsBeforeSetEffort(t *testing.T) {
	sup := newFakeSup(modelSelectRoster())
	env := modelSelectEnv(nil)
	env.SaveConfig = func(config.Config) error { return errTestSaveFailed }

	m := newModelSelectApp(t, sup, env)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
	m = dispatchSlash(t, m, "/thinking")
	m = pressDown(t, m, 2)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if len(sup.ops) != 0 {
		t.Fatalf("sup.ops = %v; want none — a SaveConfig error must stop before SetEffort", sup.ops)
	}
	if got := content(m); !strings.Contains(got, "couldn't save default reasoning effort") {
		t.Fatalf("expected the SaveConfig error status note, got:\n%s", got)
	}
}

// TestThinkingSelectConfigReadErrorAbortsBeforeSave guards against the same
// silent data loss applyModelSelection guards: a failed config READ must not
// fall through to SaveConfig with a zero-value config, which would overwrite
// config.json and drop the user's permissions/telemetry settings.
func TestThinkingSelectConfigReadErrorAbortsBeforeSave(t *testing.T) {
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	env := modelSelectEnv(&saved)
	env.Config = func() (config.Config, error) { return config.Config{}, errTestReadFailed }

	m := newModelSelectApp(t, sup, env)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
	m = dispatchSlash(t, m, "/thinking")
	m = pressDown(t, m, 2)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if len(saved) != 0 {
		t.Fatalf("SaveConfig calls = %v; want none — a config read error must abort before save (no data loss)", saved)
	}
	if len(sup.ops) != 0 {
		t.Fatalf("sup.ops = %v; want none — the read error must short-circuit before SetEffort", sup.ops)
	}
	if got := content(m); !strings.Contains(got, "couldn't load config") {
		t.Fatalf("expected the config-load error status note, got:\n%s", got)
	}
}

// TestThinkingSetEffortErrorSurfacesAsDanger covers the failed-op path: a
// daemon-side rejection (an unknown level from a newer daemon, or the runner's
// own capability refusal) arrives as opDoneMsg — the ONLY op→danger route — and
// replaces the optimistic note this process already set.
func TestThinkingSetEffortErrorSurfacesAsDanger(t *testing.T) {
	var saved []config.Config
	sup := newFakeSup(modelSelectRoster())
	sup.setEffortErr = errTestSetEffortFailed
	m := newModelSelectApp(t, sup, modelSelectEnv(&saved))

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
	m, cmd := dispatchSlashCmd(t, m, "/thinking high")
	m = runCmd(t, m, cmd)

	if got := content(m); !strings.Contains(got, errTestSetEffortFailed.Error()) {
		t.Fatalf("expected the daemon's rejection to reach the status line, got:\n%s", got)
	}
}

// TestGoldenPanelThinking covers the whole rendered panel on the Thinking tab,
// including the tab bar and footer — the frame a user actually sees for
// `/thinking`.
func TestGoldenPanelThinking(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = dispatchSlash(t, m, "/thinking")
	testkit.AssertGolden(t, "app_panel_thinking", content(m))
}

// TestGoldenPanelThinkingFirstFrame is the zero-height guard at the APP level:
// a frame rendered before any WindowSizeMsg has arrived (width/height 0), with
// the panel open. A zero-height first frame has panicked this TUI before, and
// the panel overlay is the part that does the most arithmetic on the available
// rows.
func TestGoldenPanelThinkingFirstFrame(t *testing.T) {
	sup := newFakeSup(modelSelectRoster())
	var m tea.Model = tui.NewApp(theme.Test(), sup, tui.GoldenMeta(), modelSelectEnv(nil))
	m, _ = m.Update(m.Init()())
	m = dispatchSlash(t, m, "/thinking")

	// Rendering must not panic, at zero size or at a one-row terminal.
	_ = content(m)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 1, Height: 1})
	_ = content(m)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 5})
	if got := content(m); got == "" {
		t.Fatal("expected a rendered frame at 80x5 with the panel open")
	}
}
