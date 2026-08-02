package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/jedwards1230/agent-sdk-go/runner"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/telemetry"
	"github.com/jedwards1230/gofer/internal/worker"
)

// newSupervisor is the seam a test swaps to observe the [supervisor.Config] a
// worker actually builds — the same shape as this package's startDaemonProcess
// and serveForeground seams.
//
// It exists for one assertion that nothing else in the process can make: that a
// worker started WITHOUT --router hands supervisor.New a nil Subagents factory.
// That property is the "a worker never creates a session" invariant, and a
// worker's tool surface is not observable from outside it — gofer/capabilities
// carries MCP servers and skills, not builtins, and driving the spawn tool would
// need a real model turn. Asserting on the startup warning instead was tried and
// is too weak: the warning is computed from the local variable BEFORE
// supervisor.New is reached, so re-adding the fail-open default immediately
// above the call left the warning intact and the whole suite green.
var newSupervisor = supervisor.New

// readRouterTokenFile reads the router dial-back bearer token from path and
// DELETES the file, so the credential exists on disk only for the moment
// between the router writing it and this worker starting.
//
// An empty path is not an error: a router on a loopback bind with no bearer
// token configures none, and a worker with no token dials an unauthenticated
// router exactly as it did before this existed.
//
// A path that was given but cannot be read IS an error, and deliberately fails
// worker startup. The alternative — carrying on with an empty token — produces a
// worker whose every spawn dies with an opaque 401 several minutes later, inside
// a tool call, with nothing pointing back at the real cause.
//
// The delete is best-effort and never fails startup: the router sweeps a
// leftover token file with the rest of the worker's runtime artifacts (see
// internal/router's removeWorkerArtifacts).
func readRouterTokenFile(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path) // #nosec G304 -- the path comes from our own parent router's argv, not from a session.
	if err != nil {
		return "", fmt.Errorf("session-worker: read --router-token-file: %w", err)
	}
	if rmErr := os.Remove(path); rmErr != nil && !os.IsNotExist(rmErr) {
		_ = rmErr
	}
	return strings.TrimSpace(string(b)), nil
}

