package tui

// config_view_test.go covers the /config command-panel tab (config_view.go,
// M4 step 3): the initial/filtered list render, navigating into and editing
// a row per [SettingKind] (bool toggle, enum cycle, string edit committing
// through SaveConfig), the two-stage Esc contract, and auth-independence (a
// nil/erroring Config or SaveConfig never blocks the view). White-box
// (package tui) because configView is unexported — the App-level "/config
// opens the panel" and end-to-end SaveConfig-persistence behavior are
// covered separately in command_test.go (package tui_test).

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

func renderConfig(t *testing.T, name string, v configView) {
	t.Helper()
	testkit.AssertGolden(t, name, testkit.Render(v, testkit.Width, testkit.Height))
}

func renderConfigStyled(t *testing.T, name string, v configView) {
	t.Helper()
	testkit.AssertGoldenStyled(t, name, testkit.Render(v, testkit.Width, testkit.Height))
}

// typeText feeds each rune of s through handleKey, mirroring how the app
// root would deliver keystrokes one at a time.
func typeText(v configView, s string) configView {
	for _, r := range s {
		v = v.handleKey(tea.KeyPressMsg{Text: string(r)})
	}
	return v
}

func pressKey(v configView, code rune) configView {
	return v.handleKey(tea.KeyPressMsg{Code: code})
}

// TestGoldenConfigInitialList covers the view's opening state: an empty
// filter box and every registry setting listed at its default value, no row
// highlighted.
func TestGoldenConfigInitialList(t *testing.T) {
	v := newConfigView(theme.Test(), GoldenCommandEnv())
	renderConfig(t, "config_initial", v)
}

// TestGoldenConfigInitialListStyled is TestGoldenConfigInitialList's
// color-state counterpart: the "Search settings…" placeholder renders in
// MutedStyle, invisible under theme.Test()'s forced Ascii profile.
func TestGoldenConfigInitialListStyled(t *testing.T) {
	v := newConfigView(testkit.ColorTheme(), GoldenCommandEnv())
	renderConfigStyled(t, "config_initial", v)
}

// TestGoldenConfigFilteredList covers substring filtering: "model" matches
// only the Default model row (by Label), dropping every other row from the
// list.
func TestGoldenConfigFilteredList(t *testing.T) {
	v := newConfigView(theme.Test(), GoldenCommandEnv())
	v = typeText(v, "model")
	renderConfig(t, "config_filtered", v)
}

// TestGoldenConfigRowSelected covers ↓ from the filter box selecting the
// first row: the "▸ " marker is plain text, visible even in the Ascii
// golden.
func TestGoldenConfigRowSelected(t *testing.T) {
	v := newConfigView(theme.Test(), GoldenCommandEnv())
	v = pressKey(v, tea.KeyDown)
	renderConfig(t, "config_row_selected", v)
}

// TestGoldenConfigRowSelectedStyled is TestGoldenConfigRowSelected's
// color-state counterpart: the highlighted row renders in AccentStyle.
func TestGoldenConfigRowSelectedStyled(t *testing.T) {
	v := newConfigView(testkit.ColorTheme(), GoldenCommandEnv())
	v = pressKey(v, tea.KeyDown)
	renderConfigStyled(t, "config_row_selected", v)
}

// TestGoldenConfigBoolToggle covers activating a highlighted bool row:
// Enter flips it in place (false → true, telemetry.enabled's default) and
// saves immediately.
func TestGoldenConfigBoolToggle(t *testing.T) {
	var saved []config.Config
	env := GoldenCommandEnv()
	env.SaveConfig = func(c config.Config) error {
		saved = append(saved, c)
		return nil
	}
	v := newConfigView(theme.Test(), env)
	// session.model, session.permission_mode, tui.roster_view,
	// telemetry.enabled — four ↓ presses (the first selects row 0) land on
	// telemetry.enabled.
	for i := 0; i < 4; i++ {
		v = pressKey(v, tea.KeyDown)
	}
	v = pressKey(v, tea.KeyEnter)
	renderConfig(t, "config_bool_toggled", v)

	if len(saved) != 1 {
		t.Fatalf("SaveConfig called %d times, want 1", len(saved))
	}
	if !saved[0].Telemetry.Enabled {
		t.Fatalf("saved Telemetry.Enabled = false, want true")
	}
}

