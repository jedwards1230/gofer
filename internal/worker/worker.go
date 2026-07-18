// Package worker hosts a single-session gofer daemon — the "a worker is a
// single-session daemon" realization from the M6 process-isolation design
// (docs/milestones/M6-process-isolation.md). A worker binds a per-session unix
// socket (path keyed by its session uuid), serves the EXISTING daemon wire (ACP
// v1 + gofer/* native) capped at one session, and announces its address to the
// parent process (the M6 router) two ways: an on-disk endpoint file (for the
// router's future adoption scan) and a single machine-readable handshake line
// printed to stdout before it begins serving (for fresh-spawn discovery).
//
// A worker is detached from the router by design (design §3): the router spawns
// it with Setsid so it outlives a router restart. Everything above the SDK loop
// (pump, gate, journal, broker) runs here, so the router can be restarted
// without disturbing an in-flight turn.
//
// # Session-id pinning (design Option A)
//
// A worker's socket, endpoint file, and single-writer lock are all keyed by the
// SESSION uuid. But the worker does not itself mint that uuid — the router
// pre-generates it and passes it as --session so it can key those files BEFORE
// the worker starts. The worker therefore treats --session as REQUIRED and pins
// it as its session id via [PinnedIDGen]; see that helper for why the store's
// first id draw is the session id.
//
// # Router restart survival (adoption)
//
// A detached worker outlives a router restart. The worker keeps running, keeps
// holding its unix socket, its <uuid>.lock, and its <uuid>.json endpoint file;
// the NEXT router start scans those endpoint files and re-adopts the still-alive
// worker by dialing its socket (see internal/router's adoptExistingWorkers).
// This endpoint file is that scan's advertisement — pid for the liveness probe,
// addr to dial, wire/binary version for the adopt/skew decision.
//
// # Handshake transport contract
//
// The router codes against this exact contract for fresh-spawn discovery:
//
//   - The worker writes exactly ONE handshake line — the JSON encoding of
//     [Handshake] — to stdout, before it begins serving. Under the router's
//     detached spawn stdout is redirected to the worker's log file, so the
//     router discovers the line by scanning that file; in-process tests capture
//     it via [Options.Stdout]/[Options.Ready].
//   - Ordering is load-bearing: the worker binds its listener, THEN writes the
//     handshake, THEN serves. The handshake's appearance means "ready to dial".
//   - [Handshake.Addr] is self-describing ("unix://<path>"); the router dials it
//     verbatim with daemon.Dial (no token).
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// unixSchemePrefix prefixes a self-describing unix-socket address the worker
// advertises (in its [Handshake] and its [daemon.WorkerEndpoint]) and the
// router dials verbatim via daemon.Dial's unix:// scheme (slice 2a).
const unixSchemePrefix = "unix://"

// Handshake is the single machine-readable line a worker prints to stdout,
// before it begins serving, so the spawning parent (the M6 router) can learn
// the address to dial. It is the JSON encoding of this struct on ONE line, and
// it is the FIRST thing the worker writes to stdout. The router scans lines
// (skipping any log lines interleaved under a detached spawn's shared stdio
// file) until it decodes one.
//
// Addr is required; PID and Version are additive/forward-useful (PID lets the
// router reap/probe the worker; Version feeds the M6 skew-routing decision).
// The router imports and parses this exact type — treat the JSON tags as a
// wire contract.
type Handshake struct {
	// Addr is the worker's self-describing unix-socket address,
	// "unix://<path>" (see [daemon.WorkerSocketPath]) — dialed verbatim by the
	// router via daemon.Dial's unix:// scheme.
	Addr string `json:"addr"`
	// PID is the worker process id (os.Getpid()), for the router's reap/probe
	// path.
	PID int `json:"pid"`
	// Version is the worker's build version (cmd/gofer's effectiveVersion),
	// forward-useful for M6 version-skew routing. Omitted when empty.
	Version string `json:"version,omitempty"`
}

// readHeaderTimeout bounds how long the worker's HTTP server waits to read a
// request's headers, defending the slowloris DoS the same way the daemon's own
// listener does (internal/daemon.readHeaderTimeout). Even a loopback-only
// worker sets it so a wedged upgrade request cannot tie up a goroutine
// indefinitely.
const readHeaderTimeout = 10 * time.Second

// shutdownTimeout bounds the worker's graceful shutdown so a wedged handler
// cannot block exit forever — mirrors internal/daemon.shutdownTimeout.
const shutdownTimeout = 5 * time.Second

