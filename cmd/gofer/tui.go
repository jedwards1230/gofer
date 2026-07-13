package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// driveTUI is the interactive frontend for `gofer run`/`gofer resume`: it
// renders r's event stream live through the bubbletea attach [tui.Program]
// instead of driveSession's line-oriented renderer, while prompt runs on its
// own goroutine exactly as driveSession drives it. It only ever runs when
// useTUI has already confirmed stdout and stdin are real terminals and the
// prompt did not come from stdin (the TUI reads keys from it).
func driveTUI(ctx context.Context, r sessionDriver, prompt string, stdout, stderr io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	prog := tui.NewProgram(theme.Default())
	// The live view runs in the alternate screen (requested by Program.View)
	// so its height-clipped frames leave no residue on the normal buffer; on
	// exit the alt screen is torn down and the full transcript is flushed to
	// the scrollback (see below), fixing the M1 bug where only the
	// viewport-clipped final frame remained.
	opts := []tea.ProgramOption{tea.WithContext(ctx), tea.WithInput(os.Stdin)}
	if f, ok := stdout.(*os.File); ok {
		opts = append(opts, tea.WithOutput(f))
	}
	p := tea.NewProgram(prog, opts...)

	// Subscribe before prompting so no event is missed.
	sub := r.Events()

	// Forward every session event into the running program. When the
	// subscription channel closes — r.Close, called by the run goroutine
	// below once the turn settles — quit the program too, so a stream that
	// ends on its own (not just a user esc/ctrl-c) also exits the TUI.
	go func() {
		for e := range sub.C {
			p.Send(tui.EventMsg{Event: e})
		}
		p.Quit()
	}()

	// Drive the prompt on its own goroutine, closing r once the turn
	// settles (whatever the outcome) so the forwarder above observes the
	// subscription close. Buffered so it never blocks on a program that
	// has already exited.
	runDone := make(chan error, 1)
	go func() {
		perr := r.Prompt(ctx, prompt)
		runDone <- errors.Join(perr, r.Close())
	}()

	// p.Run returns when the event stream ends on its own (forwarder Quit),
	// the user quits (esc/ctrl-c → tea.Quit), or ctx is cancelled (ctrl-c via
	// the signal handler → tea.WithContext → tea.ErrProgramKilled). A kill by
	// ctx cancellation is an expected interrupt, not a UI failure, so it falls
	// through to the run-outcome handling below; any other p.Run error is a
	// genuine TUI failure.
	finalModel, uiErr := p.Run()

	// Cancel unconditionally: a no-op once the turn has already settled, or the
	// signal that interrupts an in-flight one when the user quit early —
	// mirroring driveSession's Ctrl-C handling.
	cancel()
	runErr := <-runDone

	// Flush the full transcript to the (now-restored) normal buffer, so the
	// scrollback holds the whole conversation rather than the clipped final
	// frame. Runs on every exit path that yields a program model — clean
	// stream-end, esc/ctrl-c, or a cancelled context — since Run always tears
	// down the alt screen before returning.
	if prog, ok := finalModel.(tui.Program); ok {
		if ft := prog.FinalTranscript(); ft != "" {
			_, _ = fmt.Fprintln(stdout, ft)
		}
	}

	if uiErr != nil && !errors.Is(uiErr, tea.ErrProgramKilled) {
		return fmt.Errorf("tui: %w", uiErr)
	}
	if runErr != nil {
		// A pure cancellation is an expected interrupt, not a failure — but a
		// journal-write error joined alongside it still means the saved
		// prefix may be incomplete, so only a *pure* cancellation is clean.
		if ctx.Err() != nil && errors.Is(runErr, context.Canceled) && !hasNonCancel(runErr) {
			_, _ = fmt.Fprintf(stderr, "gofer: interrupted — progress saved, resume with `gofer resume %s`\n", r.ID())
			return nil
		}
		return fmt.Errorf("run session: %w", runErr)
	}
	return nil
}

// useTUI reports whether a run/resume invocation should drive through the
// interactive attach TUI (driveTUI) rather than the line renderer
// (driveSession): --json was not requested, the prompt came from CLI
// arguments rather than stdin (driveTUI reads raw key presses from stdin
// itself, so a prompt sourced from a stdin pipe would collide with it), and
// both stdout and the process's real stdin are connected to a terminal.
// useTUI reports whether an interactive attach TUI should render this
// run/resume rather than the line renderer: --json was not requested and BOTH
// stdin and stdout are terminals. The prompt's SOURCE (CLI args vs the
// interactive `prompt>` read) is deliberately not a factor. On a terminal the
// prompt read is line-buffered — one line, no over-read — so stdin is left
// clean for bubbletea to take over; the piped-stdin case (where a bufio
// over-read could steal the TUI's keystrokes) is already excluded by stdinTTY.
func useTUI(asJSON, stdinTTY, stdoutTTY bool) bool {
	return !asJSON && stdinTTY && stdoutTTY
}

// isTerminal reports whether f is connected to a terminal.
func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// interactiveTTY reports whether stdout is a real terminal — fit for
// rendering the attach TUI's live, redrawing output, as opposed to a pipe,
// redirected file, or an in-test fake like *bytes.Buffer.
func interactiveTTY(stdout io.Writer) bool {
	f, ok := stdout.(*os.File)
	return ok && isTerminal(f)
}

// stdinIsTTY reports whether the process's real stdin is a terminal.
// driveTUI reads raw key presses from os.Stdin directly (see
// tea.WithInput above), so a piped or redirected stdin must fall back to
// driveSession's line renderer instead.
func stdinIsTTY() bool { return isTerminal(os.Stdin) }
