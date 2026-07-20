package router

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider/faux"
	"github.com/jedwards1230/agent-sdk-go/runner"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/wirestream"
	"github.com/jedwards1230/gofer/internal/worker"
)

// Env keys the faux-worker re-exec seam passes to its child. They stand in for
// the real `gofer session-worker` flags (--session/--root/--version): a re-exec
// of the test binary cannot take flags without colliding with `go test`'s own.
const (
	envWorkerMode    = "GOFER_ROUTER_TEST_WORKER"
	envWorkerRoot    = "GOFER_ROUTER_TEST_ROOT"
	envWorkerSession = "GOFER_ROUTER_TEST_SESSION"
	// envWorkerVersion stamps the build version the faux worker reports over
	// gofer/hello and its endpoint file — the seam a version-skew test uses to
	// make a worker look like it came from a different build.
	envWorkerVersion = "GOFER_ROUTER_TEST_VERSION"
	// envWorkerSessionOverride, when non-empty, makes the faux worker pin its
	// SESSION CONTENT (the id its runner adopts, and therefore the id it echoes
	// back on session/new) to a DIFFERENT value than envWorkerSession — which
	// still keys the worker's socket/endpoint/lock files, exactly like the real
	// `gofer session-worker` always does. This reproduces the one way the two
	// can legitimately diverge in the field (a broken pinning bridge on the
	// worker binary), without touching how the router discovers/dials the
	// worker. See TestCreateRefusesSessionIDMismatch.
	envWorkerSessionOverride = "GOFER_ROUTER_TEST_SESSION_OVERRIDE"
	// envParkedProcess re-execs the test binary as an INERT process that just
	// blocks until it is signalled. It backs [parkedPID]: a test that plants a
	// fake worker endpoint needs a REAL, live, killable pid to advertise that is
	// not the test binary's own — see parkedPID for why that distinction matters.
	envParkedProcess = "GOFER_ROUTER_TEST_PARKED"
)