// Options configures [Serve].
type Options struct {
	// Supervisor is the single-session supervisor the worker hosts. The caller
	// builds it (root/model/permissions/telemetry) and hands it over; Serve
	// closes it on shutdown. Required. The caller is responsible for building it
	// with a [PinnedIDGen] factory so its one session adopts Session as its id.
	Supervisor *supervisor.Supervisor
	// Session is the pinned session uuid the router pre-generated (design Option
	// A). It keys the worker's socket ([daemon.WorkerSocketPath]), endpoint file
	// ([daemon.WorkerEndpointPath]), and single-writer lock
	// ([daemon.WorkerLockPath]). REQUIRED: Serve fails fast if it is empty —
	// there is no self-generated fallback, since that would desync the file
	// keying from the pinned session id.
	Session string
	// DefaultModel is the model a session/new resolves to when the client
	// supplies none — forwarded verbatim into daemon.Config.DefaultModel.
	DefaultModel string
	// Version is the worker's build version (cmd/gofer's effectiveVersion). It
	// stamps all three of the worker's version advertisements: the handshake
	// line's Version (see [Handshake]), the endpoint file's BinaryVersion (the
	// router's cheap pre-dial hint), and gofer/hello's binaryVersion (the
	// authoritative in-protocol handshake the router classifies skew from,
	// design §6). Empty omits it from the handshake and reports an empty
	// binaryVersion — which a router classifies as unknown-but-adoptable rather
	// than failing.
	Version string
	// Logger receives the worker's structured logs. Nil discards them. Logs go
	// to stderr in the cmd wiring; they must never reach Stdout, which carries
	// only the handshake line.
	Logger *slog.Logger
	// Stdout receives the single handshake line. Nil defaults to os.Stdout.
	// Kept as an injectable io.Writer so a test can capture and parse the
	// handshake without os/exec. It MUST be unbuffered (os.Stdout and an
	// io.Pipe both are): writeHandshake does not flush, so a bufio.Writer would
	// strand the handshake and the router would hang waiting to read it.
	Stdout io.Writer
	// Ready, if non-nil, is invoked exactly once with the bound Handshake
	// immediately after the listener binds and the handshake is written — the
	// in-process test seam that lets a test learn the address (and drive a
	// turn) without spawning a process. It runs before Serve blocks on ctx.
	Ready func(Handshake)
}

