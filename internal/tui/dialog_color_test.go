package tui

// dialog_color_test.go locks down the approval-modal compositor under a real
// color profile — the case theme.Test's forced termenv.Ascii can never
// exercise, and therefore the case the checked-in *.golden files silently
// miss. renderApprovalDialog styles the box (WarnStyle), so with color on its
// lines carry ANSI escapes; overlayCenter must composite by display column,
// never by rune offset, or those escape bytes splice into the underlying
// frame as stray cells scattered across the screen (the "torn modal / orphan
// │ fragments" defect).

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// colorTestTheme is theme.Test with a real color profile forced on, so
// lipgloss actually emits ANSI. The composited geometry must be identical to
// the colorless render — color changes styling, never layout.
func colorTestTheme() theme.Theme {
	th := theme.Test()
	th.Profile = termenv.TrueColor
	return th
}

// newColorAppWithApproval builds an App through th, drives it into the attach
// screen of the recency-first session, and feeds it a pending
// PermissionRequested — the state that renders the approval modal over the
// attach frame. It mirrors attachForDialogTest but lets the caller pick the
// theme (attachForDialogTest hardcodes theme.Test via newAppForGolden).
func newColorAppWithApproval(t *testing.T, th theme.Theme) App {
	t.Helper()
	sup := newInternalFakeSup(appGoldenRoster())
	a := NewApp(th, sup, appGoldenMeta())

	mdl, _ := a.Update(tea.WindowSizeMsg{Width: testkit.Width, Height: testkit.Height})
	a = mdl.(App)
	mdl, _ = a.Update(rosterMsg{sessions: appGoldenRoster()})
	a = mdl.(App)

	mdl, cmd := a.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	a = mdl.(App)
	if cmd == nil {
		t.Fatal("expected a subscribe cmd after entering attach")
	}
	mdl, _ = a.Update(cmd())
	a = mdl.(App)

	mdl, _ = a.Update(sessEventMsg{
		id: a.sessID,
		ev: event.NewPermissionRequested(a.sessID, "perm-1", "bash",
			map[string]any{"cmd": "rm -rf /tmp/x"}, []string{"no rule"}),
	})
	a = mdl.(App)
	if a.dialog == nil {
		t.Fatal("expected a.dialog set after PermissionRequested")
	}
	return a
}

// TestColorApprovalDialogComposite is the direct reproduction of the
// permission-dialog scatter defect. The same App state is rendered twice —
// once colorless (theme.Test), once colored (colorTestTheme) — and the
// composited colored frame, stripped of ANSI, must equal the colorless frame
// exactly, with no line overrunning the terminal width. Under the pre-fix
// rune-splicing overlayCenter the colored frame's box lands at the wrong
// column with escape fragments strewn across the base, so the stripped frame
// does not match.
func TestColorApprovalDialogComposite(t *testing.T) {
	plain := newColorAppWithApproval(t, theme.Test()).render()
	colored := newColorAppWithApproval(t, colorTestTheme()).render()

	if stripped := ansi.Strip(colored); stripped != plain {
		t.Errorf("composited dialog frame, color-stripped, != colorless frame\n"+
			"(overlayCenter changed geometry under color)\n--- stripped ---\n%s\n--- plain ---\n%s",
			stripped, plain)
	}

	for i, line := range strings.Split(colored, "\n") {
		if w := ansi.StringWidth(line); w > testkit.Width {
			t.Errorf("composited line %d overruns width %d (got %d cells): %q", i, testkit.Width, w, line)
		}
	}

	// The border and title must survive compositing as intact, contiguous
	// runs — not the scattered fragments the bug produced.
	stripped := ansi.Strip(colored)
	if !strings.Contains(stripped, "┌──────────") || !strings.Contains(stripped, "Permission requested") {
		t.Errorf("composited frame missing an intact dialog border/title:\n%s", stripped)
	}
}