// TestMain re-execs the test binary as a faux-provider worker when
// GOFER_ROUTER_TEST_WORKER is set, so the router's spawn seam can start REAL,
// killable worker processes (an in-process goroutine could not be `kill -9`'d,
// which crash isolation must prove). Any other invocation runs the tests.
func TestMain(m *testing.M) {
	if os.Getenv(envWorkerMode) == "1" {
		runFauxWorker()
		return
	}
	if os.Getenv(envParkedProcess) == "1" {
		// An inert stand-in process (see parkedPID). It exists only to own a pid,
		// so it does nothing but stay alive until it is signalled. The sleep is a
		// backstop far longer than any test yet bounded, so a test that somehow
		// failed to reap it still cannot leave a process behind indefinitely; a
		// bare select{} would instead trip the runtime's deadlock detector and
		// exit immediately, defeating the whole point.
		time.Sleep(10 * time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// parkedPID starts a real, inert child process and returns its pid plus a
// waitKilled func that blocks until that process has actually died and reaps it.
//
// It exists because the fake-worker endpoint tests need a live pid to advertise,
// and the obvious choice — os.Getpid() — is exactly the pid a router must never
// signal: killHandleProcess SIGKILLs an adopted handle's endpoint pid, so
// advertising the test binary's own pid would make killWorkers kill the test
// run. (adoptWorker's self-pid guard now defends the daemon against precisely
// that, which is why those tests previously had to skip killWorkers entirely and
// left the adopted kill path uncovered.) A separate parked process gives that
// path a real, safe target to actually kill.
//
// Nothing leaks: the returned waitKilled reaps the child, and a t.Cleanup kills
// and reaps it unconditionally for the paths (an early t.Fatal, a test that
// never kills it) that never call waitKilled.
func parkedPID(t *testing.T) (pid int, waitKilled func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), envParkedProcess+"=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start parked process: %v", err)
	}

	// cmd.Wait must run exactly once (a second call errors), but BOTH waitKilled
	// and the cleanup need to reap — whichever gets there first does it.
	var (
		once    sync.Once
		waitErr error
	)
	reap := func() { once.Do(func() { waitErr = cmd.Wait() }) }
	t.Cleanup(func() {
		_ = cmd.Process.Kill() // no-op if it is already dead (os.ErrProcessDone)
		reap()
	})

	return cmd.Process.Pid, func() {
		t.Helper()
		// Reap on a goroutine so a process that was NOT killed fails the test
		// instead of hanging it until the go test deadline.
		done := make(chan struct{})
		go func() { reap(); close(done) }()
		select {
		case <-done:
			// A killed process yields an *exec.ExitError; a nil error would mean
			// it exited on its own, i.e. nothing actually signalled it.
			if waitErr == nil {
				t.Errorf("parked process %d exited cleanly; the adopted kill path did not SIGKILL it", cmd.Process.Pid)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("parked process %d still alive 10s after the kill; the adopted kill path did not signal it", cmd.Process.Pid)
		}
	}
}

// runFauxWorker is the worker half of the re-exec: it hosts a single-session
// daemon whose sessions run against the SDK's deterministic faux provider (no
// network), over the shared store root, and serves until SIGKILL. It mirrors
// internal/worker's own test harness, including the session-id pinning the real
// `gofer session-worker` performs: runner.Options.SessionID pins the session id
// to the router-supplied GOFER_ROUTER_TEST_SESSION uuid so the worker's socket/
// endpoint/lock keying matches what the router expects (design Option A).
func runFauxWorker() {
	root := os.Getenv(envWorkerRoot)
	sessionID := os.Getenv(envWorkerSession)
	if sessionID == "" {
		os.Exit(2) // the router must pass --session (via env in this seam)
	}
	// pinnedID is the id the worker's session actually adopts and echoes back on
	// session/new. It matches sessionID (the file-keying id) unless the seam
	// stamped a deliberate override — see envWorkerSessionOverride.
	pinnedID := sessionID
	if override := os.Getenv(envWorkerSessionOverride); override != "" {
		pinnedID = override
	}
	sup, err := supervisor.New(supervisor.Config{
		Root: root,
		NewSession: func(ctx context.Context, opts runner.Options) (supervisor.Session, error) {
			opts.SessionID = pinnedID
			opts.Model = "faux"
			opts.Provider = faux.New(multiTurnScript())
			return runner.New(ctx, opts)
		},
	})
	if err != nil {
		os.Exit(1)
	}
	// Blocks until the process is killed (the router SIGKILLs a worker on Kill/
	// Archive or a crash-isolation Process.Kill; Close does NOT kill it). Stdout
	// (redirected to the per-worker log by SpawnDetached) carries the handshake.
	if err := worker.Serve(context.Background(), worker.Options{
		Supervisor:   sup,
		Session:      sessionID,
		DefaultModel: "faux",
		// Empty unless the seam stamped one; worker.Serve then reports an empty
		// binaryVersion, exactly like a worker predating version reporting.
		Version: os.Getenv(envWorkerVersion),
		Stdout:  os.Stdout,
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

// fauxWorkerOptions shapes what a spawned faux worker reports about itself. It
// is a struct rather than positional arguments so the seam stays general as
// later slices need more knobs (the 3b mixed-version demo shares this helper).
type fauxWorkerOptions struct {
	// Version is the build version the worker reports over gofer/hello and its
	// endpoint file. Empty leaves the worker unidentified, which is what a
	// worker built before version reporting looks like.
	Version string
	// SessionIDOverride, when non-empty, makes the worker's session adopt (and
	// echo back on session/new) THIS id instead of the router-pinned uuid —
	// simulating a worker whose pinning bridge is broken. The worker's
	// socket/endpoint/lock files stay keyed by the pinned uuid regardless (see
	// envWorkerSessionOverride) — only the in-protocol session/new response
	// diverges, which is the exact shape router.go's mismatch guard exists for.
	SessionIDOverride string
}

// fauxWorkerSeam returns a Config.NewWorkerCmd that re-execs this test binary as
// a faux worker rooted at root, forwarding the router-pinned session uuid so the
// worker pins its session id and keys its runtime files to the same value. The
// worker reports no build version; use fauxWorkerSeamOpts to stamp one.
func fauxWorkerSeam(root string) func(ctx context.Context, sessionID, model, cwd string) *exec.Cmd {
	return fauxWorkerSeamOpts(root, fauxWorkerOptions{})
}

// fauxWorkerSeamOpts is fauxWorkerSeam with the worker's self-reported identity
// under the test's control — the injection point for binary-version skew.
func fauxWorkerSeamOpts(root string, opts fauxWorkerOptions) func(ctx context.Context, sessionID, model, cwd string) *exec.Cmd {
	return func(ctx context.Context, sessionID, _, _ string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0])
		cmd.Env = append(os.Environ(),
			envWorkerMode+"=1",
			envWorkerRoot+"="+root,
			envWorkerSession+"="+sessionID,
			envWorkerVersion+"="+opts.Version,
			envWorkerSessionOverride+"="+opts.SessionIDOverride,
		)
		// Stdio is left unset here — daemon.SpawnDetached redirects the worker's
		// stdout+stderr (including the handshake line) to its per-worker log file.
		return cmd
	}
}

// TestCrashIsolation is the shippable value of this slice: killing one session's
// worker process leaves the daemon and every other session serving, surfaces the
// dead session as offline, and delivers a terminal error to a peer attached to
// it. It drives the real double-hop — a worker-mode daemon over httptest, dialed
// by real clients — and kills a worker mid-fleet with SIGKILL.
func TestCrashIsolation(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()
	sup, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	// Detached workers survive Supervisor.Close (design §3), so terminate any
	// still-live worker processes before Close to avoid leaking orphaned faux
	// workers past the test binary's exit.
	t.Cleanup(func() {
		killWorkers(sup)
		_ = sup.Close()
	})

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

// shortRuntimeDir points XDG_RUNTIME_DIR at a short-rooted per-user runtime dir
// so each worker's unix socket ([daemon.WorkerSocketPath]) stays within its
// ~103-byte budget — a deep macOS t.TempDir() would overflow it. The worker
// processes inherit the env through the re-exec seam.
func shortRuntimeDir(t *testing.T) {
	t.Helper()
	base := "/tmp"
	if _, err := os.Stat(base); err != nil {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, "gfrr")
	if err != nil {
		t.Fatalf("mkdir short runtime dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("XDG_RUNTIME_DIR", dir)
}

// killWorkers force-terminates every live worker process. Detached workers
// survive Supervisor.Close (design §3), so a test that spawned them must reap
// them itself or they leak as orphans reparented to pid 1.
func killWorkers(s *Supervisor) {
	s.mu.Lock()
	handles := make([]*workerHandle, 0, len(s.workers))
	for _, h := range s.workers {
		handles = append(handles, h)
	}
	s.mu.Unlock()
	for _, h := range handles {
		killHandleProcess(h)
	}
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
