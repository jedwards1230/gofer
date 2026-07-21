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
	"github.com/jedwards1230/gofer/internal/router"
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

// serveForeground is the seam a test swaps to observe whether a `gofer daemon`
// invocation reached the foreground-serve path at all. It is what makes "an
// unknown sub-verb starts nothing" an assertion about the serve function never
// being entered, rather than an inference from an exit code — the old
// fall-through bug produced a non-zero exit too (via the already-running
// guard), so exit code alone cannot distinguish the fix from the defect.
var serveForeground = serveDaemonForeground

// runDaemon implements `gofer daemon` (alias `serve`): it dispatches the
// lifecycle sub-verbs and otherwise hands off to the foreground serve path.
func runDaemon(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	// Peel a lifecycle sub-verb before treating the remaining args as
	// foreground-serve flags: `gofer daemon install|uninstall|status|stop|restart`
	// manage the daemon's lifecycle, while a bare `gofer daemon` — or one whose
	// first token is a FLAG, e.g. `--listen` — falls through to foreground serve.
	// Mirrors runAuth peeling its lone positional.
	//
	// The default arm is load-bearing, not defensive. Without it every
	// unrecognized positional fell through to foreground serve, so `gofer daemon
	// stop` literally tried to START a daemon and the already-running guard then
	// answered "a gofer daemon is already running — stop it first"; a typo like
	// `gofer daemon staus` silently started one. Only a leading `-` still falls
	// through, because that is a serve flag rather than a mistyped sub-verb.
	if len(args) > 0 {
		switch args[0] {
		case "install":
			return runDaemonInstall(ctx, args[1:], stdout, stderr)
		case "uninstall":
			return runDaemonUninstall(ctx, args[1:], stdout, stderr)
		case "status":
			return runDaemonStatus(ctx, args[1:], stdout, stderr)
		case "stop":
			return runDaemonStop(ctx, args[1:], stdout, stderr)
		case "restart":
			return runDaemonRestart(ctx, args[1:], stdout, stderr)
		default:
			if !strings.HasPrefix(args[0], "-") {
				return &usageError{msg: fmt.Sprintf(
					"unknown sub-verb %q (want install, uninstall, status, stop, or restart; a bare `gofer daemon` starts one in the foreground)", args[0])}
			}
		}
	}

	return serveForeground(ctx, args, stdout, stderr)
}

