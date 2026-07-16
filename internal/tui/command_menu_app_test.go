package tui_test

// command_menu_app_test.go covers the slash-command autocomplete popup
// (command_menu.go) wired end to end through App's exported surface — both
// text-entry paths it applies to (the overview dispatch bar and the attach
// input), the trigger rule's literal-slash exception, Tab/Enter/Esc's accept
// semantics, and dispatch precedence ahead of the per-screen handlers. The
// component's own filtering/scrolling/completion logic is covered in
// isolation in command_menu_test.go (package tui, with synthetic
// registries wide enough to scroll — gofer's own three commands aren't).

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// TestGoldenMenuOverviewFiltered covers the popup composed above the
// overview dispatch bar's rule: "/c" narrows gofer's registry to /config.
func TestGoldenMenuOverviewFiltered(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = type_(t, m, "/c")
	testkit.AssertGolden(t, "app_menu_overview_filtered", content(m))
}

func TestGoldenMenuOverviewFilteredStyled(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	var m tea.Model = tui.NewApp(testkit.ColorTheme(), sup, tui.GoldenMeta(), tui.GoldenCommandEnv())
	m, _ = m.Update(tea.WindowSizeMsg{Width: testkit.Width, Height: testkit.Height})
	m, _ = m.Update(m.Init()())
	m = type_(t, m, "/c")
	testkit.AssertGoldenStyled(t, "app_menu_overview_filtered", content(m))
}

// TestGoldenMenuAttachFiltered covers the same popup composed above the
// attach input's rule.
func TestGoldenMenuAttachFiltered(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach the selected session
	m = type_(t, m, "/c")
	testkit.AssertGolden(t, "app_menu_attach_filtered", content(m))
}

func TestGoldenMenuAttachFilteredStyled(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	var m tea.Model = tui.NewApp(testkit.ColorTheme(), sup, tui.GoldenMeta(), tui.GoldenCommandEnv())
	m, _ = m.Update(tea.WindowSizeMsg{Width: testkit.Width, Height: testkit.Height})
	m, _ = m.Update(m.Init()())
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
	m = type_(t, m, "/c")
	testkit.AssertGoldenStyled(t, "app_menu_attach_filtered", content(m))
}

// TestMenuLiteralSlashDoesNotOpen covers the trigger rule's literal
// exception: a "/" preceded by a non-space rune (mid-word, "foo/bar") never
// opens the popup — the dispatch bar renders exactly as plain typed text,
// with no popup rows or scroll affordance above it.
func TestMenuLiteralSlashDoesNotOpen(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = type_(t, m, "foo/bar")

	got := content(m)
	if !strings.Contains(got, "❯ foo/bar") {
		t.Fatalf("expected the literal text typed into the dispatch bar, got:\n%s", got)
	}
	if strings.Contains(got, "▸ /") {
		t.Fatalf("expected no popup row for a literal mid-word slash, got:\n%s", got)
	}
}

// TestMenuBacktickSlashDoesNotOpen covers the other literal case named in
// the trigger rule: a "/" immediately preceded by a backtick.
func TestMenuBacktickSlashDoesNotOpen(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = type_(t, m, "`/x")

	got := content(m)
	if strings.Contains(got, "▸ /") {
		t.Fatalf("expected no popup row for a backtick-preceded slash, got:\n%s", got)
	}
}

// TestMenuOpensAfterSpace covers the other half of the trigger rule: a "/"
// preceded by whitespace (not just buffer start) still opens the popup.
func TestMenuOpensAfterSpace(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = type_(t, m, "hello /c")

	got := content(m)
	if !strings.Contains(got, "/config") {
		t.Fatalf("expected the popup open with /config matching \"/c\" after a space, got:\n%s", got)
	}
}

// TestMenuTabCompletesWithArgHintTrailingSpace covers the Tab-accept rule
// for a command that takes an argument: /model gets a trailing space, ready
// to type an id.
func TestMenuTabCompletesWithArgHintTrailingSpace(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = type_(t, m, "/mo")
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyTab})

	if got := content(m); !strings.Contains(got, "❯ /model ▏") {
		t.Fatalf("expected Tab to complete to \"/model \" with a trailing space (ArgHint), got:\n%s", got)
	}
}

// TestMenuTabCompletesWithoutArgHintNoTrailingSpace covers the other half:
// /config carries no ArgHint, so Tab completes it with no trailing space.
func TestMenuTabCompletesWithoutArgHintNoTrailingSpace(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = type_(t, m, "/conf")
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyTab})

	if got := content(m); !strings.Contains(got, "❯ /config▏") {
		t.Fatalf("expected Tab to complete to \"/config\" with no trailing space (no ArgHint), got:\n%s", got)
	}
}

// TestMenuEscClosesKeepingText covers Esc's contract: it closes the popup
// but leaves the typed text in the buffer, unlike the overview's own Esc
// (which clears the whole input when the menu is closed).
func TestMenuEscClosesKeepingText(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = type_(t, m, "/conf")

	if got := content(m); !strings.Contains(got, "▸") {
		t.Fatalf("expected the popup open before Esc, got:\n%s", got)
	}

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})

	got := content(m)
	if strings.Contains(got, "▸ /") {
		t.Fatalf("expected Esc to close the popup, got:\n%s", got)
	}
	if !strings.Contains(got, "❯ /conf▏") {
		t.Fatalf("expected Esc to keep the typed text, got:\n%s", got)
	}
}

