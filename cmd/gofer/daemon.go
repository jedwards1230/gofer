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
	"syscall"
	"time"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/telemetry"
)

// telemetryShutdownTimeout bounds the deferred telemetry flush at daemon exit.
// On the enabled path tel.Shutdown flushes the OTLP exporter to the configured
// collector; with an unbounded context (context.Background()) an unreachable or
// slow collector would wedge the whole process AFTER the listener has already
// closed — a graceful shutdown that never returns. Bounding it means the flush
// gets a best effort and then the process exits regardless. Mirrors the
// daemon's own graceful HTTP shutdownTimeout (internal/daemon).
const telemetryShutdownTimeout = 5 * time.Second

// runDaemon implements `gofer daemon` (alias `serve`): it builds a supervisor,
// hosts it behind an ACP-over-WebSocket listener, and blocks in the
// foreground until interrupted (SIGINT) or ctx is otherwise cancelled, then
// shuts both down.
func runDaemon(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	// Peel a lifecycle sub-verb before treating the remaining args as
	// foreground-serve flags: `gofer daemon install|uninstall|status` manage the
	// launchd/systemd unit, while a bare `gofer daemon` (or any other leading
	// token, e.g. `--listen`) falls through to today's foreground serve.
	// Mirrors runAuth peeling its lone positional.
	if len(args) > 0 {
		switch args[0] {
		case "install":
			return runDaemonInstall(ctx, args[1:], stdout, stderr)
		case "uninstall":
			return runDaemonUninstall(ctx, args[1:], stdout, stderr)
		case "status":
			return runDaemonStatus(ctx, args[1:], stdout, stderr)
		}
	}

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
	// Same explicit-fallback pattern as token/GOFER_TOKEN above: the flag's own
	// default is "", not os.Getenv("GOFER_LOG_LEVEL"), purely for symmetry
	// (there's no leak risk here, but one env-fallback convention in this
	// command is easier to read than two).
	logLevel := fs.String("log-level", "", "log level: debug, info, warn, or error (default: $GOFER_LOG_LEVEL, or \"info\")")
	if help, err := parseFlags(fs, args); err != nil {
		return err
	} else if help {
		return nil
	}

	bearerToken := *token
	if bearerToken == "" {
		bearerToken = os.Getenv("GOFER_TOKEN")
	}
	// Final fallback for a service-managed daemon: when a non-loopback install
	// delivered the token via the 0600 <root>/daemon.env file (never the unit
	// file or argv — see cmd/gofer/service.go writeDaemonEnvToken), read it here
	// so the launchd/systemd unit stays token-free. Best-effort and silent: a
	// read error or missing file just leaves the token empty (ValidateListen
	// then decides), and the token is never logged.
	if bearerToken == "" {
		if t, err := readDaemonEnvToken(*root); err == nil {
			bearerToken = t
		}
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
	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: lvl}))

	// Fail fast, before building a supervisor or resolving a model: a
	// non-loopback bind with no bearer token is a misconfiguration that
	// leaves the daemon open to unauthenticated, unattended tool execution.
	// daemon.Serve enforces this too (it's the authoritative check — see its
	// doc); this call exists purely so the CLI error is clean and immediate
	// rather than surfacing after a supervisor and model resolution have
	// already run.
	if err := daemon.ValidateListen(*listen, bearerToken); err != nil {
		return err
	}

	// Guard against a second `gofer daemon` clobbering a still-live one's
	// endpoint advertisement (see internal/daemon/endpoint.go and
	// guardLiveEndpoint's doc) — fail fast here, before any supervisor or
	// model resolution work, for the same reason ValidateListen runs first.
	if err := guardLiveEndpoint(ctx, *root, *listen); err != nil {
		return err
	}

	// Resolve --root through gofer's own default (~/.gofer, never any SDK
	// default) once, up front, and reuse it for both credential resolution
	// and the supervisor's session store.
	rootDir, err := supervisor.ResolveRoot(*root)
	if err != nil {
		return err
	}

	// Resolve the model before starting anything: a daemon with no usable
	// credential should fail fast at startup, not on the first session/new.
	modelID := *model
	if modelID == "" {
		var rerr error
		modelID, rerr = resolveRunModel(ctx, rootDir)
		if rerr != nil {
			return rerr
		}
	}

	// Load gofer's native config (permissions ruleset) from <root>/config.json.
	// A missing file is not an error — it compiles to the default
	// contain-or-ask policy (see config.Config.Engine); a malformed or invalid
	// file fails fast here, before the daemon starts accepting tool calls.
	cfg, err := config.Load(config.DefaultPath(rootDir))
	if err != nil {
		return err
	}

	// Build the telemetry provider from the loaded config (disabled unless
	// cfg.Telemetry.Enabled — see telemetry.Setup's off-by-default
	// guarantee) and re-wrap the logger with trace-correlation BEFORE it's
	// handed to daemon.New, so every downstream log call already carries the
	// correlating handler.
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
		// Attach a per-session telemetry observer at registration, before the
		// session's first turn — subscribing here (rather than after a turn
		// has already started) means Events' replay backlog is still empty,
		// so Instrument never sees a phantom span for history that predates
		// it. Mirrors watchPermissions' own subscribe-at-registration
		// precedent. When telemetry is disabled, tel.Instrument runs over a
		// noop tracer/meter: the hook still runs but produces zero spans and
		// zero exports, only cheap channel drains.
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

	d := daemon.New(sup, daemon.Config{
		ListenAddr:   *listen,
		BearerToken:  bearerToken,
		DefaultModel: modelID,
		Version:      version,
		Logger:       logger,
		// Let gofer/models report per-model availability to remote clients: a
		// phone ACP client can't see the daemon host's auth state, so the daemon
		// resolves the logged-in providers here (the same store `gofer auth`
		// reads) and stamps each model's Available flag. Non-fatal by contract —
		// a store/Status error degrades to "none authenticated" (see
		// daemon.Config.AuthedProviders), never a failed model discovery.
		AuthedProviders: func() (map[string]bool, error) {
			store, err := newAuthStore(rootDir)
			if err != nil {
				return nil, err
			}
			entries, err := store.Status()
			if err != nil {
				return nil, err
			}
			authed := make(map[string]bool, len(entries))
			for _, e := range entries {
				authed[e.Provider] = true
			}
			return authed, nil
		},
	})

	// Install the interrupt handler around the whole serve loop: the daemon
	// reads no interactive stdin, so there is no blocking-read-before-signal
	// hazard the other commands guard against.
	ctx, stop := interruptCtx(ctx)
	defer stop()

	// The listen address is operationally useful (an operator watching a log,
	// or copy-pasting it into an ACP client); the token, configured or not, is
	// never printed — pass one with --token or GOFER_TOKEN.
	logger.Info("daemon listening", "addr", *listen)

	// Advertise our endpoint so a same-host client (ps/kill/archive/attach/
	// agents, bare gofer) can discover us without --daemon/--token — see
	// cmd/gofer/daemonclient.go's daemonFlags.resolve and
	// internal/daemon/endpoint.go. Written only after guardLiveEndpoint above
	// has confirmed no live daemon already owns this root's endpoint file, and
	// only after every other startup check has passed, so a failed startup
	// never advertises a daemon that isn't actually going to serve.
	ourPID := os.Getpid()
	if err := daemon.WriteEndpoint(*root, daemon.Endpoint{
		Addr:      *listen,
		Token:     bearerToken,
		PID:       ourPID,
		StartedAt: time.Now(),
		Version:   version,
	}); err != nil {
		return fmt.Errorf("write daemon endpoint: %w", err)
	}
	// Guarded: only remove the file if it still names OUR pid when we get
	// here. A clean shutdown (the common case) always finds its own pid and
	// removes it. A crash leaves the file in place — clients self-heal past
	// a stale one (see the pidAlive/Probe check in guardLiveEndpoint, and
	// dialDaemon's own dead-address handling); a LATER daemon that started
	// after we crashed and overwrote the file with its own pid must NOT have
	// its endpoint clobbered by this deferred cleanup running (it never does,
	// since this process is already gone by then — this guard matters for
	// the case where guardLiveEndpoint judged an existing file stale and we
	// overwrote it, then something else races us).
	defer func() {
		if err := removeOwnEndpoint(*root, ourPID); err != nil {
			// Path/permission errors only — never the endpoint's contents
			// (address, token) — see [daemon.Endpoint]'s security note.
			logger.Warn("remove daemon endpoint file", "err", err)
		}
	}()

	serveErr := d.Serve(ctx)
	if cerr := sup.Close(); cerr != nil && serveErr == nil {
		serveErr = fmt.Errorf("close supervisor: %w", cerr)
	}
	return serveErr
}

