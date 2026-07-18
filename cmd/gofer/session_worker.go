package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/jedwards1230/agent-sdk-go/runner"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/telemetry"
	"github.com/jedwards1230/gofer/internal/worker"
)

// runSessionWorker implements `gofer session-worker`: a single-session daemon
// that binds a loopback ephemeral port, prints a machine-readable handshake
// line to stdout (the only thing it ever writes there — all logs go to
// stderr), and serves the existing daemon wire until interrupted. It is the
// per-session process the M6 router spawns (docs/milestones/M6-process-
// isolation.md); it is NOT a discoverable top-level daemon, so it writes no
// endpoint file, runs under no launchd/systemd unit, and takes no bearer token
// — its sole client is the parent that read its handshake.
//
// It mirrors runDaemon's supervisor construction (root, model, permissions,
// telemetry) but hosts a single session whose id is PINNED to --session: the
// M6 router pre-generates the session uuid so it can key the worker's socket,
// endpoint file, and lock by it before the worker starts (design Option A). So
// --session is REQUIRED; --resume is intentionally absent (full resume is a
// later M6 phase, §8).
func runSessionWorker(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("session-worker", flag.ContinueOnError)
	fs.SetOutput(stderr)
	session := fs.String("session", "", "REQUIRED: the pinned session uuid the router pre-generated")
	model := fs.String("model", "", "default model for the session (default: the sole logged-in provider's model)")
	root := fs.String("root", "", "session store root (default ~/.gofer)")
	// Same explicit env-fallback convention as `gofer daemon` (see runDaemon):
	// the flag default is "", and $GOFER_LOG_LEVEL is applied below.
	logLevel := fs.String("log-level", "", "log level: debug, info, warn, or error (default: $GOFER_LOG_LEVEL, or \"info\")")
	if help, err := parseFlags(fs, args); err != nil {
		return err
	} else if help {
		return nil
	}

	// Hard-fail on a missing --session: there is no self-generated fallback. A
	// self-minted id would desync the worker's socket/endpoint/lock keying (all
	// derived from this uuid by the router) from its actual session id.
	if *session == "" {
		return errors.New("session-worker: --session <uuid> is required")
	}

	levelStr := *logLevel
	if levelStr == "" {
		levelStr = os.Getenv("GOFER_LOG_LEVEL")
	}
	if levelStr == "" {
		levelStr = "info"
	}
	lvl, err := parseLogLevel(levelStr)
	if err != nil {
		return err
	}
	// Logs to stderr, unconditionally: stdout is reserved for the single
	// handshake line the parent parses, so nothing else may land there.
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: lvl}))

	// Resolve --root through gofer's own default (~/.gofer) once, up front, and
	// reuse it for credential resolution and the session store — same as
	// runDaemon.
	rootDir, err := supervisor.ResolveRoot(*root)
	if err != nil {
		return err
	}

	// Resolve the model before starting anything: a worker with no usable
	// credential should fail fast at startup, not on the first session/new.
	modelID := *model
	if modelID == "" {
		var rerr error
		modelID, rerr = resolveRunModel(ctx, rootDir)
		if rerr != nil {
			return rerr
		}
	}

	// Load gofer's native config (permissions ruleset) from <root>/config.json;
	// a missing file compiles to the default contain-or-ask policy, a malformed
	// one fails fast here — identical to runDaemon.
	cfg, err := config.Load(config.DefaultPath(rootDir))
	if err != nil {
		return err
	}

	tel, wrappedLogger, err := telemetry.New(ctx, cfg.Telemetry.ToTelemetry(), logger.Handler())
	if err != nil {
		return fmt.Errorf("build telemetry: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), telemetryShutdownTimeout)
		defer cancel()
		if err := tel.Shutdown(shutdownCtx); err != nil {
			wrappedLogger.Warn("telemetry shutdown", "err", err)
		}
	}()
	logger = wrappedLogger

	sup, err := supervisor.New(supervisor.Config{
		Root:        rootDir,
		Permissions: cfg.Engine,
		// Pin the sole session's id to --session (design Option A): the factory
		// installs a stateful IDGen whose first draw is the pinned uuid (the
		// session id) and whose later draws are fresh UUIDv7 entry ids. This is
		// the BRIDGE isolated in worker.PinnedIDGen; when the SDK grows a proper
		// runner.Options.SessionID seam it collapses to a one-liner. Omitting the
		// shared-store injection here (unlike the default factory) lets runner.New
		// build its own store honoring opts.IDGen.
		NewSession: func(ctx context.Context, opts runner.Options) (supervisor.Session, error) {
			opts.IDGen = worker.PinnedIDGen(*session)
			return runner.New(ctx, opts)
		},
		// Attach a per-session telemetry observer at registration — mirrors
		// runDaemon's OnRegister exactly (see its doc for why subscribing here
		// avoids a phantom replay span; disabled telemetry runs a noop tracer).
		OnRegister: func(sess supervisor.Session) func() {
			sub := sess.Events()
			done := make(chan struct{})
			go func() {
				defer close(done)
				tel.Instrument(ctx, sess.ID(), sub.C)
			}()
			return func() {
				sub.Close()
				<-done
			}
		},
	})
	if err != nil {
		return fmt.Errorf("build supervisor: %w", err)
	}

	// Install the interrupt handler around the serve loop: the worker reads no
	// interactive stdin, so there is no blocking-read-before-signal hazard.
	ctx, stop := interruptCtx(ctx)
	defer stop()

	// worker.Serve binds the loopback listener, writes the handshake to stdout,
	// serves the wire, and closes the supervisor on shutdown.
	return worker.Serve(ctx, worker.Options{
		Supervisor:   sup,
		Session:      *session,
		DefaultModel: modelID,
		Version:      effectiveVersion(),
		Logger:       logger,
		Stdout:       stdout,
	})
}