// TestGoldenConfigEnumCycle covers activating a highlighted enum row: Enter
// cycles session.permission_mode from its default "ask" to "yolo".
func TestGoldenConfigEnumCycle(t *testing.T) {
	var saved []config.Config
	env := GoldenCommandEnv()
	env.SaveConfig = func(c config.Config) error {
		saved = append(saved, c)
		return nil
	}
	v := newConfigView(theme.Test(), env)
	v = pressKey(v, tea.KeyDown) // session.model
	v = pressKey(v, tea.KeyDown) // session.permission_mode
	v = pressKey(v, tea.KeyEnter)
	renderConfig(t, "config_enum_cycled", v)

	if len(saved) != 1 {
		t.Fatalf("SaveConfig called %d times, want 1", len(saved))
	}
	if saved[0].Session.PermissionMode != "yolo" {
		t.Fatalf("saved Session.PermissionMode = %q, want yolo", saved[0].Session.PermissionMode)
	}
}

// TestGoldenConfigStringEditCommit covers a string row's full edit flow:
// Enter opens the inline edit line, typed text edits its buffer, a second
// Enter commits and saves.
func TestGoldenConfigStringEditCommit(t *testing.T) {
	var saved []config.Config
	env := GoldenCommandEnv()
	env.SaveConfig = func(c config.Config) error {
		saved = append(saved, c)
		return nil
	}
	v := newConfigView(theme.Test(), env)
	v = pressKey(v, tea.KeyDown)  // select session.model (row 0)
	v = pressKey(v, tea.KeyEnter) // open the inline edit line
	v = typeText(v, "claude-sonnet-5")
	v = pressKey(v, tea.KeyEnter) // commit
	renderConfig(t, "config_string_edited", v)

	if len(saved) != 1 {
		t.Fatalf("SaveConfig called %d times, want 1", len(saved))
	}
	if saved[0].Session.Model != "claude-sonnet-5" {
		t.Fatalf("saved Session.Model = %q, want claude-sonnet-5", saved[0].Session.Model)
	}
}

// TestConfigStringEditBackspaceEdits covers Backspace editing the
// in-progress buffer rather than the filter while a string edit is open.
func TestConfigStringEditBackspaceEdits(t *testing.T) {
	v := newConfigView(theme.Test(), GoldenCommandEnv())
	v = pressKey(v, tea.KeyDown)
	v = pressKey(v, tea.KeyEnter)
	v = typeText(v, "abc")
	v = pressKey(v, tea.KeyBackspace)
	if v.editBuf != "ab" {
		t.Fatalf("editBuf = %q, want %q", v.editBuf, "ab")
	}
	if v.filter != "" {
		t.Fatalf("filter = %q, want empty — backspace during an edit must not touch the filter", v.filter)
	}
}

// TestConfigEscCancelsEditFirst covers Esc's priority order: with a filter
// already set and a string edit open, the first Esc cancels the edit only
// (filter untouched, panel stays open), and only a later Esc — once there is
// no edit and no filter left — bubbles to close the panel.
func TestConfigEscCancelsEditFirst(t *testing.T) {
	v := newConfigView(theme.Test(), GoldenCommandEnv())
	v = typeText(v, "model")
	v = pressKey(v, tea.KeyDown)
	v = pressKey(v, tea.KeyEnter) // opens the inline edit for the filtered row 0

	v, ok := v.handleEscape()
	if !ok {
		t.Fatalf("first Esc: consumed=false, want true (cancel the edit)")
	}
	if v.editing {
		t.Fatalf("first Esc: still editing, want the edit canceled")
	}
	if v.filter != "model" {
		t.Fatalf("first Esc: filter = %q, want unchanged %q", v.filter, "model")
	}

	v, ok = v.handleEscape()
	if !ok {
		t.Fatalf("second Esc: consumed=false, want true (clear the filter)")
	}
	if v.filter != "" {
		t.Fatalf("second Esc: filter = %q, want cleared", v.filter)
	}

	_, ok = v.handleEscape()
	if ok {
		t.Fatalf("third Esc: consumed=true, want false (bubble to close — nothing left to clear)")
	}
}

