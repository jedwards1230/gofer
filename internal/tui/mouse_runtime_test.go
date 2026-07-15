package tui_test

// mouse_runtime_test.go empirically proves (or disproves) that the mouse
// enable sequence App.View sets (see app.go's View doc) actually reaches a
// real bubbletea Program's output, not just the tea.View struct App hands
// back. It runs a genuine tea.Program end to end — but entirely in memory
// (tea.WithInput/tea.WithOutput over bytes buffers, never a real terminal
// device) — so it stays inside docs/TESTING.md's "never test through a PTY"
// rule while still exercising the runtime path a PTY-based teatest probe
// would, just without allocating one.

import (
	"bytes"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// TestProgramEmitsMouseEnableSequence drives a real tea.Program (in-memory
// input/output, no PTY) through its first real frame and asserts the raw
// output bytes contain the mouse button-event (1002) and SGR extended
// (1006) enable sequences App.View's MouseModeCellMotion requests. bubbletea
// v2's cursedRenderer writes these on the very first frame it flushes (see
// cursed_renderer.go's flush, both the "s.lastView == nil" branch and the
// steady-state "view.MouseMode != s.lastView.MouseMode" branch cover it),
// so any message reaching Update-then-render before Quit is enough to prove
// the enable sequence round-trips out of the real Program, not just out of
// App.View() in isolation (already covered by TestViewEnablesMouseMode).
func TestProgramEmitsMouseEnableSequence(t *testing.T) {
	sup := newFakeSup(nil)
	app := tui.NewApp(theme.Test(), sup, tui.GoldenMeta(), tui.GoldenCommandEnv())

	var out bytes.Buffer
	p := tea.NewProgram(app,
		tea.WithInput(strings.NewReader("")),
		tea.WithOutput(&out),
		tea.WithWindowSize(80, 24),
		tea.WithoutSignalHandler(),
		tea.WithoutCatchPanics(),
	)

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = p.Run()
	}()

	// A message we send ourselves is only enqueued once the Program's
	// eventLoop is actively receiving (p.msgs is unbuffered — see tea.go),
	// and Send blocks (program order, single goroutine here) until that
	// happens. Every processed message triggers a render (tea.go's
	// eventLoop, the unconditional p.render(model) after model.Update), so
	// this guarantees at least one real frame — carrying App.View's
	// MouseModeCellMotion — flushes to out before we Quit.
	p.Send(tea.WindowSizeMsg{Width: 80, Height: 24})
	p.Quit()

	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("tea.Program did not exit after Quit; possible deadlock")
	}

	got := out.String()
	const (
		mouseButtonEvent = "\x1b[?1002h" // ansi.SetModeMouseButtonEvent
		mouseExtSGR      = "\x1b[?1006h" // ansi.SetModeMouseExtSgr
	)
	if !strings.Contains(got, mouseButtonEvent) {
		t.Errorf("program output missing mouse button-event enable sequence %q; mouse reporting was never turned on.\noutput: %q", mouseButtonEvent, got)
	}
	if !strings.Contains(got, mouseExtSGR) {
		t.Errorf("program output missing SGR extended mouse-mode enable sequence %q; wheel events would arrive in the legacy (unreliable past col/row 223) encoding, or not enabled at all.\noutput: %q", mouseExtSGR, got)
	}
}
