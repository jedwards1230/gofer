package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// runDaemon implements `gofer daemon` (alias `serve`): it builds a supervisor,
// hosts it behind an ACP-over-WebSocket listener, and blocks in the
// foreground until interrupted (SIGINT) or ctx is otherwise cancelled, then
// shuts both down.
func runDaemon(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", daemon.DefaultListenAddr, "address to bind the ACP WebSocket listener")
	// The flag's own default is deliberately "", not os.Getenv("GOFER_TOKEN"):
	// flag.PrintDefaults (on --help or a parse error) renders a non-empty
	// default value verbatim into the usage banner, which would leak the
	// token to stderr/a terminal scrollback the instant it was set in the
	// environment. The env fallback is applied explicitly below instead.
	token := fs.String("token", "", "bearer token required of ws clients (default: $GOFER_TOKEN; empty disables auth)")
	model := fs.String("model", "", "default model for sessions created over ACP (default: the sole logged-in provider's model)")
	root := fs.String("root", "", "session store root (default ~/.gofer)")
	if help, err := parseFlags(fs, args); err != nil {
		return err
	} else if help {
		return nil
	}

	bearerToken := *token
	if bearerToken == "" {
		bearerToken = os.Getenv("GOFER_TOKEN")
	}

	// Resolve the model before starting anything: a daemon with no usable
	// credential should fail fast at startup, not on the first session/new.
	modelID := *model
	if modelID == "" {
		var rerr error
		modelID, rerr = resolveRunModel(ctx, *root)
		if rerr != nil {
			return rerr
		}
	}

	sup, err := supervisor.New(supervisor.Config{Root: *root})
	if err != nil {
		return fmt.Errorf("build supervisor: %w", err)
	}

	d := daemon.New(sup, daemon.Config{
		ListenAddr:   *listen,
		BearerToken:  bearerToken,
		DefaultModel: modelID,
	})

	// Install the interrupt handler around the whole serve loop: the daemon
	// reads no interactive stdin, so there is no blocking-read-before-signal
	// hazard the other commands guard against.
	ctx, stop := interruptCtx(ctx)
	defer stop()

	// The listen address is operationally useful (an operator watching a log,
	// or copy-pasting it into an ACP client); the token, configured or not, is
	// never printed — see docs/M2-PROOF.md for how to mint and pass one.
	_, _ = fmt.Fprintf(stderr, "gofer daemon: listening on %s\n", *listen)

	serveErr := d.Serve(ctx)
	if cerr := sup.Close(); cerr != nil && serveErr == nil {
		serveErr = fmt.Errorf("close supervisor: %w", cerr)
	}
	return serveErr
}
