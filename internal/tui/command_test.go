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

	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
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
