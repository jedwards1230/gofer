package tui_test

// help_app_test.go is /help's App-level half: the command and the `?` key both
// opening the panel on the Help tab, and the frame a user actually sees. The
// body's own content assertions live in help_test.go (white-box, because the
// live-registry claim is only provable from inside the package).

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
)

// TestGoldenPanelHelp captures /help's whole frame, the way /status and the
// rest of the tabs are captured.
func TestGoldenPanelHelp(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = dispatchSlash(t, m, "/help")
	testkit.AssertGolden(t, "app_panel_help", content(m))
}

// TestHelpQuestionMarkOpensHelpFromAnEmptyDispatchBar covers the roster
// footer's long-standing "? shortcuts" promise, which had nothing bound behind
// it until /help existed.
func TestHelpQuestionMarkOpensHelpFromAnEmptyDispatchBar(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = press(t, m, tea.KeyPressMsg{Text: "?"})

	if got := content(m); !strings.Contains(got, "[Help]") {
		t.Fatalf("expected ? to open the panel on the Help tab, got:\n%s", got)
	}
}

// TestHelpQuestionMarkWithTextIsALiteral is the other half of the conditional
// binding — the same rule the bare → follows. Once the dispatch bar has text,
// "?" is an ordinary character and must not hijack a prompt mid-sentence.
func TestHelpQuestionMarkWithTextIsALiteral(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = type_(t, m, "what now?")

	got := content(m)
	if strings.Contains(got, "[Help]") {
		t.Fatalf("? opened the help panel from a non-empty dispatch bar, got:\n%s", got)
	}
	if !strings.Contains(got, "what now?") {
		t.Fatalf("expected the literal ? in the dispatch bar, got:\n%s", got)
	}
}

// TestHelpTabIsReachableByTabbing pins that Help is a first-class panel tab
// rather than a special-cased view: ←/→ reach it from any other tab.
func TestHelpTabIsReachableByTabbing(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = dispatchSlash(t, m, "/status")
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyLeft}) // wraps backwards onto the last tab

	if got := content(m); !strings.Contains(got, "[Help]") {
		t.Fatalf("expected ← from the Status tab to wrap onto Help, got:\n%s", got)
	}
}
