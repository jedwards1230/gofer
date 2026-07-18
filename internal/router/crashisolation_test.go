package router

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider/faux"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/wirestream"
	"github.com/jedwards1230/gofer/internal/worker"
)

// TestMain re-execs the test binary as a faux-provider worker when
// GOFER_ROUTER_TEST_WORKER is set, so the router's spawn seam can start REAL,
// killable worker processes (an in-process goroutine could not be `kill -9`'d,
// which crash isolation must prove). Any other invocation runs the tests.
func TestMain(m *testing.M) {
	if os.Getenv("GOFER_ROUTER_TEST_WORKER") == "1" {
		runFauxWorker()
		return
	}
	os.Exit(m.Run())
}

// runFauxWorker is the worker half of the re-exec: it hosts a single-session
// daemon whose sessions run against the SDK's deterministic faux provider (no
// network), over the shared store root, and serves until SIGKILL. It mirrors
// internal/worker's own test harness.
func runFauxWorker() {
	root := os.Getenv("GOFER_ROUTER_TEST_ROOT")
	store, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		os.Exit(1)
	}
	sup, err := supervisor.New(supervisor.Config{
		Root:  root,
		Store: store,
		NewSession: func(ctx context.Context, opts runner.Options) (supervisor.Session, error) {
			opts.Store = store
			opts.Model = "faux"
			opts.Provider = faux.New(multiTurnScript())
			return runner.New(ctx, opts)
		},
		ResumeSession: func(ctx context.Context, id string, opts runner.Options) (supervisor.Session, error) {
			opts.Store = store
			opts.Model = "faux"
			opts.Provider = faux.New(multiTurnScript())
			return runner.Resume(ctx, id, opts)
		},
	})
	if err != nil {
		os.Exit(1)
	}
	// Blocks until the process is killed (the router SIGKILLs workers on Close or
	// on a crash-isolation Process.Kill). Stdout carries only the handshake line.
	if err := worker.Serve(context.Background(), worker.Options{
		Supervisor:   sup,
		DefaultModel: "faux",
		Stdout:       os.Stdout,
	}); err != nil {
		os.Exit(1)
	}
}

// multiTurnScript repeats the canonical faux turn so a single session can run
// several turns (each Stream call consumes one turn); the test drives some
// sessions more than once.
func multiTurnScript() faux.Script {
	base := faux.Default().Turns
	turns := make([]faux.Turn, 0, len(base)*8)
	for i := 0; i < 8; i++ {
		turns = append(turns, base...)
	}
	return faux.Script{Turns: turns}
}

// fauxWorkerSeam returns a Config.NewWorkerCmd that re-execs this test binary as
// a faux worker rooted at root.
func fauxWorkerSeam(root string) func(ctx context.Context, model, cwd string) *exec.Cmd {
	return func(ctx context.Context, _, _ string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0])
		cmd.Env = append(os.Environ(),
			"GOFER_ROUTER_TEST_WORKER=1",
			"GOFER_ROUTER_TEST_ROOT="+root,
		)
		// Worker stderr is discarded (nil → /dev/null); the helper logs nothing
		// anyway. Stdout is owned by the router via StdoutPipe.
		return cmd
	}
}

// TestCrashIsolation is the shippable value of this slice: killing one session's
// worker process leaves the daemon and every other session serving, surfaces the
// dead session as offline, and delivers a terminal error to a peer attached to
// it. It drives the real double-hop — a worker-mode daemon over httptest, dialed
// by real clients — and kills a worker mid-fleet with SIGKILL.
func TestCrashIsolation(t *testing.T) {
	root := t.TempDir()
	sup, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	d := daemon.New(sup, daemon.Config{DefaultModel: "faux"})
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")

	ctx := context.Background()

	// client1 drives control + session/new + prompts; it must drain
	// notifications while a session/prompt streams (Client contract).
	c1 := mustDial(t, ctx, addr)
	go func() {
		for range c1.Notifications() {
		}
	}()

	dirA, dirB := t.TempDir(), t.TempDir()
	sessA := mustNewSession(t, ctx, c1, dirA)
	sessB := mustNewSession(t, ctx, c1, dirB)
	if sessA == sessB {
		t.Fatalf("two sessions got the same id %q", sessA)
	}

	// Both sessions run a full turn to prove they are live and to write journals.
	mustPrompt(t, ctx, c1, sessA)
	mustPrompt(t, ctx, c1, sessB)

	// Crash worker A: SIGKILL its process directly, bypassing router.Kill, so the
	// reaper observes an UNEXPECTED exit — a genuine crash, not a teardown.
	crashWorker(t, sup, sessA)
	waitOffline(t, sup, sessA)

	// (a) The daemon still serves: gofer/roster answers, and A is gone from it.
	roster := mustRoster(t, ctx, c1)
	if _, ok := roster[sessA]; ok {
		t.Errorf("crashed session %s still in live roster", sessA)
	}
	if !roster[sessB].Live {
		t.Errorf("survivor %s missing/not live in roster after sibling crash", sessB)
	}

	// (b) The surviving session still works: another full turn on B succeeds.
	mustPrompt(t, ctx, c1, sessB)

	// (c) The killed session shows offline in List (gofer/ps): union of live ∪ disk.
	ps := mustPS(t, ctx, c1)
	a, okA := ps[sessA]
	if !okA {
		t.Fatalf("crashed session %s absent from List (gofer/ps)", sessA)
	}
	if a.Live {
		t.Errorf("crashed session %s reported Live=true in List, want offline", sessA)
	}
	if b, okB := ps[sessB]; !okB || !b.Live {
		t.Errorf("survivor %s in List = %+v (present=%v), want Live=true", sessB, b, okB)
	}

	// (d) A peer attached to the crashed session sees a terminal error: a
	// wirestream client attaching to A and driving a turn observes a reconstructed
	// fatal session.error, because the session/prompt to the dead worker fails.
	assertTerminalError(t, ctx, addr, sessA)
}