// TestConfigZeroCommandEnvDoesNotPanic covers the zero CommandEnv (nil
// Config/SaveConfig closures) rendering and accepting edits without
// panicking — the auth-independence contract every panel view honors.
func TestConfigZeroCommandEnvDoesNotPanic(t *testing.T) {
	v := newConfigView(theme.Test(), CommandEnv{})
	got := v.View(testkit.Width, testkit.Height)
	if !strings.Contains(got, "Search settings…") {
		t.Fatalf("expected the empty-filter placeholder, got:\n%s", got)
	}

	for i := 0; i < 4; i++ {
		v = pressKey(v, tea.KeyDown)
	}
	v = pressKey(v, tea.KeyEnter) // toggles telemetry.enabled in memory; SaveConfig is nil
	if !v.cfg.Telemetry.Enabled {
		t.Fatalf("expected the in-memory toggle to apply even with a nil SaveConfig")
	}
}

// TestConfigLoadErrorShowsAsErr covers a malformed on-disk config: newConfigView
// records env.Config's error rather than blocking the view from opening.
func TestConfigLoadErrorShowsAsErr(t *testing.T) {
	env := CommandEnv{Config: func() (config.Config, error) { return config.Config{}, errors.New("boom") }}
	v := newConfigView(theme.Test(), env)
	if v.err == "" {
		t.Fatalf("expected a Config load error to be recorded, got none")
	}
	got := v.View(testkit.Width, testkit.Height)
	if !strings.Contains(got, "Error: boom") {
		t.Fatalf("expected the error row rendered, got:\n%s", got)
	}
}

// TestConfigSaveErrorSurfacesAsRow covers a SaveConfig failure: the edit
// still applies in memory (never silently reverted) and the error renders as
// a trailing row instead of blocking further edits.
func TestConfigSaveErrorSurfacesAsRow(t *testing.T) {
	env := GoldenCommandEnv()
	env.SaveConfig = func(config.Config) error { return errors.New("disk full") }
	v := newConfigView(theme.Test(), env)
	for i := 0; i < 4; i++ {
		v = pressKey(v, tea.KeyDown)
	}
	v = pressKey(v, tea.KeyEnter)

	if !v.cfg.Telemetry.Enabled {
		t.Fatalf("expected the in-memory toggle to apply despite the save error")
	}
	got := v.View(testkit.Width, testkit.Height)
	if !strings.Contains(got, "Error: disk full") {
		t.Fatalf("expected the save error rendered as a row, got:\n%s", got)
	}
}

// TestConfigNoMatchLine covers a filter matching nothing: the list renders a
// "no match" line instead of an empty gap.
func TestConfigNoMatchLine(t *testing.T) {
	v := newConfigView(theme.Test(), GoldenCommandEnv())
	v = typeText(v, "zzz-nope")
	got := v.View(testkit.Width, testkit.Height)
	if !strings.Contains(got, "No settings match.") {
		t.Fatalf("expected the no-match line, got:\n%s", got)
	}
}

// TestColorConfigRowSelectedLayout covers the ANSI-width invariant
// (docs/TUI.md's "layer 3") for the one line configView colors — the
// AccentStyle-highlighted row: stripping ANSI from the colored render must
// reproduce the plain render exactly, and no colored line may exceed width
// display cells.
func TestColorConfigRowSelectedLayout(t *testing.T) {
	build := func(th theme.Theme) configView {
		v := newConfigView(th, GoldenCommandEnv())
		return pressKey(v, tea.KeyDown)
	}
	for _, width := range []int{80, 24} {
		t.Run(fmt.Sprintf("width=%d", width), func(t *testing.T) {
			plain := testkit.Render(build(theme.Test()), width, testkit.Height)
			colored := testkit.Render(build(testkit.ColorTheme()), width, testkit.Height)

			if stripped := ansi.Strip(colored); stripped != plain {
				t.Errorf("colored render stripped of ANSI != plain render (color changed layout)\n--- stripped ---\n%s\n--- plain ---\n%s", stripped, plain)
			}
			for i, line := range strings.Split(colored, "\n") {
				if w := ansi.StringWidth(line); w > width {
					t.Errorf("line %d exceeds width %d cells (got %d): %q", i, width, w, line)
				}
			}
		})
	}
}
