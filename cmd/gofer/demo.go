package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	_ "embed"

	"github.com/jedwards1230/agent-sdk-go/compose"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/render"
)

// embeddedManifest is the default demo manifest, compiled into the binary so
// `gofer demo` runs with no filesystem dependency.
//
//go:embed agent.yaml
var embeddedManifest []byte

// demoPrompt is the fixed prompt fed to the faux provider. The provider ignores
// its content — its output is fully scripted — so any string reads back the
// same canned turn.
const demoPrompt = "demo"

// runDemo loads a manifest (embedded by default, or --manifest <path>), runs one
// scripted turn, and renders the resulting event stream. With --json it emits
// the stream as JSONL; otherwise it prints a legible transcript.
func runDemo(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	manifestPath := fs.String("manifest", "", "path to an agent manifest (default: embedded faux manifest)")
	asJSON := fs.Bool("json", false, "emit each event as JSONL instead of a human-readable transcript")
	if err := fs.Parse(args); err != nil {
		return err
	}

	sess, err := loadSession(ctx, *manifestPath)
	if err != nil {
		return err
	}
	defer sess.Close()

	var r render.Renderer
	if *asJSON {
		r = render.NewJSONL(stdout)
	} else {
		r = render.NewHuman(stdout, colorEnabled(stdout))
	}

	// Subscribe before prompting so no event is missed; session.created is also
	// replayed to late subscribers, but subscribing first keeps ordering obvious.
	sub := sess.Events()

	// Drive the turn on its own goroutine and Close the session when it returns,
	// which closes the subscription channel and ends the range below. Prompt
	// publishes into an ample buffer, so it never blocks on a slow renderer.
	promptErr := make(chan error, 1)
	go func() {
		promptErr <- sess.Prompt(ctx, demoPrompt)
		sess.Close()
	}()

	var renderErr error
	for e := range sub.C {
		if err := r.Render(e); err != nil {
			renderErr = err
			break
		}
	}
	if err := <-promptErr; err != nil {
		return fmt.Errorf("run session: %w", err)
	}
	if renderErr != nil {
		return fmt.Errorf("render stream: %w", renderErr)
	}
	if dropped := sub.Dropped(); dropped > 0 {
		_, _ = fmt.Fprintf(stderr, "gofer demo: dropped %d lossy event(s)\n", dropped)
	}
	return nil
}

// loadSession builds a session from the manifest at path, or from the embedded
// manifest when path is empty.
func loadSession(ctx context.Context, path string) (*session.Session, error) {
	if path != "" {
		sess, err := compose.Load(ctx, path)
		if err != nil {
			return nil, fmt.Errorf("load manifest: %w", err)
		}
		return sess, nil
	}
	m, err := compose.Parse(embeddedManifest)
	if err != nil {
		return nil, fmt.Errorf("parse embedded manifest: %w", err)
	}
	sess, err := compose.Build(ctx, m)
	if err != nil {
		return nil, fmt.Errorf("build session: %w", err)
	}
	return sess, nil
}

// colorEnabled reports whether ANSI styling should be used for w: true only when
// w is a terminal and NO_COLOR is unset.
func colorEnabled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