// guardLiveEndpoint reports whether a still-running `gofer daemon` already
// owns the endpoint file at root and is bound to the SAME address this
// process is about to bind — in which case starting would be a silent
// double-listen a client could not tell apart from the original, so it is a
// clear error instead ("stop it first"), and the existing file is left
// untouched.
//
// An endpoint file that names a dead pid, or whose recorded address no
// longer answers a dial (see [daemon.Probe]), is stale — the residue of a
// crash rather than a clean shutdown (see runDaemon's own guarded-remove
// defer) — and is silently treated as absent; the caller
// ([daemon.WriteEndpoint]) then overwrites it. A live daemon recorded at a
// DIFFERENT address than the one we're about to bind is also let through
// unblocked (a deliberate second instance over the same root, e.g. during a
// migration) — this process's own WriteEndpoint call then becomes the
// root's advertised endpoint going forward.
func guardLiveEndpoint(ctx context.Context, root, listenAddr string) error {
	existing, err := daemon.ReadEndpoint(root)
	if err != nil {
		// Missing (nothing to guard against) or unreadable/corrupt (as good
		// as missing — WriteEndpoint replaces it below): either way, proceed.
		return nil
	}
	if !pidAlive(existing.PID) {
		return nil
	}
	dctx, cancel := context.WithTimeout(ctx, daemonDialTimeout)
	defer cancel()
	if !daemon.Probe(dctx, existing.Addr, existing.Token) {
		return nil
	}
	if existing.Addr != listenAddr {
		return nil
	}
	return fmt.Errorf("a gofer daemon is already running at %s (pid %d) — stop it first", existing.Addr, existing.PID)
}

// removeOwnEndpoint removes the endpoint file at root only if it still
// records pid as its owner, so a shutdown that runs after some other process
// has already taken over the file (see guardLiveEndpoint) never clobbers
// that other daemon's advertisement.
func removeOwnEndpoint(root string, pid int) error {
	cur, err := daemon.ReadEndpoint(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if cur.PID != pid {
		return nil
	}
	return daemon.RemoveEndpoint(root)
}

// pidAlive reports whether a process with the given pid is currently
// running — the liveness half of guardLiveEndpoint's stale-file detection.
// Unix-only (this repo ships no Windows build): os.FindProcess always
// succeeds on Unix regardless of whether pid is alive, so signal 0 is the
// portable "is it there" probe — it performs error checking without
// actually delivering a signal. A nil error, or EPERM (exists, just not
// ours to signal), both mean alive; anything else (typically ESRCH /
// [os.ErrProcessDone]) means gone.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// parseLogLevel maps a --log-level flag value to a [slog.Level]. Only the
// four canonical names are accepted (case-insensitive); anything else is a
// clean usage error rather than slog's own zero-value fallback (which would
// silently accept garbage as "info").
func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("--log-level: unrecognized level %q (want debug, info, warn, or error)", s)
	}
}