// crashWorker SIGKILLs the process backing sessionID, simulating a panic/OOM/kill
// without going through router.Kill (so stopped stays false and the reaper treats
// it as a crash).
func crashWorker(t *testing.T, s *Supervisor, sessionID string) {
	t.Helper()
	s.mu.Lock()
	h := s.workers[sessionID]
	s.mu.Unlock()
	if h == nil {
		t.Fatalf("no live worker for session %s to crash", sessionID)
	}
	// Tolerate a worker that already exited between the lookup and the kill —
	// os.ErrProcessDone is not a failure to simulate a crash, it IS one.
	if err := h.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("kill worker %s: %v", sessionID, err)
	}
}

// waitOffline blocks until the router has reaped sessionID's handle (dropped from
// the live registry), or fails after a deadline.
func waitOffline(t *testing.T, s *Supervisor, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := s.get(sessionID); !ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("router did not reap crashed worker for session %s within deadline", sessionID)
}

func mustDial(t *testing.T, ctx context.Context, addr string) *daemon.Client {
	t.Helper()
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := daemon.Dial(dctx, addr, "")
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func mustNewSession(t *testing.T, ctx context.Context, c *daemon.Client, cwd string) string {
	t.Helper()
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := c.Call(cctx, acp.MethodSessionNew, acp.NewSessionRequest{Cwd: cwd})
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}
	var resp acp.NewSessionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode NewSessionResponse: %v", err)
	}
	if resp.SessionID == "" {
		t.Fatal("session/new returned an empty id")
	}
	return resp.SessionID
}

func mustPrompt(t *testing.T, ctx context.Context, c *daemon.Client, sessionID string) {
	t.Helper()
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := c.Call(cctx, acp.MethodSessionPrompt, acp.PromptRequest{
		SessionID: sessionID,
		Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
	}); err != nil {
		t.Fatalf("session/prompt %s: %v", sessionID, err)
	}
}

// mustRoster / mustPS decode the gofer-native roster/ps rows into the exported
// wire type keyed by id.
func mustRoster(t *testing.T, ctx context.Context, c *daemon.Client) map[string]wirestream.SessionInfo {
	t.Helper()
	return callRows(t, ctx, c, "gofer/roster")
}

func mustPS(t *testing.T, ctx context.Context, c *daemon.Client) map[string]wirestream.SessionInfo {
	t.Helper()
	return callRows(t, ctx, c, "gofer/ps")
}

func callRows(t *testing.T, ctx context.Context, c *daemon.Client, method string) map[string]wirestream.SessionInfo {
	t.Helper()
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	raw, err := c.Call(cctx, method, nil)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	var rows []wirestream.SessionInfo
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode %s rows: %v", method, err)
	}
	out := make(map[string]wirestream.SessionInfo, len(rows))
	for _, r := range rows {
		out[r.ID] = r
	}
	return out
}

// assertTerminalError attaches a wirestream client to the crashed session and
// drives a turn, expecting a reconstructed fatal session.error — the terminal
// error an attached peer observes when its worker is gone.
func assertTerminalError(t *testing.T, ctx context.Context, addr, sessionID string) {
	t.Helper()
	c := mustDial(t, ctx, addr)
	rec := wirestream.New(c)
	t.Cleanup(func() { _ = rec.Close() })

	// RegisterFresh avoids a session/load first-reference (the session is dead;
	// there is nothing to replay) and marks the stream live for Send.
	rec.RegisterFresh(sessionID)
	sub, err := rec.Subscribe(ctx, sessionID)
	if err != nil {
		t.Fatalf("subscribe %s: %v", sessionID, err)
	}
	defer sub.Close()

	if err := rec.Send(ctx, sessionID, "boom"); err != nil {
		t.Fatalf("send to crashed session %s: %v", sessionID, err)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-sub.C:
			if !ok {
				t.Fatalf("stream for %s closed with no terminal session.error", sessionID)
			}
			if se, isErr := ev.(event.SessionError); isErr {
				if !se.Fatal {
					t.Errorf("session.error for %s not fatal: %+v", sessionID, se)
				}
				return // terminal error observed
			}
		case <-deadline:
			t.Fatalf("no terminal session.error for %s within deadline", sessionID)
		}
	}
}
