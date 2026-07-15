package tui

// command_menu_test.go covers the [commandMenu] autocomplete popup
// component in isolation — filtering, row-highlight navigation, scrolling
// past [commandMenuMaxRows], Tab-completion's ArgHint trailing-space rule,
// and Enter-selection — against synthetic registries built directly in this
// file so the scrolling tests aren't limited to gofer's own three
// commands. The App-level wiring (trigger rule end to end through both
// input paths, dispatch precedence) is covered separately in
// command_menu_app_test.go (package tui_test). White-box (package tui)
// because commandMenu/Registry.matching are unexported.

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// noopRun is a [Command.Run] stub for synthetic test registries — the menu
// tests below never execute it (they assert on [commandMenu.selected],
// leaving the App-level Run coupling to command_menu_app_test.go).
func noopRun(a App, _ []string) (App, tea.Cmd) { return a, nil }

// wideRegistry returns a Registry with more commands than
// [commandMenuMaxRows] so the scrolling tests have something to scroll —
// gofer's own three builtins (command.go) aren't enough. Names are chosen to
// sort predictably (alpha0..alpha7) and one, "hiddenopt", is Hidden to prove
// it's excluded from the popup.
func wideRegistry() Registry {
	var r Registry
	for _, n := range []string{"alpha0", "alpha1", "alpha2", "alpha3", "alpha4", "alpha5", "alpha6", "alpha7"} {
		r.register(Command{Name: n, Summary: "summary for " + n, Run: noopRun})
	}
	r.register(Command{Name: "hiddenopt", Hidden: true, Summary: "should never appear", Run: noopRun})
	return r
}

func smallRegistry() Registry {
	var r Registry
	r.register(Command{Name: "status", Summary: "Show session, cwd, and provider status", Run: noopRun})
	r.register(Command{Name: "config", Aliases: []string{"cfg"}, Summary: "View and edit settings", Run: noopRun})
	r.register(Command{Name: "model", ArgHint: "[id]", Summary: "Pick the active/default model", Run: noopRun})
	return r
}

func TestCommandMenuClosedWithNoActiveToken(t *testing.T) {
	m := newCommandMenu(theme.Test(), smallRegistry(), "hello world", 11)
	if m.open() {
		t.Fatalf("expected the menu closed with no active token, got %d rows", len(m.rows))
	}
}

func TestCommandMenuClosedOnNoMatch(t *testing.T) {
	m := newCommandMenu(theme.Test(), smallRegistry(), "/zzz", 4)
	if m.open() {
		t.Fatalf("expected the menu closed when nothing matches, got %d rows", len(m.rows))
	}
}

func TestCommandMenuFiltersByPrefixAndExcludesHidden(t *testing.T) {
	m := newCommandMenu(theme.Test(), wideRegistry(), "/alpha0", 7)
	if len(m.rows) != 1 || m.rows[0].Name != "alpha0" {
		t.Fatalf("expected exactly alpha0 to match, got %v", m.rows)
	}

	all := newCommandMenu(theme.Test(), wideRegistry(), "/", 1)
	if len(all.rows) != 8 {
		t.Fatalf("expected the bare slash to match every non-Hidden command (8), got %d: %v", len(all.rows), all.rows)
	}
	for _, cmd := range all.rows {
		if cmd.Hidden {
			t.Fatalf("expected Hidden commands excluded from the popup, got %q", cmd.Name)
		}
	}
}

func TestCommandMenuMatchesAlias(t *testing.T) {
	m := newCommandMenu(theme.Test(), smallRegistry(), "/cfg", 4)
	if len(m.rows) != 1 || m.rows[0].Name != "config" {
		t.Fatalf("expected the alias prefix to match /config, got %v", m.rows)
	}
}

func TestCommandMenuUpDownClampAtBounds(t *testing.T) {
	m := newCommandMenu(theme.Test(), smallRegistry(), "/", 1)
	if m.cursor != 0 {
		t.Fatalf("expected the first match highlighted on open, got cursor=%d", m.cursor)
	}

	for i := 0; i < len(m.rows)+3; i++ {
		m = m.moveDown()
	}
	if m.cursor != len(m.rows)-1 {
		t.Fatalf("expected ↓ to clamp at the last row (%d), got cursor=%d", len(m.rows)-1, m.cursor)
	}

	for i := 0; i < len(m.rows)+3; i++ {
		m = m.moveUp()
	}
	if m.cursor != 0 {
		t.Fatalf("expected ↑ to clamp at row 0, got cursor=%d", m.cursor)
	}
}