// runSessionWorker implements `gofer session-worker`: a single-session daemon
// that binds a unix-domain socket ([daemon.WorkerSocketPath]), prints a
// machine-readable handshake
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
	// The router's own listen address, so this worker can dial BACK to create
	// subagent sessions and deliver a finished child's report (see
	// internal/worker.RouterSubagents). Empty — a worker started by hand, or by
	// a router with no RouterAddr — leaves agent-initiated spawning unavailable
	// in this worker, which is the correct degradation: a single-session daemon
	// cannot host a sibling session, so there is nowhere local for a spawn to go.
	routerAddr := fs.String("router", "", "the router's listen address for subagent spawn/report dial-back (default: none)")
	// A PATH, never the token itself, and deliberately not an env var either:
	// the token is read once from this 0600 file and the file is deleted
	// immediately (see readRouterTokenFile). See router.Config.RouterToken for
	// why both of the obvious channels leak — argv to every local user, the
	// environment to the agent's own bash tool.
	routerTokenFile := fs.String("router-token-file", "", "path to a 0600 file holding the --router bearer token; read once and deleted")
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

	// The subagent seam reads the supervisor back (for the parent's model/cwd)
	// while the supervisor is being constructed with the seam. supervisor.Config
	// takes a FACTORY for exactly that reason, so the seam is built with the
	// live *Supervisor in hand rather than closed over a variable assigned
	// later.
	//
	// It dials nothing until a session actually spawns, so a worker whose
	// session never delegates pays nothing even if the router is unreachable.
	//
	// NIL WHEN NO --router WAS GIVEN, and nil is the whole point: a worker never
	// creates a session (see internal/worker's package doc — its embedded daemon
	// is MaxSessions: 1), so with no dial-back there is no legal answer to a
	// spawn and this process must not have one. supervisor.New leaves its seam
	// nil in turn, which suppresses the spawn tool and the child→parent report
	// regardless of what subagents.enabled says — the ONLY way to obtain the
	// session-creating implementation is to name supervisor.LocalSubagents, and
	// a worker never does. Passing supervisor.LocalSubagents here would let a
	// worker mint children locally, bypassing the router and the daemon's
	// MaxSessions cap; that is what the nil expresses and what
	// TestSessionWorkerNeverInstallsLocalSubagents pins.
	var subagents func(*supervisor.Supervisor) supervisor.Subagents
	if addr := *routerAddr; addr != "" {
		// Consumed and deleted HERE, at startup, before the supervisor exists
		// and long before any tool call can run — so the credential is in this
		// process's memory only, never on disk for the session's lifetime and
		// never in an environment the agent's shell inherits.
		token, terr := readRouterTokenFile(*routerTokenFile)
		if terr != nil {
			return terr
		}
		// The factory runs synchronously inside supervisor.New below, on this
		// goroutine, so seam is assigned before anything reads it — including
		// the deferred close, which runs at THIS function's exit and tolerates
		// a supervisor.New that failed before ever calling the factory.
		var seam *worker.RouterSubagents
		defer func() {
			if seam != nil {
				_ = seam.Close()
			}
		}()
		subagents = func(s *supervisor.Supervisor) supervisor.Subagents {
			seam = worker.NewRouterSubagents(addr, token, func() *supervisor.Supervisor { return s }, logger)
			return seam
		}
	}
	if subagents == nil && cfg.Subagents.IsEnabled() {
		// An operator opted into subagents on a worker that has no way to
		// deliver one. Suppressing the tool is correct (see above) but silence
		// about it is not: the symptom is a model that never delegates and a
		// parent that never hears back, neither of which points at the cause.
		// Read off the startup snapshot rather than the per-session resolver
		// because the dial-back this warns about is fixed for the process's
		// life — a later config edit cannot make this worker able to spawn.
		logger.Warn("subagents.enabled is set but this worker has no --router dial-back: spawn_subagent is not registered and no report will reach a parent",
			"session", *session)
	}

	sup, err := newSupervisor(supervisor.Config{
		Root:        rootDir,
		Permissions: cfg.Engine,
		// Same config-driven subagent depth cap as `gofer daemon`: the worker
		// hosts the session that a session/new with a gofer/parent `_meta`
		// actually creates, so it is the process that enforces the cap.
		MaxSubagentDepth: cfg.Session.SubagentDepthLimit(),
		// A worker hosts exactly one session, so this resolves once — but it
		// resolves through the same closure the daemon uses rather than the cfg
		// snapshot above, keeping one answer to "what posture does a new session
		// get" across every supervisor gofer builds.
		PermissionMode: permissionModeResolver(rootDir),
		// Same re-read-per-turn shape as PermissionMode above, for the
		// automatic-compaction trigger — the worker process is where a turn
		// actually settles, so it is the one that must observe a live edit.
		Compaction: compactionResolver(rootDir),
		// Same reasoning for lsp.* — see lspConfigResolver.
		LSP: lspConfigResolver(rootDir),
		// Same reasoning for mcp.* — see mcpConfigResolver. Under M6 process
		// isolation a worker hosts exactly one session, so "one manager per
		// gofer process" (internal/mcpconn's doc) means one manager per
		// session here — the same shape lspManager already takes.
		MCP: mcpConfigResolver(rootDir),
		// Same reasoning for tools.*/search.* — see toolsConfigResolver/
		// searchConfigResolver.
		Tools:  toolsConfigResolver(rootDir),
		Search: searchConfigResolver(rootDir),
		// Same reasoning for skills.* — see skillsConfigResolver.
		Skills: skillsConfigResolver(rootDir),
		// Same re-read-per-session shape, for subagents.* — see
		// subagentsConfigResolver. Unlike the in-process daemon this worker never
		// names supervisor.LocalSubagents: its own daemon is capped at one
		// session, so a spawn either leaves the process (see
		// internal/worker.RouterSubagents) or does not happen. See the seam's
		// construction above for why a nil factory is the fail-closed answer.
		SubagentsConfig: subagentsConfigResolver(rootDir),
		Subagents:       subagents,
		// Pin the sole session's id to --session (design Option A) through the
		// SDK's pre-assigned-session-id seam: runner.New creates the session with
		// this exact id, leaving entry-id generation on the store default.
		// SessionID is honored regardless of store injection — CreateWithID is a
		// session.Store interface method, so runner.New calls it on whatever store
		// it has. This factory simply omits the injection because the worker owns a
		// single session and lets runner.New build its own FileStore over the same
		// root; the omission is incidental to the pinning, not load-bearing for it.
		NewSession: func(ctx context.Context, opts runner.Options) (supervisor.Session, error) {
			opts.SessionID = *session
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

	// worker.Serve binds the unix socket, writes the handshake to stdout,
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
