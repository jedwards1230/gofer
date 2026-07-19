package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jedwards1230/agent-sdk-go/acp"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/worker"
)

// These tests drive the REAL `gofer session-worker` entrypoint —
// runSessionWorker in session_worker.go — not a re-implementation of it.
// internal/router's crash-isolation tests spawn a faux worker that mirrors this
// wiring; that harness is a router-side stand-in and cannot catch a regression
// in cmd/gofer's own copy. The pin below (design Option A: the router
// pre-generates the session uuid and keys the worker's socket, endpoint file,
// and lock by it BEFORE the worker starts) is asserted here, against the
// production code path.
//
// Precisely: the RUNNER-SIDE pin — runner.Options.SessionID, which decides the
// id the session and its journal adopt — lives only in cmd/gofer. The flag also
// feeds worker.Options.Session, which keys the socket, endpoint file and lock;
// that half is equally load-bearing and is exercised in internal/worker. This
// test covers the runner-side half, which had no coverage at all.
//
// Hermetic: no network. The worker builds a real provider, but only its
// CREDENTIAL is pre-flighted at session creation (runner.newProvider) — the
// sk-test-key below satisfies that, and no test drives a turn, so nothing ever
// reaches a model API.

// shortWorkerRuntimeDir points XDG_RUNTIME_DIR at a short-rooted temp dir for
// the test's duration so the worker's unix socket ([daemon.WorkerSocketPath])
// stays inside its ~103-byte budget — a deep macOS t.TempDir() would overflow
// it. Mirrors internal/worker's own harness.
func shortWorkerRuntimeDir(t *testing.T) {
	t.Helper()
	base := "/tmp"
	if _, err := os.Stat(base); err != nil {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, "gfcw")
	if err != nil {
		t.Fatalf("mkdir short runtime dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("XDG_RUNTIME_DIR", dir)
}

// syncBuffer is a mutex-guarded io.Writer for the worker's stderr: the worker
// logs from its own goroutine while the test reads the buffer to report
// failures, so an unguarded bytes.Buffer would be a data race under -race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// startSessionWorker runs runSessionWorker with the given extra args against a
// temp root, reads the handshake line off its stdout exactly as the router
// does, and returns the handshake plus a stop func that shuts the worker down
// and asserts a clean return. Callers get the root back so they can inspect
// on-disk artifacts.
func startSessionWorker(t *testing.T, sessionID string, extra ...string) (hs worker.Handshake, root string, stop func()) {
	t.Helper()
	shortWorkerRuntimeDir(t)
	// A credential must RESOLVE (never be used): runSessionWorker resolves its
	// model from the sole credentialed provider, and runner.New pre-flights that
	// provider's credential before creating the journal. No network call is made
	// — the key is never spent, only checked for presence.
	//
	// IF THIS TEST FAILS WITH A CREDENTIAL OR PROVIDER-RESOLUTION ERROR rather
	// than a session-id mismatch, that is NOT a pin regression: the SDK's
	// provider requirements have changed and this env setup needs updating to
	// match. The pin failure looks like `session id = "…", want the pinned …` or
	// `journal id on disk = "…", want …`. Anything else is scaffolding rot, and
	// reading it as a pinning bug will cost an hour in the wrong file.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-key")
	t.Setenv("OPENAI_API_KEY", "")
	// Neutralize ambient config that fails BEFORE the handshake. An invalid
	// GOFER_LOG_LEVEL on the developer's machine would fail parseLogLevel during
	// setup, which is the pre-handshake path — survivable now that the pipe is
	// closed on return, but it would fail with a confusing log-level error in a
	// test about session ids. Pin it.
	t.Setenv("GOFER_LOG_LEVEL", "error")

	root = t.TempDir()
	args := append([]string{"--session", sessionID, "--root", root}, extra...)

	// An io.Pipe stands in for the worker's stdout so the handshake is read
	// through the same "scan lines until one decodes" path the router uses.
	pr, pw := io.Pipe()
	stderr := &syncBuffer{}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		err := runSessionWorker(ctx, args, pw, stderr)
		// Closing the write end is what makes a PRE-HANDSHAKE failure
		// observable. runSessionWorker can return an error before worker.Serve
		// ever writes the handshake (a bad credential, an unparseable log level,
		// any early setup error). Without this close, nothing ever writes to the
		// pipe and nothing closes it, so the scanner below blocks FOREVER — the
		// test hangs until the package-wide timeout panics and takes every other
		// test in cmd/gofer with it, reporting a goroutine dump that names
		// neither this test nor the real cause.
		_ = pw.CloseWithError(io.EOF)
		errCh <- err
	}()

	scanner := bufio.NewScanner(pr)
	for {
		if !scanner.Scan() {
			cancel()
			// Report all three signals. The scan error alone says only "the pipe
			// ended"; the actual cause is whatever runSessionWorker returned,
			// which is why it is drained here rather than left in the channel.
			var runErr error
			select {
			case runErr = <-errCh:
			case <-time.After(10 * time.Second):
				runErr = errors.New("runSessionWorker did not return")
			}
			t.Fatalf("no handshake line on session-worker stdout\n  scan=%v\n  runSessionWorker=%v\n  stderr: %s",
				scanner.Err(), runErr, stderr.String())
		}
		if err := json.Unmarshal(scanner.Bytes(), &hs); err == nil && hs.Addr != "" {
			break
		}
	}

	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("runSessionWorker returned %v, want nil\nstderr: %s", err, stderr.String())
			}
		case <-time.After(10 * time.Second):
			t.Errorf("runSessionWorker did not return after ctx cancel\nstderr: %s", stderr.String())
		}
		_ = pw.Close()
	}
	t.Cleanup(stop)
	return hs, root, stop
}

