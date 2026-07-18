package worker_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/provider/faux"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/worker"
)

// newFauxSupervisor builds a Supervisor whose sessions are real
// [runner.Runner]s over the SDK's deterministic faux provider — no network —
// mirroring internal/daemon's own test harness (newTestSupervisorModelAtRoot).
func newFauxSupervisor(t *testing.T) *supervisor.Supervisor {
	t.Helper()
	root := t.TempDir()
	store, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		t.Fatalf("session.NewFileStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	sup, err := supervisor.New(supervisor.Config{
		Root:  root,
		Store: store,
		NewSession: func(ctx context.Context, opts runner.Options) (supervisor.Session, error) {
			opts.Store = store
			opts.Model = "faux"
			opts.Provider = faux.New(faux.Default())
			return runner.New(ctx, opts)
		},
		ResumeSession: func(ctx context.Context, id string, opts runner.Options) (supervisor.Session, error) {
			opts.Store = store
			opts.Model = "faux"
			opts.Provider = faux.New(faux.Default())
			return runner.Resume(ctx, id, opts)
		},
	})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	// Serve closes the supervisor on shutdown; this Cleanup double-closes
	// (idempotent) so a test that returns before Serve does still frees it.
	t.Cleanup(func() { _ = sup.Close() })
	return sup
}

// TestWorkerServeHandshakeAndSingleSession drives the worker in-process: it
// parses the handshake line exactly as the M6 router will (read stdout lines
// until one decodes), dials the advertised address, runs one full turn against
// the faux provider, and confirms the single-session cap rejects a second
// session/new.
func TestWorkerServeHandshakeAndSingleSession(t *testing.T) {
	sup := newFauxSupervisor(t)

	// An io.Pipe stands in for the worker's stdout, so the test reads the
	// handshake through the same "scan lines, decode JSON" path the router
	// uses — no os/exec.
	pr, pw := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Ready is the in-process seam: it must fire exactly once with the same
	// bound Handshake the worker writes to stdout. Capturing it here both
	// exercises the seam and lets the test assert stdout and Ready agree.
	readyCh := make(chan worker.Handshake, 1)

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- worker.Serve(ctx, worker.Options{
			Supervisor:   sup,
			DefaultModel: "faux",
			Version:      "test-version",
			Stdout:       pw,
			Ready:        func(h worker.Handshake) { readyCh <- h },
		})
	}()

	// Read the FIRST stdout line and decode it as a Handshake — the router's
	// discovery step.
	scanner := bufio.NewScanner(pr)
	if !scanner.Scan() {
		t.Fatalf("no handshake line on worker stdout: %v", scanner.Err())
	}
	var hs worker.Handshake
	if err := json.Unmarshal(scanner.Bytes(), &hs); err != nil {
		t.Fatalf("decode handshake %q: %v", scanner.Text(), err)
	}
	if hs.Addr == "" {
		t.Fatal("handshake Addr is empty")
	}
	if !strings.HasPrefix(hs.Addr, "127.0.0.1:") {
		t.Errorf("handshake Addr = %q, want a loopback address", hs.Addr)
	}
	if hs.PID != os.Getpid() {
		t.Errorf("handshake PID = %d, want %d", hs.PID, os.Getpid())
	}
	if hs.Version != "test-version" {
		t.Errorf("handshake Version = %q, want %q", hs.Version, "test-version")
	}

	// The Ready callback fired with the identical Handshake (same bound addr).
	select {
	case ready := <-readyCh:
		if ready != hs {
			t.Errorf("Ready Handshake = %+v, want the stdout handshake %+v", ready, hs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Ready callback did not fire")
	}

	dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
	defer dialCancel()
	c, err := daemon.Dial(dialCtx, hs.Addr, "")
	if err != nil {
		t.Fatalf("Dial(%s): %v", hs.Addr, err)
	}
	defer func() { _ = c.Close() }()

	// The Client contract requires draining notifications while a streaming
	// call (session/prompt) is in flight — see daemon.Client's doc.
	go func() {
		for range c.Notifications() {
		}
	}()

	newRes, err := c.Call(ctx, acp.MethodSessionNew, acp.NewSessionRequest{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}
	var sess acp.NewSessionResponse
	if err := json.Unmarshal(newRes, &sess); err != nil {
		t.Fatalf("unmarshal NewSessionResponse: %v", err)
	}

	// Run one full turn; faux.Default() terminates on its own (end_turn), so
	// this Call returns once the turn finishes.
	if _, err := c.Call(ctx, acp.MethodSessionPrompt, acp.PromptRequest{
		SessionID: sess.SessionID,
		Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
	}); err != nil {
		t.Fatalf("session/prompt: %v", err)
	}

	// A second session/new must be refused: the worker caps at one session.
	_, err = c.Call(ctx, acp.MethodSessionNew, acp.NewSessionRequest{Cwd: t.TempDir()})
	if err == nil {
		t.Fatal("second session/new succeeded, want a single-session-cap error")
	}
	var callErr *daemon.CallError
	if !errors.As(err, &callErr) {
		t.Fatalf("second session/new error = %v (%T), want a *daemon.CallError", err, err)
	}
	if !strings.Contains(callErr.Message, "session limit reached") {
		t.Errorf("second session/new error = %q, want it to mention the session limit", callErr.Message)
	}

	// Shut the worker down and confirm a clean return.
	cancel()
	select {
	case err := <-serveErr:
		if err != nil {
			t.Errorf("worker.Serve returned %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker.Serve did not return after ctx cancel")
	}
	_ = pw.Close()
}
