package tui_test

// command_test.go covers the slash dispatcher + command panel host (M4 step
// 1): the parse intercept on both submit paths, the panel's overlay routing
// (Esc closes it, ahead of the per-screen handlers), and the unknown-command
// status line. It reuses app_test.go's fake Supervisor and key-press helpers
// — the dispatcher is exercised entirely through App's exported Update/View
// surface, no internal access needed.

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// dispatchSlash types cmd into the overview dispatch bar and presses enter,
// returning the resulting model.
func dispatchSlash(t *testing.T, m tea.Model, cmd string) tea.Model {
	t.Helper()
	m = type_(t, m, cmd)
	return press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
}

// TestGoldenPanelStatus verifies /status opens the command panel on the
// Status tab.
func TestGoldenPanelStatus(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = dispatchSlash(t, m, "/status")
	testkit.AssertGolden(t, "app_panel_status", content(m))
}

// TestStatusReflectsAttachedSession verifies the end-to-end wiring from App
// into the panel (openPanel capturing [App.currentSessionInfo] at open
// time, command.go): /status opened after attaching to a session shows that
// session's name and id, not the overview's "—" (see status_test.go's
// statusView-level goldens for the field mapping itself).
func TestStatusReflectsAttachedSession(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach the selected (recency-first) session

	m = dispatchSlash(t, m, "/status")

	got := content(m)
	if !strings.Contains(got, "Session: wire the app root") {
		t.Errorf("expected the attached session's title, got:\n%s", got)
	}
	if !strings.Contains(got, "Session ID: 0192a1b2-app0-7000-8000-000000000001") {
		t.Errorf("expected the attached session's id, got:\n%s", got)
	}
}

// TestGoldenPanelConfig verifies /config opens the command panel on the
// Config tab.
func TestGoldenPanelConfig(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = dispatchSlash(t, m, "/config")
	testkit.AssertGolden(t, "app_panel_config", content(m))
}

// TestGoldenPanelModel verifies /model opens the command panel on the Model
// tab.
func TestGoldenPanelModel(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = dispatchSlash(t, m, "/model")
	testkit.AssertGolden(t, "app_panel_model", content(m))
}

// TestPanelArrowsSwitchTabs verifies → moves the active tab while the panel
// is open.
func TestPanelArrowsSwitchTabs(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = dispatchSlash(t, m, "/status")

	if got := content(m); !strings.Contains(got, "[Status]") {
		t.Fatalf("expected the Status tab active after /status, got:\n%s", got)
	}

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight})

	if got := content(m); !strings.Contains(got, "[Config]") {
		t.Fatalf("expected → to move to the Config tab, got:\n%s", got)
	}
}

// TestPanelEscCloses verifies Esc closes the panel and returns to the
// overview underneath it, unchanged.
func TestPanelEscCloses(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	before := content(m)

	m = dispatchSlash(t, m, "/status")
	if got := content(m); !strings.Contains(got, "Status") || got == before {
		t.Fatalf("expected the panel open after /status, got:\n%s", got)
	}

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})

	if got := content(m); got != before {
		t.Fatalf("expected esc to close the panel and restore the overview;\ngot:\n%s\nwant:\n%s", got, before)
	}
}

// TestSlashUnknownCommandSetsStatus verifies an unrecognized command sets the
// transient status line instead of dispatching a prompt or opening the
// panel.
func TestSlashUnknownCommandSetsStatus(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := newTestApp(t, sup)
	m = dispatchSlash(t, m, "/nope")

	if got := content(m); !strings.Contains(got, "unknown command: /nope") {
		t.Fatalf("expected the unknown-command status line, got:\n%s", got)
	}
	if len(sup.created) != 0 {
		t.Errorf("sup.created = %v; want none — an unknown command must not dispatch a prompt", sup.created)
	}
}

// TestSlashFromAttachOpensPanel verifies the attach input's submit path
// intercepts a slash command the same way the overview dispatch bar does.
func TestSlashFromAttachOpensPanel(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := newTestApp(t, sup)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach the selected session

	m = type_(t, m, "/config")
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if got := content(m); !strings.Contains(got, "[Config]") {
		t.Fatalf("expected /config from the attach input to open the panel, got:\n%s", got)
	}
	if len(sup.sent) != 0 {
		t.Errorf("sup.sent = %v; want none — a slash command must not be sent as a prompt", sup.sent)
	}
}

// TestPanelConfigEscClearsFilterThenCloses verifies the Config tab's
// two-stage Esc contract end to end through App's exported surface: a first
// Esc with a filter typed clears it and leaves the panel open, and only a
// second Esc — filter now empty — closes the panel back to the overview
// underneath it.
func TestPanelConfigEscClearsFilterThenCloses(t *testing.T) {
	before := content(newTestApp(t, newFakeSup(tui.GoldenRoster())))

	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = dispatchSlash(t, m, "/config")
	m = type_(t, m, "model")

	if got := content(m); !strings.Contains(got, "Search: model") {
		t.Fatalf("expected the typed filter rendered, got:\n%s", got)
	}

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if got := content(m); !strings.Contains(got, "[Config]") || strings.Contains(got, "Search: model") {
		t.Fatalf("expected the first Esc to clear the filter but keep the panel open, got:\n%s", got)
	}

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if got := content(m); got != before {
		t.Fatalf("expected the second Esc to close the panel and restore the overview;\ngot:\n%s\nwant:\n%s", got, before)
	}
}

// TestPanelConfigEditPersistsViaSaveConfig verifies the end-to-end wiring
// from a committed Config-tab edit through to [CommandEnv.SaveConfig]: this
// builds App directly (rather than through newTestApp/GoldenCommandEnv) so
// the test can supply its own SaveConfig closure and observe what was
// written.
func TestPanelConfigEditPersistsViaSaveConfig(t *testing.T) {
	var saved []config.Config
	env := tui.GoldenCommandEnv()
	env.SaveConfig = func(c config.Config) error {
		saved = append(saved, c)
		return nil
	}

	var m tea.Model = tui.NewApp(theme.Test(), newFakeSup(tui.GoldenRoster()), tui.GoldenMeta(), env)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m, _ = m.Update(m.Init()())

	m = dispatchSlash(t, m, "/config")
	// session.model, session.permission_mode, tui.roster_view,
	// tui.autoscroll, tui.mouse, telemetry.enabled — six ↓ presses (the first
	// selects row 0) land on telemetry.enabled, a bool row that saves on
	// Enter with no further input needed.
	for i := 0; i < 6; i++ {
		m = press(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	}
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if len(saved) != 1 {
		t.Fatalf("SaveConfig called %d times, want 1", len(saved))
	}
	if !saved[0].Telemetry.Enabled {
		t.Fatalf("saved Telemetry.Enabled = false, want true")
	}
	if got := content(m); !strings.Contains(got, "Telemetry enabled … true") {
		t.Fatalf("expected the toggled row rendered, got:\n%s", got)
	}
}
