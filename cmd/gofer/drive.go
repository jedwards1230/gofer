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
		promptErr <- r.Prompt(ctx, prompt)
		_ = r.Close()
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
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
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
