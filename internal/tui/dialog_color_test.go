package tui

// dialog_color_test.go locks down the inline approval prompt under a real
// color profile — the case theme.Test's forced termenv.Ascii can never
// exercise, and therefore the case the checked-in *.golden files silently
// miss (see PR #61's display-width lesson: an Ascii-only golden can pass
// while a colored render overruns the frame). renderApprovalPrompt styles
// every line (WarnStyle), so with color on those lines carry ANSI escapes;
// Model.View/TailView must still lay the block out identically to the
// colorless render, with every composited line staying within the frame
// width.

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
// lipgloss actually emits ANSI. The rendered geometry must be identical to
// the colorless render — color changes styling, never layout.
func colorTestTheme() theme.Theme {
	th := theme.Test()
	th.Profile = termenv.TrueColor
	return th
}

// newColorAppWithApproval builds an App through th, drives it into the attach
// screen of the recency-first session, and feeds it a pending
// PermissionRequested — the state that renders the inline approval prompt
// above the attach screen's status/input lines.
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
	if !a.sess.HasPendingApproval() {
		t.Fatal("expected a pending approval after PermissionRequested")
	}
	return a
}

// TestColorApprovalDialogComposite is the inline prompt's version of the #61
// display-width lesson: the same App state is rendered twice — once
// colorless (theme.Test), once colored (colorTestTheme) — and the colored
// frame, stripped of ANSI, must equal the colorless frame exactly, with no
// line overrunning the terminal width.
func TestColorApprovalDialogComposite(t *testing.T) {
	plain := newColorAppWithApproval(t, theme.Test()).render()
	colored := newColorAppWithApproval(t, colorTestTheme()).render()

	if stripped := ansi.Strip(colored); stripped != plain {
		t.Errorf("colored approval-prompt frame, stripped of ANSI, != colorless frame\n"+
			"--- stripped ---\n%s\n--- plain ---\n%s", stripped, plain)
	}

	for i, line := range strings.Split(colored, "\n") {
		if w := ansi.StringWidth(line); w > testkit.Width {
			t.Errorf("composited line %d overruns width %d (got %d cells): %q", i, testkit.Width, w, line)
		}
	}

	// The prompt must survive compositing intact — the tool+args line, the
	// question, the action row, and the footer — not fragments scattered by a
	// display-width bug.
	stripped := ansi.Strip(colored)
	for _, want := range []string{
		"bash · cmd=rm -rf /tmp/x",
		"Allow this tool call?",
		"[a] allow   [d] deny   [r] remember: off",
		"esc cancel · session ",
	} {
		if !strings.Contains(stripped, want) {
			t.Errorf("composited frame missing inline approval prompt content %q:\n%s", want, stripped)
		}
	}
}