// TestMenuEnterRunsHighlightedCommand covers Enter's accept semantics: with
// the popup open on a single match, Enter runs it directly (opening the
// command panel) rather than falling through to the dispatch-bar's own
// Enter handling.
func TestMenuEnterRunsHighlightedCommand(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = type_(t, m, "/mo")
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if got := content(m); !strings.Contains(got, "[Model]") {
		t.Fatalf("expected Enter on the open popup to open the panel on the Model tab, got:\n%s", got)
	}
}

// TestMenuUpDownMovesHighlight covers ↓/↑ moving the highlighted row across
// every builtin command (the bare "/" matches all three, Name-sorted:
// config, model, status).
func TestMenuUpDownMovesHighlight(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = type_(t, m, "/")

	if got := content(m); !strings.Contains(got, "▸ /config") {
		t.Fatalf("expected /config highlighted first (Name-sorted), got:\n%s", got)
	}

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if got := content(m); !strings.Contains(got, "▸ /model") {
		t.Fatalf("expected ↓ to move the highlight to /model, got:\n%s", got)
	}

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if got := content(m); !strings.Contains(got, "▸ /status") {
		t.Fatalf("expected a second ↓ to move the highlight to /status, got:\n%s", got)
	}

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if got := content(m); !strings.Contains(got, "▸ /model") {
		t.Fatalf("expected ↑ to move the highlight back to /model, got:\n%s", got)
	}
}

// TestMenuTabWhileOpenDoesNotToggleRosterView covers dispatch precedence:
// Tab toggles the roster's flat/grouped view when the dispatch bar has no
// active command token (the normal binding, handleOverviewKey), but is
// claimed by the open menu instead — GoldenRoster's two sessions share a
// cwd, so the flat view's cwd header ("~/orchestration") stays put instead
// of flipping to the grouped view's "Working"/"Needs input" section
// headers.
func TestMenuTabWhileOpenDoesNotToggleRosterView(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = type_(t, m, "/mo")
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyTab}) // completes the popup selection, does NOT toggle the view

	got := content(m)
	if !strings.Contains(got, "~/orchestration") {
		t.Fatalf("expected the flat view's cwd header untouched by the menu-consumed Tab, got:\n%s", got)
	}
	// The grouped view's section header is a standalone line containing only
	// the status word (overview_render.go's rows()); the flat view's row text
	// merely mentions "Needs input" inline as part of a longer summary
	// column, so check for the standalone line specifically, not a
	// substring match.
	for _, line := range strings.Split(got, "\n") {
		if strings.TrimSpace(line) == "Needs input" {
			t.Fatalf("expected the roster to stay in the flat view (Tab claimed by the menu), got:\n%s", got)
		}
	}
}

// TestMenuClosesOnceInputEmpty covers a corollary of the trigger rule: once
// the buffer is fully cleared (Backspace to empty), the popup closes.
func TestMenuClosesOnceInputEmpty(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = type_(t, m, "/c")
	if got := content(m); !strings.Contains(got, "▸") {
		t.Fatalf("expected the popup open, got:\n%s", got)
	}

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyBackspace})

	if got := content(m); strings.Contains(got, "▸ /") {
		t.Fatalf("expected the popup closed once the buffer is empty, got:\n%s", got)
	}
}

// menuOpenApp builds an App through th, sized to width/testkit.Height, with
// "/c" typed into the dispatch bar — the popup-open state
// [TestColorMenuMatchesLayoutAcrossWidths] renders plain and colored.
func menuOpenApp(t *testing.T, th theme.Theme, width int) tea.Model {
	t.Helper()
	var m tea.Model = tui.NewApp(th, newFakeSup(tui.GoldenRoster()), tui.GoldenMeta(), tui.GoldenCommandEnv())
	m, _ = m.Update(tea.WindowSizeMsg{Width: width, Height: testkit.Height})
	m, _ = m.Update(m.Init()())
	return type_(t, m, "/c")
}

// TestColorMenuMatchesLayoutAcrossWidths is the popup's version of the #61
// display-width lesson (color_layout_test.go): the same App state — the
// popup open above the dispatch bar's rule — is rendered once plain, once
// colored, at a normal and a narrow width, and the colored render (stripped
// of ANSI) must reproduce the plain one exactly, with no line overrunning
// the frame.
func TestColorMenuMatchesLayoutAcrossWidths(t *testing.T) {
	for _, width := range []int{80, 24} {
		t.Run(fmt.Sprintf("width=%d", width), func(t *testing.T) {
			plain := content(menuOpenApp(t, theme.Test(), width))
			colored := content(menuOpenApp(t, testkit.ColorTheme(), width))
			assertColorLayout(t, plain, colored, width)
		})
	}
}

// TestMenuAppliesToBothInputPaths is a smoke test that the same popup opens
// identically from the attach input as it does the dispatch bar — the two
// text-entry surfaces the trigger rule covers (docs/TUI.md).
func TestMenuAppliesToBothInputPaths(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach
	m = type_(t, m, "/mo")

	if got := content(m); !strings.Contains(got, "▸ /model") {
		t.Fatalf("expected the popup open from the attach input, got:\n%s", got)
	}

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := content(m); !strings.Contains(got, "[Model]") {
		t.Fatalf("expected Enter from the attach input's popup to open the panel, got:\n%s", got)
	}
}
