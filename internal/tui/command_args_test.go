package tui

// command_args_test.go holds the GENERAL guard behind issue #165: a command
// that advertises an argument in the UI must actually consume it.
//
// The defect it exists to catch is structural, not specific to /model.
// [Command.ArgHint] is rendered in the autocomplete popup, and the popup
// appends a trailing space on accept precisely because the command declares an
// argument (command_menu.go) — the UI invites the argument and positions the
// cursor for it. A Run wired to [openPanel] (or any other
// `func(App, _ []string)`) then throws it away with no error at all. So the
// assertion below iterates [newBuiltinRegistry] rather than naming /model: any
// FUTURE command that gains an ArgHint and discards its args fails here
// without anyone remembering to extend this file.
//
// White-box (package tui) because it needs newBuiltinRegistry and the App
// fields — a.panel, a.status — that make "did the argument reach anything?"
// observable.

import (
	"strings"
	"testing"

	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// unroutableModelID is an id no provider adapter can route, so feeding it to
// an arg-consuming command produces a visible complaint without ever reaching
// a config write, a daemon call, or a vendor request. It doubles as the
// sentinel for "did this argument reach anything at all?".
const unroutableModelID = "no-such-vendor/no-such-model-165"

// commandOutcome is the observable result of running one Command: whether it
// opened the command panel, and the status note (plus severity) it left
// behind. It is deliberately coarse — it captures what a USER can see, which
// is exactly the surface an args-discarding Run fails to vary.
type commandOutcome struct {
	panelOpen bool
	status    string
	sev       statusSeverity
}

// runCommandOutcome runs cmd's Run against a fresh App and reports what it
// changed. The returned tea.Cmd is intentionally NOT executed: the assertion
// is about the argument reaching the command's own logic, and running the
// follow-on would drag in config writes and vendor requests this test has no
// business making.
func runCommandOutcome(t *testing.T, cmd Command, args []string) commandOutcome {
	t.Helper()
	a := NewApp(theme.Test(), newInternalFakeSup(GoldenRoster()), GoldenMeta(), GoldenCommandEnv())
	got, _ := cmd.Run(a, args)
	return commandOutcome{panelOpen: got.panel != nil, status: got.status, sev: got.statusSev}
}

// TestArgHintCommandsConsumeArgs asserts that every registered command
// declaring a non-empty ArgHint behaves DIFFERENTLY with an argument than
// without one. A Run that discards its args returns byte-identical outcomes
// for both and fails.
//
// This is the regression guard for issue #165, where /model declared
// ArgHint "[id]" but was wired to openPanel — `/model <id>` opened the picker
// and silently dropped the id.
func TestArgHintCommandsConsumeArgs(t *testing.T) {
	cmds := newBuiltinRegistry().List()
	checked := 0
	for _, cmd := range cmds {
		if cmd.ArgHint == "" {
			continue // /status, /config: discarding args is correct for them
		}
		checked++
		t.Run(cmd.Name, func(t *testing.T) {
			bare := runCommandOutcome(t, cmd, nil)
			withArg := runCommandOutcome(t, cmd, []string{unroutableModelID})
			if bare == withArg {
				t.Errorf("/%s declares ArgHint %q but produced an identical outcome "+
					"with and without an argument (%+v) — its Run discards args "+
					"while the UI advertises and invites one (issue #165)",
					cmd.Name, cmd.ArgHint, bare)
			}
			// An unroutable argument must complain, not open the picker: a
			// silently-opened panel is the exact failure #165 describes.
			if withArg.panelOpen {
				t.Errorf("/%s %s opened the command panel; an unusable argument must be reported, not swallowed",
					cmd.Name, unroutableModelID)
			}
			if withArg.sev != sevDanger || withArg.status == "" {
				t.Errorf("/%s %s left status %q (severity %v); want a non-empty sevDanger note naming the problem",
					cmd.Name, unroutableModelID, withArg.status, withArg.sev)
			}
			if !strings.Contains(withArg.status, unroutableModelID) {
				t.Errorf("/%s %s status %q does not name the offending id", cmd.Name, unroutableModelID, withArg.status)
			}
		})
	}
	if checked == 0 {
		// Guards the loop itself: if List() ever returns nothing, or every
		// ArgHint is dropped, the test above would vacuously pass.
		t.Fatal("no registered command declares an ArgHint — this test asserted nothing")
	}
}

// TestArgHintOutcomeComparisonDiscriminates is the must-fire twin for
// TestArgHintCommandsConsumeArgs: it registers a command that declares an
// ArgHint and wires the args-discarding openPanel — precisely the #165 defect
// — and asserts the comparison above DOES see the two outcomes as identical.
//
// Without this, a comparison that silently stopped discriminating (a field
// dropped from commandOutcome, an outcome that always differs for unrelated
// reasons) would leave the real test passing while guarding nothing.
func TestArgHintOutcomeComparisonDiscriminates(t *testing.T) {
	defective := Command{
		Name:    "defective",
		ArgHint: "[id]",
		Summary: "a command that advertises an argument and throws it away",
		Run:     openPanel(panelStatus),
	}
	bare := runCommandOutcome(t, defective, nil)
	withArg := runCommandOutcome(t, defective, []string{unroutableModelID})
	if bare != withArg {
		t.Fatalf("the args-discarding control produced DIFFERENT outcomes (%+v vs %+v); "+
			"commandOutcome is varying on something other than the argument, so "+
			"TestArgHintCommandsConsumeArgs would pass on a defective command",
			bare, withArg)
	}
}
