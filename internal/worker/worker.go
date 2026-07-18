// Package worker hosts a single-session gofer daemon — the "a worker is a
// single-session daemon" realization from the M6 process-isolation design
// (docs/milestones/M6-process-isolation.md). A worker binds a loopback
// ephemeral TCP port it picks itself, serves the EXISTING daemon wire (ACP v1
// + gofer/* native) capped at one session, and announces its address to a
// parent process (the M6 router, built separately) via a single machine-
// readable handshake line printed to stdout before it begins serving.
//
// The worker is deliberately NOT a discoverable top-level daemon: it writes no
// endpoint file, runs no launchd/systemd unit, and takes no bearer token — its
// one client is the parent that spawned it and learned its address from the
// handshake. Everything above the SDK loop (pump, gate, journal, broker) runs
// here, so the router can be restarted without disturbing an in-flight turn.
//
// # Handshake transport contract
//
// The router codes against this exact contract when it spawns a worker
// (os/exec) and discovers its address:
//
//   - The worker writes exactly ONE line to stdout — the JSON encoding of
//     [Handshake] — as the FIRST and only thing on stdout. All logs go to
//     stderr; nothing else is ever written to stdout.
//   - The line is newline-terminated and written to an UNBUFFERED writer, so it
//     is readable the instant the listener is accepting.
//   - Ordering is load-bearing: the worker binds its listener, THEN writes the
//     handshake, THEN serves. The handshake's appearance means "ready to dial".
//   - The router reads stdout line by line until one decodes as a [Handshake],
//     then dials Handshake.Addr with daemon.Dial (no token).
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

// Handshake is the single machine-readable line a worker prints to stdout,
// before it begins serving, so the spawning parent (the M6 router) can learn
// the loopback address to dial. It is the JSON encoding of this struct on ONE
// line, and it is the FIRST and only thing the worker writes to stdout — all
// logs go to stderr. The router reads stdout lines until it decodes one.
//
// Addr is required; PID and Version are additive/forward-useful (PID lets the
// router reap/probe the worker; Version feeds the M6 skew-routing decision).
// The router imports and parses this exact type — treat the JSON tags as a
// wire contract.
type Handshake struct {
	// Addr is the worker's bound loopback address, e.g. "127.0.0.1:54321"
	// (from net.Listener.Addr().String()) — the address the router dials with
	// daemon.Dial.
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
	// closes it on shutdown. Required.
	Supervisor *supervisor.Supervisor
	// DefaultModel is the model a session/new resolves to when the client
	// supplies none — forwarded verbatim into daemon.Config.DefaultModel.
	DefaultModel string
	// Version stamps the handshake's Version field (see [Handshake]). Empty
	// omits it.
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

// Serve hosts opts.Supervisor behind a single-session daemon on a loopback
// ephemeral TCP port it binds itself, prints the [Handshake] line, and blocks
// serving the daemon wire until ctx is cancelled — then gracefully shuts the
// listener down and closes the supervisor. It is the shared core of the
// `gofer session-worker` command and its in-process tests.
//
// The order is load-bearing: bind, then WRITE THE HANDSHAKE, then serve. The
// router treats the handshake as "the worker is ready to be dialed", so it
// must not appear until the listener is actually accepting.
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

	// Loopback + ephemeral (:0): the kernel picks a free port, and the worker
	// learns it back from ln.Addr(). Loopback-only is the worker's isolation
	// posture — its sole client is the parent that reads the handshake, so it
	// needs (and offers) no non-loopback bind and no bearer token.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("worker: bind loopback listener: %w", err)
	}

	addr := ln.Addr().String()
	d := daemon.New(opts.Supervisor, daemon.Config{
		ListenAddr:   addr,
		DefaultModel: opts.DefaultModel,
		MaxSessions:  1, // a worker IS a single-session daemon (M6)
		Logger:       logger,
	})

	hs := Handshake{Addr: addr, PID: os.Getpid(), Version: opts.Version}
	if err := writeHandshake(stdout, hs); err != nil {
		// Serve owns the supervisor's Close once it is handed over; a bail-out
		// before the serve loop must still honor that, or an in-process caller
		// (a test reusing the process) leaks the supervisor's journal handles.
		_ = ln.Close()
		if cerr := opts.Supervisor.Close(); cerr != nil {
			logger.Warn("close supervisor after handshake write failure", "err", cerr)
		}
		return fmt.Errorf("worker: write handshake: %w", err)
	}
	logger.Info("worker listening", "addr", addr, "pid", hs.PID)
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
		return serveErr
	case err := <-errCh:
		// The listener stopped on its own (a bind/accept failure — not a
		// requested shutdown). Symmetry with the ctx.Done path: cancel any live
		// connection contexts via d.Shutdown before closing the supervisor,
		// even though srv itself has already stopped accepting.
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