// TestSessionWorkerPinsSessionID is the guard on the production session-id
// pinning: `gofer session-worker --session <uuid>` must create its ONE session
// under exactly that uuid. Dropping opts.SessionID from runSessionWorker's
// NewSession factory makes the session adopt a freshly minted uuid instead,
// which this test catches TWO ways: the handle the client sees (session/new)
// and the journal filename on disk would both disagree with the id the router
// keyed everything to.
//
// The endpoint-file assertions below are NOT a third pin check, and it would be
// an overclaim to count them as one. internal/worker writes that file from
// worker.Options.Session — the `--session` FLAG value — during Serve, before any
// session/new and independent of runner.Options.SessionID. Deleting the pin
// leaves it byte-identical (mutation-confirmed: only the handle and journal
// assertions fire). It is a Serve-wiring smoke check, kept because the endpoint
// file is what the router's adoption scan reads, and worker.Options.Session is
// separately load-bearing — but it does not cover the runner-side pin.
func TestSessionWorkerPinsSessionID(t *testing.T) {
	sessionID := uuid.Must(uuid.NewV7()).String()
	hs, root, _ := startSessionWorker(t, sessionID)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	c, err := daemon.Dial(ctx, hs.Addr, "")
	if err != nil {
		t.Fatalf("Dial(%s): %v", hs.Addr, err)
	}
	defer func() { _ = c.Close() }()
	go func() {
		for range c.Notifications() {
		}
	}()

	res, err := c.Call(ctx, acp.MethodSessionNew, acp.NewSessionRequest{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}
	var sess acp.NewSessionResponse
	if err := json.Unmarshal(res, &sess); err != nil {
		t.Fatalf("unmarshal NewSessionResponse: %v", err)
	}

	// (1) The registered handle carries the pinned id verbatim.
	if sess.SessionID != sessionID {
		t.Errorf("session/new SessionID = %q, want the pinned --session %q\n"+
			"runSessionWorker's NewSession factory must set runner.Options.SessionID = --session (design Option A)",
			sess.SessionID, sessionID)
	}

	// (2) The journal on disk is keyed by the pinned id. This is the half that
	//     actually bites in production: the router keys the socket, endpoint
	//     file, and lock by the uuid it generated, so a self-minted session id
	//     leaves artifacts and handle silently disagreeing.
	journals, err := filepath.Glob(filepath.Join(root, "sessions", "*", "*.jsonl"))
	if err != nil {
		t.Fatalf("glob journals: %v", err)
	}
	if len(journals) != 1 {
		t.Fatalf("found %d journals under %s, want exactly 1: %v", len(journals), root, journals)
	}
	if got := strings.TrimSuffix(filepath.Base(journals[0]), ".jsonl"); got != sessionID {
		t.Errorf("journal id on disk = %q, want the pinned --session %q", got, sessionID)
	}

	// (3) Serve-wiring smoke check — NOT a pin assertion (see the doc comment).
	//     The endpoint file the router's adoption scan reads exists under the
	//     pinned uuid and is internally consistent. Written from
	//     worker.Options.Session, so it survives deleting the runner-side pin.
	ep, err := daemon.ReadWorkerEndpoint(sessionID)
	if err != nil {
		t.Fatalf("ReadWorkerEndpoint(%s): %v", sessionID, err)
	}
	// Addr and PID are near-tautological here: this test runs the worker
	// IN-PROCESS, so both sides read the same local values. They pin the
	// endpoint file's shape, not cross-process agreement, and are not counted as
	// coverage of anything the router depends on.
	if ep.Addr != hs.Addr {
		t.Errorf("endpoint Addr = %q, want the handshake addr %q", ep.Addr, hs.Addr)
	}
	if ep.PID != os.Getpid() || hs.PID != os.Getpid() {
		t.Errorf("endpoint PID = %d, handshake PID = %d, want %d", ep.PID, hs.PID, os.Getpid())
	}
	if ep.WireVersion != daemon.WireVersion {
		t.Errorf("endpoint WireVersion = %d, want %d", ep.WireVersion, daemon.WireVersion)
	}
}

// TestSessionWorkerCleanShutdownClearsEndpoint confirms the rest of
// runSessionWorker's setup survives too: a cancelled worker returns nil and
// removes its endpoint file, so a restarting router does not try to adopt a
// dead worker.
//
// DO NOT delete this as redundant with the pin test. Its unique assertion is
// that runSessionWorker returns **nil** on context cancellation — a clean
// shutdown, not an error. The pin test's helper cancels too, but a non-nil
// return there surfaces as a t.Errorf inside cleanup, which is easy to miss and
// easy to "fix" by loosening. Here it is the point of the test. The endpoint
// removal it also checks is what stops a restarting router from adopting a
// corpse.
func TestSessionWorkerCleanShutdownClearsEndpoint(t *testing.T) {
	sessionID := uuid.Must(uuid.NewV7()).String()
	_, _, stop := startSessionWorker(t, sessionID)

	if _, err := daemon.ReadWorkerEndpoint(sessionID); err != nil {
		t.Fatalf("ReadWorkerEndpoint(%s) while serving: %v", sessionID, err)
	}
	stop() // asserts a nil return
	if _, err := daemon.ReadWorkerEndpoint(sessionID); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ReadWorkerEndpoint after clean shutdown err = %v, want os.ErrNotExist", err)
	}
}

// TestSessionWorkerRequiresSession pins the hard-fail on a missing --session:
// there is no self-generated fallback, because a self-minted id would desync
// the worker's socket/endpoint/lock keying from the router's.
func TestSessionWorkerRequiresSession(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := runSessionWorker(context.Background(), []string{"--root", t.TempDir()}, &stdout, &stderr)
	if err == nil {
		t.Fatal("runSessionWorker without --session = nil error, want a hard failure")
	}
	if !strings.Contains(err.Error(), "--session") {
		t.Errorf("error = %q, want it to name the required --session flag", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing (stdout carries only the handshake line)", stdout.String())
	}
}