// TestCommandMenuCompleteAppendsSpaceForArgHint covers the Tab-accept rule:
// a command with an ArgHint gets a trailing space (ready for an argument), a
// command without one does not (ready to submit).
func TestCommandMenuCompleteAppendsSpaceForArgHint(t *testing.T) {
	m := newCommandMenu(theme.Test(), smallRegistry(), "/mo", 3)
	if len(m.rows) != 1 || m.rows[0].Name != "model" {
		t.Fatalf("expected /model to match /mo, got %v", m.rows)
	}
	got, ok := m.complete("/mo", 3)
	if !ok {
		t.Fatal("expected complete to succeed with a highlighted row")
	}
	if want := "/model "; got != want {
		t.Fatalf("complete(%q) = %q, want %q (trailing space — model has an ArgHint)", "/mo", got, want)
	}
}

func TestCommandMenuCompleteNoTrailingSpaceWithoutArgHint(t *testing.T) {
	m := newCommandMenu(theme.Test(), smallRegistry(), "/conf", 5)
	if len(m.rows) != 1 || m.rows[0].Name != "config" {
		t.Fatalf("expected /config to match /conf, got %v", m.rows)
	}
	got, ok := m.complete("/conf", 5)
	if !ok {
		t.Fatal("expected complete to succeed with a highlighted row")
	}
	if want := "/config"; got != want {
		t.Fatalf("complete(%q) = %q, want %q (no trailing space — config has no ArgHint)", "/conf", got, want)
	}
}

// TestCommandMenuCompleteReplacesWholeToken covers completion mid-buffer:
// the token starting at m.start (the "/") through cursor is replaced,
// leaving any text after the cursor untouched.
func TestCommandMenuCompleteReplacesWholeToken(t *testing.T) {
	m := newCommandMenu(theme.Test(), smallRegistry(), "/sta", 4)
	got, ok := m.complete("/sta", 4)
	if !ok {
		t.Fatal("expected complete to succeed")
	}
	if want := "/status"; got != want {
		t.Fatalf("complete(%q) = %q, want %q", "/sta", got, want)
	}
}

func TestCommandMenuCompleteClosedIsNoOp(t *testing.T) {
	var m commandMenu
	got, ok := m.complete("/xyz", 4)
	if ok {
		t.Fatal("expected complete on a closed menu to report ok=false")
	}
	if got != "/xyz" {
		t.Fatalf("expected the buffer unchanged, got %q", got)
	}
}

func TestCommandMenuSelected(t *testing.T) {
	m := newCommandMenu(theme.Test(), smallRegistry(), "/sta", 4)
	cmd, ok := m.selected()
	if !ok || cmd.Name != "status" {
		t.Fatalf("expected /status highlighted, got %v ok=%v", cmd, ok)
	}
}

// TestGoldenCommandMenuFiltered covers the popup's basic filtered-list
// render: "/c" narrows gofer's real registry to just /config (its Aliases
// don't collide with /model or /status).
func TestGoldenCommandMenuFiltered(t *testing.T) {
	m := newCommandMenu(theme.Test(), smallRegistry(), "/c", 2)
	got := strings.Join(m.Lines(testkit.Width), "\n")
	testkit.AssertGolden(t, "command_menu_filtered", got)
}

func TestGoldenCommandMenuFilteredStyled(t *testing.T) {
	m := newCommandMenu(testkit.ColorTheme(), smallRegistry(), "/c", 2)
	got := strings.Join(m.Lines(testkit.Width), "\n")
	testkit.AssertGoldenStyled(t, "command_menu_filtered", got)
}

// TestGoldenCommandMenuScrolled covers the scroll-affordance rendering: 8
// matches against a 6-row max window, highlighted row moved down past the
// fold (row index 6 of 8) so both the "↑ N more" and "↓ N more" markers
// show simultaneously.
func TestGoldenCommandMenuScrolled(t *testing.T) {
	m := newCommandMenu(theme.Test(), wideRegistry(), "/alpha", 6)
	for i := 0; i < 6; i++ {
		m = m.moveDown()
	}
	got := strings.Join(m.Lines(testkit.Width), "\n")
	testkit.AssertGolden(t, "command_menu_scrolled", got)
}

func TestGoldenCommandMenuScrolledStyled(t *testing.T) {
	m := newCommandMenu(testkit.ColorTheme(), wideRegistry(), "/alpha", 6)
	for i := 0; i < 6; i++ {
		m = m.moveDown()
	}
	got := strings.Join(m.Lines(testkit.Width), "\n")
	testkit.AssertGoldenStyled(t, "command_menu_scrolled", got)
}

func TestCommandMenuClosedLinesIsNil(t *testing.T) {
	var m commandMenu
	if lines := m.Lines(testkit.Width); lines != nil {
		t.Fatalf("expected a closed menu to render no lines, got %v", lines)
	}
}
