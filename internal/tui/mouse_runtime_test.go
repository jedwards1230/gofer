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
	"encoding/base64"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// syncBuffer is a mutex-guarded bytes.Buffer: the OSC 52 tests below need to
// poll the program's output WHILE it's still running (a Cmd like
// tea.SetClipboard executes on its own goroutine and round-trips back
// through the event loop, so there's no single synchronous point at which
// the write is guaranteed to have landed — see TestProgramCopiesSelectionViaOSC52's
// doc), and a plain bytes.Buffer isn't safe for concurrent
// read-while-write (the eventLoop goroutine is still writing to it).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitForOutput polls get (a *syncBuffer's String method) until it contains
// want or timeout elapses, returning the final observed output either way —
// the deterministic stand-in for a fixed sleep before asserting on an
// asynchronously-produced Cmd's output.
func waitForOutput(get func() string, want string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for {
		got := get()
		if strings.Contains(got, want) || time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

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

// TestProgramCopiesSelectionViaOSC52 is the required "OSC 52 byte sequence
// emission (captured-output test, like the mouse-enable test)": it drives a
// real tea.Program (same in-memory input/output harness as
// TestProgramEmitsMouseEnableSequence above) through a plain left-button
// click/drag/release over a roster row — INSIDE the overview's
// transcript-region equivalent, not its identity header (row 1, which
// [App.transcriptRegion] deliberately excludes from selection — see
// mouse_test.go's TestSelectionHighlightAndCopyExcludeChrome) — and asserts
// the raw output bytes carry bubbletea's OSC 52 clipboard-set sequence
// ("\x1b]52;c;<base64>\x07" — tea.SetClipboard, per the ansi package's
// SetSystemClipboard) for the exact text that selection covers ("wire", row
// 6's columns 2-5: row 0 is layout.TopPadding's blank filler, rows 1-4 are
// the identity header, row 5 is the "~/orchestration" cwd group header, row 6
// is "▸ wire the app root …", the first roster row — see
// testdata/app_overview.golden).
func TestProgramCopiesSelectionViaOSC52(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	app := tui.NewApp(theme.Test(), sup, tui.GoldenMeta(), tui.GoldenCommandEnv())

	out := &syncBuffer{}
	p := tea.NewProgram(app,
		tea.WithInput(strings.NewReader("")),
		tea.WithOutput(out),
		tea.WithWindowSize(80, 24),
		tea.WithoutSignalHandler(),
		tea.WithoutCatchPanics(),
	)

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = p.Run()
	}()

	p.Send(tea.WindowSizeMsg{Width: 80, Height: 24})
	// Init's fetchRoster Cmd (app.go) resolves asynchronously — wait for the
	// roster row the click targets to actually be on screen before clicking
	// it, rather than racing a frame that still shows the initial empty
	// roster (both would have their own render, but only one has "wire" at
	// row 6).
	_ = waitForOutput(out.String, "wire the app root", 2*time.Second)
	p.Send(tea.MouseClickMsg{X: 2, Y: 6, Button: tea.MouseLeft})
	p.Send(tea.MouseMotionMsg{X: 5, Y: 6, Button: tea.MouseLeft})
	p.Send(tea.MouseReleaseMsg{X: 5, Y: 6, Button: tea.MouseLeft})

	wantB64 := base64.StdEncoding.EncodeToString([]byte("wire"))
	wantSeq := "\x1b]52;c;" + wantB64 + "\x07"
	// tea.SetClipboard's Cmd executes asynchronously (its own goroutine,
	// round-tripping the resulting message back through the event loop
	// before the OSC 52 sequence is actually written) — there is no single
	// synchronous point at which p.Send returning guarantees it has landed,
	// so this polls the output instead of asserting immediately.
	got := waitForOutput(out.String, wantSeq, 2*time.Second)
	p.Quit()

	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("tea.Program did not exit after Quit; possible deadlock")
	}

	if !strings.Contains(got, wantSeq) {
		t.Errorf("program output missing the OSC 52 clipboard sequence %q for the selected text \"gofer\"\noutput: %q", wantSeq, got)
	}
}

// TestProgramOmitsOSC52WhenMouseDisabled is
// TestProgramCopiesSelectionViaOSC52's negative counterpart: with
// tui.mouse=false, the click/drag/release sequence never reaches
// App.Update at all (View sets tea.MouseModeNone, and Update's own
// mouseEnabled gate defensively no-ops any that arrive anyway — see
// mouseEnabled's doc), so no clipboard sequence is ever emitted.
func TestProgramOmitsOSC52WhenMouseDisabled(t *testing.T) {
	sup := newFakeSup(nil)
	env := tui.GoldenCommandEnv()
	disabled := false
	env.Config = func() (config.Config, error) { return config.Config{TUI: config.TUI{Mouse: &disabled}}, nil }
	app := tui.NewApp(theme.Test(), sup, tui.GoldenMeta(), env)

	out := &syncBuffer{}
	p := tea.NewProgram(app,
		tea.WithInput(strings.NewReader("")),
		tea.WithOutput(out),
		tea.WithWindowSize(80, 24),
		tea.WithoutSignalHandler(),
		tea.WithoutCatchPanics(),
	)

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = p.Run()
	}()

	p.Send(tea.WindowSizeMsg{Width: 80, Height: 24})
	p.Send(tea.MouseClickMsg{X: 0, Y: 1, Button: tea.MouseLeft})
	p.Send(tea.MouseMotionMsg{X: 4, Y: 1, Button: tea.MouseLeft})
	p.Send(tea.MouseReleaseMsg{X: 4, Y: 1, Button: tea.MouseLeft})
	p.Quit()

	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("tea.Program did not exit after Quit; possible deadlock")
	}

	got := out.String()
	if strings.Contains(got, "\x1b]52;c;") {
		t.Errorf("program output contains an OSC 52 clipboard sequence with mouse disabled; want none\noutput: %q", got)
	}
	const mouseButtonEvent = "\x1b[?1002h"
	if strings.Contains(got, mouseButtonEvent) {
		t.Errorf("program output contains the mouse enable sequence with tui.mouse=false; want mouse capture off entirely\noutput: %q", got)
	}
}