// Serve hosts opts.Supervisor behind a single-session daemon on a per-session
// unix socket, advertises itself (endpoint file + [Handshake] line), and blocks
// serving the daemon wire until ctx is cancelled — then gracefully shuts the
// listener down, closes the supervisor, and clears its endpoint file + lock. It
// is the shared core of the `gofer session-worker` command and its in-process
// tests.
//
// The startup sequence is load-bearing (design §3/§4):
//
//  1. Compute the socket path; fail fast on its length-guard error BEFORE
//     acquiring anything.
//  2. Acquire the single-writer lock; [daemon.ErrWorkerLocked] means another
//     live worker owns this session — exit.
//  3. Remove any stale socket left by a crashed predecessor (safe: we hold the
//     lock).
//  4. Bind the unix socket.
//  5. Write the endpoint file (the router's adoption-scan advertisement).
//  6. Write the handshake (fresh-spawn discovery), THEN serve.
//
// On a CLEAN exit (ctx cancelled) it removes the endpoint file and releases the
// lock. The lifecycle is deliberately asymmetric: an abnormal listener stop —
// and a crash, where no deferred code runs at all — leaves the <uuid>.lock,
// endpoint file, and socket behind; the router's adoption scan garbage-collects
// these stale artifacts (dead pid, or a dialed-refused socket).
func Serve(ctx context.Context, opts Options) error {
	if opts.Supervisor == nil {
		return errors.New("worker: nil supervisor")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}

	// Serve owns opts.Supervisor.Close once it is handed over; every early
	// bail-out must honor that or an in-process caller leaks journal handles.
	closeSup := func() {
		if cerr := opts.Supervisor.Close(); cerr != nil {
			logger.Warn("close supervisor after worker startup failure", "err", cerr)
		}
	}

	sessionID := opts.Session
	if sessionID == "" {
		// REQUIRED — no self-generated fallback: a self-minted id would desync
		// the socket/endpoint/lock keying from the router's pinned session id.
		closeSup()
		return errors.New("worker: empty session id (--session is required in worker mode)")
	}

	// 1. Socket path first; its length guard fails fast before we acquire the
	//    lock or bind anything.
	socketPath, err := daemon.WorkerSocketPath(sessionID)
	if err != nil {
		closeSup()
		return fmt.Errorf("worker: %w", err)
	}

	// 2. Single-writer lock: the authoritative one-worker-per-session guard.
	release, err := daemon.LockWorker(sessionID)
	if err != nil {
		closeSup()
		return fmt.Errorf("worker: lock session %s: %w", sessionID, err)
	}
	// From here every failure path must release the lock (Flock(LOCK_UN)+Close;
	// it never unlinks <uuid>.lock).

	// 3. Clear a crashed predecessor's stale socket — unix Listen fails
	//    EADDRINUSE on a leftover socket file, and holding the lock means no
	//    live worker owns it.
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		_ = release()
		closeSup()
		return fmt.Errorf("worker: remove stale socket %s: %w", socketPath, err)
	}

	// 4. Bind the unix socket.
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		_ = release()
		closeSup()
		return fmt.Errorf("worker: bind unix socket %s: %w", socketPath, err)
	}

	addr := unixSchemePrefix + socketPath

	// 5. Advertise the endpoint file for the router's future adoption scan.
	if err := daemon.WriteWorkerEndpoint(sessionID, daemon.WorkerEndpoint{
		Addr:          addr,
		PID:           os.Getpid(),
		BinaryVersion: opts.Version,
		WireVersion:   daemon.WireVersion,
		StartedAt:     time.Now(),
	}); err != nil {
		_ = ln.Close()
		_ = release()
		closeSup()
		return fmt.Errorf("worker: write endpoint for %s: %w", sessionID, err)
	}

	d := daemon.New(opts.Supervisor, daemon.Config{
		ListenAddr:   socketPath,
		DefaultModel: opts.DefaultModel,
		MaxSessions:  1, // a worker IS a single-session daemon (M6)
		// The worker's build version, reported verbatim as gofer/hello's
		// binaryVersion — the AUTHORITATIVE half of the M6 §6 version exchange
		// (the endpoint file above is only the cheap pre-dial hint). Without it
		// the handshake would report an empty binaryVersion and the router could
		// never tell an old-binary worker from its own.
		Version: opts.Version,
		Logger:  logger,
		// Re-surface a still-open permission request to a router that adopts this
		// worker mid-approval: the adopting router attaches via session/load, and
		// the outstanding gate (live in-flight state, not journaled) reaches it
		// only if this worker re-emits it there (design §7).
		ReplayPendingPermissionsOnAttach: true,
	})

	// 6. Handshake, THEN serve. The router treats the handshake as "ready to be
	//    dialed", so it must not appear until the listener is accepting.
	hs := Handshake{Addr: addr, PID: os.Getpid(), Version: opts.Version}
	if err := writeHandshake(stdout, hs); err != nil {
		_ = daemon.RemoveWorkerEndpoint(sessionID)
		_ = ln.Close()
		_ = release()
		closeSup()
		return fmt.Errorf("worker: write handshake: %w", err)
	}
	logger.Info("worker listening", "addr", addr, "pid", hs.PID, "session", sessionID)
	if opts.Ready != nil {
		opts.Ready(hs)
	}

	srv := &http.Server{
		Handler:           d.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
	}

	// Serve on the already-bound listener; ln ownership passes to srv, which
	// closes it on Shutdown.
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		// CLEAN shutdown: drain, close the supervisor, then clear the endpoint
		// file and release the lock so a restarted router sees no stale
		// artifacts for this session.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		// d.Shutdown cancels every live connection's context (unblocking any
		// in-flight, hijacked WebSocket handler); srv.Shutdown then drains the
		// HTTP server. Both are best-effort under the bounded context.
		_ = d.Shutdown(shutdownCtx)
		serveErr := srv.Shutdown(shutdownCtx)
		if cerr := opts.Supervisor.Close(); cerr != nil && serveErr == nil {
			serveErr = fmt.Errorf("worker: close supervisor: %w", cerr)
		}
		_ = daemon.RemoveWorkerEndpoint(sessionID)
		_ = release()
		return serveErr
	case err := <-errCh:
		// The listener stopped on its own (a serve/accept failure — not a
		// requested shutdown). Close the supervisor, but LEAVE the endpoint file
		// and lock: this is an abnormal exit, and the router's future adoption
		// scan detects and clears the staleness. The flock auto-releases when
		// this process exits regardless.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		_ = d.Shutdown(shutdownCtx)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		} else if err != nil {
			err = fmt.Errorf("worker: serve %s: %w", addr, err)
		}
		if cerr := opts.Supervisor.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("worker: close supervisor: %w", cerr)
		}
		return err
	}
}

// writeHandshake encodes hs as one JSON line (json.Encoder appends the
// newline) to w. It does NOT flush — [Options.Stdout] must be an unbuffered
// writer (os.Stdout / an io.Pipe), so the line reaches the reader immediately.
// Kept separate so the exact "one line, JSON, newline-terminated" contract
// lives in one place the router-side parser can be read against.
func writeHandshake(w io.Writer, hs Handshake) error {
	enc := json.NewEncoder(w)
	return enc.Encode(hs)
}