// serveDaemonForeground builds a supervisor, hosts it behind an
// ACP-over-WebSocket listener, and blocks in the foreground until interrupted
// (SIGINT) or ctx is otherwise cancelled, then shuts both down. Reached only
// from runDaemon, for a bare `gofer daemon` or one whose args are serve flags.
func serveDaemonForeground(ctx context.Context, args []string, stdout, stderr io.Writer) error {
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
	// --workers opts the daemon into M6 process isolation: each session runs in
	// its own `gofer session-worker` child process behind a router supervisor,
	// so a session crash cannot take down the daemon or its siblings (see
	// docs/milestones/M6-process-isolation.md). Absent (the default), the daemon
	// hosts the in-process supervisor exactly as before — byte-for-byte
	// unchanged. A raw flag (not a config.Session knob) is deliberate for this
	// experimental slice: it is an operator-level launch mode, not a persisted
	// session default; it graduates to config once the feature is past Phase 1.
	workers := fs.Bool("workers", false, "run each session in its own worker process (M6 process isolation; experimental)")
	// --max-workers bounds the process fan-out --workers introduces: each worker
	// is a whole OS process (~10-20 MB RSS baseline plus its loop's working set
	// — M6 §10), so an unbounded session/new fan-out is a machine-resource risk
	// the in-process supervisor never had. At the cap, session/new is refused
	// with router.ErrAtCapacity before anything is forked. A flag rather than a
	// config.json field for the same reason as --workers above: it is an
	// operator-level launch mode sized to the host, not a persisted session
	// default. Ignored without --workers (the in-process supervisor forks
	// nothing).
	maxWorkers := fs.Int("max-workers", router.DefaultMaxWorkers, "under --workers, cap live worker processes (0 = unlimited)")
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
	//
	// modelPinned records the PROVENANCE of that value, not just the value:
	// whether the operator named the model on the command line. It decides
	// whether later config writes may retarget this daemon (see
	// daemonDefaultModelResolver).
	modelID := *model
	modelPinned := modelFlagPinned(fs, modelID)
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

	// The daemon hosts a Supervisor interface: the in-process supervisor by
	// default, or — under --workers — the M6 router that runs each session in
	// its own worker process. closeSup releases whichever was built.
	var sup daemon.Supervisor
	var closeSup func() error
	var routerSup *router.Supervisor
	if *workers {
		// Each worker is spawned from THIS gofer binary (`gofer session-worker`),
		// so the router needs its own executable path.
		selfExe, exeErr := os.Executable()
		if exeErr != nil {
			return fmt.Errorf("build router: resolve gofer executable: %w", exeErr)
		}
		rsup, rerr := router.New(router.Config{
			Root:  rootDir,
			Model: modelID,
			// The router's own build version, compared against each worker's
			// gofer/hello binaryVersion to classify skew. effectiveVersion() is
			// the SAME derivation every worker stamps itself with (see
			// runSessionWorker), so identical local builds compare equal.
			Version:    effectiveVersion(),
			SelfExe:    selfExe,
			Logger:     logger,
			MaxWorkers: *maxWorkers,
		})
		if rerr != nil {
			return fmt.Errorf("build router: %w", rerr)
		}
		routerSup = rsup
		// The router links no SDK runner/loop and instruments no sessions itself
		// — each worker owns its own telemetry — so there is no OnRegister hook
		// here. tel is still built above and flushed on exit (a no-op with no
		// spans), keeping the shared startup path uniform.
		sup, closeSup = rsup, rsup.Close
		logger.Info("session process isolation enabled (M6 workers)", "max_workers", *maxWorkers)
	} else {
		isup, serr := supervisor.New(supervisor.Config{
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
		if serr != nil {
			return fmt.Errorf("build supervisor: %w", serr)
		}
		sup, closeSup = isup, isup.Close
	}

	d := daemon.New(sup, daemon.Config{
		ListenAddr:   *listen,
		BearerToken:  bearerToken,
		DefaultModel: modelID,
		// Let a running daemon observe a later `session.model` config write
		// instead of freezing its startup answer forever (issue #156). nil when
		// the operator pinned --model, which keeps DefaultModel authoritative.
		ResolveDefaultModel: daemonDefaultModelResolver(modelPinned, rootDir),
		// effectiveVersion(), NOT the raw ldflags `version`: this value is what
		// gofer/hello reports as binaryVersion, and every worker stamps its own
		// with effectiveVersion() (see runSessionWorker). Stamping the raw
		// sentinel here would report "dev" for any local build while its own
		// workers report "dev-<sha>", making every locally-built worker look
		// version-skewed to the router that spawned it (M6 §6).
		Version: effectiveVersion(),
		Logger:  logger,
		// Under --workers, an adopted session's still-open permission is
		// re-surfaced into the router by its standing watcher (recordPendingPerm);
		// replaying it on session/load lets a client that attaches AFTER the
		// re-surface still see and answer it (design §7). The in-process daemon
		// leaves this false — its session/load is byte-for-byte unchanged.
		ReplayPendingPermissionsOnAttach: *workers,
		// How long session/load waits for a live session's in-flight turn to
		// finish journaling before folding history, so a load in that window does
		// not replay a short transcript (issue #137). Operator-tunable via
		// `session.load_settle_timeout_ms`; unset resolves to the default (see
		// config.Session.LoadSettleTimeout).
		LoadSettleTimeout: cfg.Session.LoadSettleTimeout(),
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

	// Bridge the router's worker-hosted sessions into the daemon's fan-out. The
	// daemon did not exist when router.New ran its adoption scan (it takes the
	// router as its Supervisor), so both relays are injected here, after
	// construction and before serve. In-process mode has no router and needs
	// neither.
	//
	//   - PERMISSIONS (design §7): publishes the relay the per-session watchers
	//     drive, so a permission asked — or re-surfaced by adoption's replay — on a
	//     session no prompt handler here drives still records its call→session
	//     route (making handlePermissionReply resolve) and reaches attached
	//     clients.
	//   - EVENTS (design §5): lets a worker-hosted turn's reconstructed events
	//     stream to attached clients verbatim (see [daemon.EventRelay]). Without
	//     it, a client attached to an adopted session watches a silent stream until
	//     the turn ends.
	if routerSup != nil {
		routerSup.SetPermissionRelay(d)
		routerSup.SetEventRelay(d)
	}

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
		// Same normalization as daemon.Config.Version above: the endpoint file is
		// the cheap pre-dial version hint, so it must advertise the SAME string
		// the in-protocol handshake reports, or the hint and the handshake would
		// disagree on every local build.
		Version: effectiveVersion(),
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
	if cerr := closeSup(); cerr != nil && serveErr == nil {
		serveErr = fmt.Errorf("close supervisor: %w", cerr)
	}
	return serveErr
}

// modelFlagPinned reports whether this daemon's default model was chosen by the
// operator on the command line, and is therefore authoritative for the process
// lifetime rather than open to being retargeted by a later config write.
//
// Both halves are required. flagWasSet, not `model != ""` alone, so the rule
// reads as "the operator passed --model" rather than being inferred from a
// sentinel — an explicit --model that happens to equal the config value is
// still explicit, and a future non-empty flag default would not silently
// become a pin. Non-empty, because `--model ""` explicitly asks for NO pinned
// model: startup then falls through to resolveRunModel exactly as an omitted
// flag does, so pinning it would freeze the daemon on a value the operator
// never named.
func modelFlagPinned(fs *flag.FlagSet, model string) bool {
	return flagWasSet(fs, "model") && model != ""
}

// flagWasSet reports whether name was actually passed on the command line, as
// opposed to merely holding its zero/default value. [flag.FlagSet.Visit] walks
// only the flags Parse actually saw, which is the one way to tell "the operator
// chose this" from "nobody said anything" without inventing a sentinel value.
func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// daemonDefaultModelResolver builds the [daemon.Config.ResolveDefaultModel]
// callback that lets a running daemon pick up a `session.model` config change
// without a restart (issue #156), and decides — once, at startup — whether this
// daemon is eligible for that at all.
//
// The rule is provenance-based:
//
//   - pinned (the operator passed --model): returns nil, so the daemon keeps
//     its flag for its whole lifetime. A model named on the command line is a
//     deliberate operator decision — this is the #147 lineage, where --model
//     exists precisely so an operator can pin a service-managed daemon — and a
//     config write by any attached client must not silently retarget it. A
//     daemon that could be redirected by a stray write would make --model
//     advisory, which is the opposite of what it is for.
//   - unpinned (the daemon inferred its default): returns a closure re-running
//     the SAME resolveRunModel policy `gofer run` uses, against the same root.
//     Re-running the whole policy — rather than reading config.Session.Model
//     directly — is what keeps one definition of "the default model": config
//     precedence, the credential scan, and #157's OpenAI credential-kind
//     routing all stay in one place, and a config write that CLEARS the model
//     correctly falls back to the credential-derived answer instead of
//     stranding the daemon on a value config no longer names.
//
// Errors are the caller's to absorb: the daemon treats a resolver error as
// non-fatal and keeps its startup value (see [daemon.Config.ResolveDefaultModel]),
// so a malformed config.json degrades a session/new to the old model rather
// than failing it.
func daemonDefaultModelResolver(pinned bool, root string) func(context.Context) (string, error) {
	if pinned {
		return nil
	}
	return func(ctx context.Context) (string, error) {
		return resolveRunModel(ctx, root)
	}
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
