package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/jedwards1230/gofer/internal/render"
	"github.com/jedwards1230/gofer/internal/runner"
)

// driveSession subscribes to r's event stream, drives prompt on its own
// goroutine (closing r once the turn settles, whatever the outcome), and
// renders every event as it arrives — mirroring demo.go's subscribe-then-
// render structure. A ctx cancellation (Ctrl-C) is reported as an interrupt,
// not an error: the settled prefix is already durable in the journal by the
// time driveSession returns, since r.Close() waits for it to drain.
func driveSession(ctx context.Context, r *runner.Runner, prompt string, asJSON bool, stdout, stderr io.Writer) error {
	var rnd render.Renderer
	if asJSON {
		rnd = render.NewJSONL(stdout)
	} else {
		rnd = render.NewHuman(stdout, colorEnabled(stdout))
	}

	// Subscribe before prompting so no event is missed.
	sub := r.Events()

	promptErr := make(chan error, 1)
	go func() {
		// Close after the turn settles, whatever the outcome: it waits for the
		// journaling consumer to drain and returns any journal-write error it
		// observed. Fold that in so a failed persist is never silently dropped —
		// the caller must know if the session did not fully save.
		perr := r.Prompt(ctx, prompt)
		promptErr <- errors.Join(perr, r.Close())
	}()

	var renderErr error
	for e := range sub.C {
		if err := rnd.Render(e); err != nil {
			renderErr = err
			break
		}
	}
	err := <-promptErr

	if renderErr != nil {
		return fmt.Errorf("render stream: %w", renderErr)
	}
	if err != nil {
		// A Ctrl-C cancellation is an expected interrupt, not a failure — but a
		// journal-write error joined alongside it still means the saved prefix
		// may be incomplete, so only a *pure* cancellation is a clean interrupt.
		if ctx.Err() != nil && errors.Is(err, context.Canceled) && !hasNonCancel(err) {
			_, _ = fmt.Fprintf(stderr, "gofer: interrupted — progress saved, resume with `gofer resume %s`\n", r.ID())
			return nil
		}
		return fmt.Errorf("run session: %w", err)
	}
	if dropped := sub.Dropped(); dropped > 0 {
		_, _ = fmt.Fprintf(stderr, "gofer: dropped %d lossy event(s)\n", dropped)
	}
	return nil
}

// hasNonCancel reports whether err carries any error other than a context
// cancellation, unwrapping an errors.Join tree. It distinguishes a clean Ctrl-C
// interrupt (only context.Canceled) from a real failure — e.g. a journal-write
// error — hiding behind one.
func hasNonCancel(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, e := range joined.Unwrap() {
			if hasNonCancel(e) {
				return true
			}
		}
		return false
	}
	return err != nil && !errors.Is(err, context.Canceled)
}
