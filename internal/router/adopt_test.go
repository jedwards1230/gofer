package router

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// writeEndpoint writes a worker endpoint file for id with the given pid and
// wire version, addr pointing at a (real or bogus) socket path. It is the seam
// the failure-matrix tests use to plant an endpoint without a live worker.
func writeEndpoint(t *testing.T, id string, pid, wireVersion int, addr string) {
	t.Helper()
	if err := daemon.WriteWorkerEndpoint(id, daemon.WorkerEndpoint{
		Addr:          addr,
		PID:           pid,
		BinaryVersion: "test",
		WireVersion:   wireVersion,
		StartedAt:     time.Now(),
	}); err != nil {
		t.Fatalf("WriteWorkerEndpoint(%s): %v", id, err)
	}
}

// endpointExists reports whether id's endpoint file is still on disk — the
// probe the GC / leave-in-place failure-matrix assertions use.
func endpointExists(t *testing.T, id string) bool {
	t.Helper()
	_, err := daemon.ReadWorkerEndpoint(id)
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	t.Fatalf("ReadWorkerEndpoint(%s): %v", id, err)
	return false
}

// deadPID returns a pid guaranteed not to be alive: it forks `true`, waits for
// it to exit, and returns its now-reaped pid. A signal-0 probe on it reports
// ESRCH (gone), the "dead worker" the §4 matrix GCs.
func deadPID(t *testing.T) int {
	t.Helper()
	// pid 1 is always alive; instead spawn-and-reap a trivial process so its pid
	// is genuinely gone. Use a very large pid as a simpler, deterministic dead
	// pid: pids that high are effectively never allocated on the test host.
	// ProcessAlive on it returns false (ESRCH).
	const improbablePID = 0x7ffffffe
	if daemon.ProcessAlive(improbablePID) {
		t.Skip("host has a live process at the improbable pid; skipping dead-pid case")
	}
	return improbablePID
}

// TestAdoptStaleFailureMatrix covers the §4 adopt/stale decisions that need no
// live worker: a dead pid and a live-pid-but-dead-socket both GC the endpoint.
//
// The wire-skew row INVERTS the pre-Phase-3 expectation on purpose. The endpoint
// file's version fields used to short-circuit the scan — a skewed hint meant
// "leave it alone, do not even dial" — so a skewed endpoint over a dead socket
// survived the scan. Under the Phase-3 policy no skew class refuses adoption, so
// the hint is advisory only and the scan proceeds to the dial, which is refused,
// which makes this exactly the stale-socket case: GC'd like any other.
func TestAdoptStaleFailureMatrix(t *testing.T) {
	tests := []struct {
		name       string
		pid        func(t *testing.T) int
		wire       int
		addr       func(id string) string
		wantExists bool // endpoint file still present after the scan?
	}{
		{
			name:       "dead pid is GC'd",
			pid:        deadPID,
			wire:       daemon.WireVersion,
			addr:       func(id string) string { return "unix:///nonexistent/" + id + ".sock" },
			wantExists: false,
		},
		{
			name:       "live pid but dead socket (dial refused) is GC'd",
			pid:        func(t *testing.T) int { return os.Getpid() },
			wire:       daemon.WireVersion,
			addr:       func(id string) string { return "unix:///nonexistent/" + id + ".sock" },
			wantExists: false,
		},
		{
			// Inverted in Phase 3: the skewed HINT no longer short-circuits the
			// dial, so a dead socket behind it is plain stale residue.
			name:       "wire-skew hint over a dead socket is GC'd (the hint no longer gates the dial)",
			pid:        func(t *testing.T) int { return os.Getpid() },
			wire:       daemon.WireVersion + 1,
			addr:       func(id string) string { return "unix:///nonexistent/" + id + ".sock" },
			wantExists: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			shortRuntimeDir(t)
			root := t.TempDir()
			id := uuid.Must(uuid.NewV7()).String()
			writeEndpoint(t, id, tc.pid(t), tc.wire, tc.addr(id))

			sup, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
			if err != nil {
				t.Fatalf("router.New: %v", err)
			}
			t.Cleanup(func() {
				killWorkers(sup)
				_ = sup.Close()
			})

			// None of these worker states is adoptable, so the roster stays empty.
			if _, ok := sup.get(id); ok {
				t.Errorf("session %s was adopted, want left offline/unadopted", id)
			}
			if got := endpointExists(t, id); got != tc.wantExists {
				t.Errorf("endpoint present after scan = %v, want %v", got, tc.wantExists)
			}
		})
	}
}

// TestAdoptGCsStaleArtifacts proves the scan sweeps a crashed worker's on-disk
// leftovers: a dead-pid endpoint plus a leftover socket file are both removed
// (the <uuid>.lock is deliberately left — a dead worker's flock auto-releases,
// and unlinking it would race a concurrent holder).
func TestAdoptGCsStaleArtifacts(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()
	id := uuid.Must(uuid.NewV7()).String()

	// A crashed worker's residue: a socket file, an endpoint naming a dead pid.
	sockPath, err := daemon.WorkerSocketPath(id)
	if err != nil {
		t.Fatalf("WorkerSocketPath: %v", err)
	}
	if mkErr := os.MkdirAll(filepath.Dir(sockPath), 0o700); mkErr != nil {
		t.Fatalf("mkdir workers dir: %v", mkErr)
	}
	if wErr := os.WriteFile(sockPath, nil, 0o600); wErr != nil {
		t.Fatalf("write leftover socket: %v", wErr)
	}
	writeEndpoint(t, id, deadPID(t), daemon.WireVersion, "unix://"+sockPath)

	sup, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	t.Cleanup(func() {
		killWorkers(sup)
		_ = sup.Close()
	})

	if endpointExists(t, id) {
		t.Errorf("stale endpoint for %s not GC'd during the scan", id)
	}
	if _, statErr := os.Stat(sockPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("stale socket %s not GC'd during the scan (stat err = %v)", sockPath, statErr)
	}
}

// fakeHelloResponder handles a single gofer/hello request and returns the
// response object to send back (a JSON-RPC result or error keyed by reqID), or
// nil to send nothing (simulating an unresponsive worker).
type fakeHelloResponder func(reqID json.RawMessage) any

// startFakeWorker binds a worker unix socket for id, writes a WIRE-COMPATIBLE
// endpoint file (so the pre-dial hint passes and the decision reaches the
// post-dial gofer/hello check), and serves a minimal JSON-RPC-over-WebSocket
// endpoint that answers gofer/hello via respond. It is the seam for the §4
// post-dial matrix rows (a live pid + dialable socket whose handshake fails or
// skews), which a real worker — always speaking the router's own wire — cannot
// reproduce.
//
// pid is optional and defaults to the TEST BINARY's own pid, which is fine for a
// test that never exercises the kill path. A test that DOES kill its adopted
// handles must pass a [parkedPID] instead: killHandleProcess SIGKILLs an adopted
// handle's endpoint pid, so the default would have the test kill itself — and
// with adoptWorker's self-pid guard it would instead be silently recorded as pid
// 0, leaving the kill path untested.
func startFakeWorker(t *testing.T, id string, respond fakeHelloResponder, pid ...int) {
	t.Helper()
	sockPath, err := daemon.WorkerSocketPath(id)
	if err != nil {
		t.Fatalf("WorkerSocketPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatalf("mkdir workers dir: %v", err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		for {
			var env struct {
				Method string          `json:"method"`
				ID     json.RawMessage `json:"id"`
			}
			if err := wsjson.Read(r.Context(), c, &env); err != nil {
				return
			}
			switch {
			case env.Method == "gofer/hello":
				if resp := respond(env.ID); resp != nil {
					_ = wsjson.Write(r.Context(), c, resp)
				}
			case len(env.ID) > 0:
				// Every OTHER request gets a prompt application error. A fake
				// worker that simply ignored them would wedge an adopting router
				// on its bounded session/load Call for the full wireCallTimeout;
				// erroring keeps the "adopting live-only" path fast. Notifications
				// (no id) are dropped, as a real peer would.
				_ = wsjson.Write(r.Context(), c, jsonRPCError(env.ID, -32000, "fake worker: "+env.Method+" unimplemented"))
			}
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = ln.Close() })

	advertisedPID := os.Getpid()
	if len(pid) > 0 {
		advertisedPID = pid[0]
	}
	writeEndpoint(t, id, advertisedPID, daemon.WireVersion, "unix://"+sockPath)
}

// jsonRPCResult builds a JSON-RPC success response object for reqID.
func jsonRPCResult(reqID json.RawMessage, result any) any {
	return map[string]any{"jsonrpc": "2.0", "id": reqID, "result": result}
}

// jsonRPCError builds a JSON-RPC error response object for reqID.
func jsonRPCError(reqID json.RawMessage, code int, msg string) any {
	return map[string]any{"jsonrpc": "2.0", "id": reqID, "error": map[string]any{"code": code, "message": msg}}
}

// TestAdoptHelloFailureLeavesWorkerUnadopted covers the §4 row that needs a
// LIVE, dialable worker socket whose handshake FAILS: the router learned nothing
// about the worker at all (which is not the same as learning it is old), so it
// does not route to it — and, unlike the stale/dead cases, does NOT GC its
// artifacts: the pid is live and the worker holds a real session.
//
// This is the ONLY post-dial row that still refuses adoption. Its former
// companion — a handshake reporting a skewed wire version — now adopts; see
// TestAdoptWireSkewedWorkerRefusesNewWork.
func TestAdoptHelloFailureLeavesWorkerUnadopted(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()
	id := uuid.Must(uuid.NewV7()).String()
	startFakeWorker(t, id, func(reqID json.RawMessage) any {
		return jsonRPCError(reqID, -32000, "worker degraded")
	})

	sup, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	if _, ok := sup.get(id); ok {
		t.Errorf("session %s adopted despite a failing handshake", id)
	}
	// LEAVE, do NOT GC: the live worker keeps its endpoint for a future router.
	if !endpointExists(t, id) {
		t.Errorf("endpoint for %s was GC'd; a live-but-unadoptable worker must be left in place", id)
	}
}

// TestAdoptPreHelloWorkerIsAdoptedUnidentified covers the ONE handshake error
// that is not "unresponsive": a worker built BEFORE gofer/hello existed replies
// method-not-found, which [daemon.Client.Hello] maps to
// daemon.ErrHelloUnsupported.
//
// Leaving it unadopted would brick every pre-handshake worker forever — the very
// outcome this slice's skewUnknown policy exists to avoid — so the router
// synthesizes a zero HelloResult and adopts it. Its absent wire version puts it
// on the STRICT side of the policy (skewWire): the session is observable and can
// finish, but takes no new work.
func TestAdoptPreHelloWorkerIsAdoptedUnidentified(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()
	id := uuid.Must(uuid.NewV7()).String()
	pid, waitKilled := parkedPID(t)
	startFakeWorker(t, id, func(reqID json.RawMessage) any {
		// -32601 method-not-found: exactly what a worker with no gofer/hello
		// handler replies.
		return jsonRPCError(reqID, -32601, "method not found: gofer/hello")
	}, pid)

	sup, err := New(Config{Root: root, Version: "router-build", NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	t.Cleanup(func() {
		killWorkers(sup)
		_ = sup.Close()
	})

	h, ok := sup.get(id)
	if !ok {
		t.Fatalf("pre-hello worker %s was not adopted; adopting it is the whole point of ErrHelloUnsupported", id)
	}
	if h.binaryVersion != "" || h.wireVersion != 0 {
		t.Errorf("adopted handle versions = (%q, %d), want the zero HelloResult (%q, 0)", h.binaryVersion, h.wireVersion, "")
	}
	if h.skew != skewWire {
		t.Errorf("adopted handle skew = %v, want skewWire (an unidentifiable wire is the strict side)", h.skew)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sup.Send(ctx, id, "another turn"); !errors.Is(err, ErrWorkerSkewed) {
		t.Errorf("Send to a pre-hello worker: err = %v, want errors.Is ErrWorkerSkewed", err)
	}
	// Adopted, so it is still killable — the endpoint is NOT stale residue.
	if !endpointExists(t, id) {
		t.Errorf("endpoint for adopted pre-hello worker %s was GC'd", id)
	}
	killWorkers(sup)
	waitKilled()
}

// TestAdoptRefusesToRecordItsOwnPIDForKill is the daemon-suicide guard.
//
// An adopted handle has no *exec.Cmd, so Kill/Archive best-effort SIGKILL the pid
// its ENDPOINT FILE advertised. If a file ever names the ROUTER'S OWN pid, one
// `gofer kill <session>` would SIGKILL the daemon and take routing down for every
// session. Adoption therefore records pid 0 for that case, which
// killHandleProcess's `h.pid > 0` check turns into a no-op.
//
// The test proves the no-op the only way that is safe: if the guard regressed,
// killWorkers here would SIGKILL the test binary and the run would die.
func TestAdoptRefusesToRecordItsOwnPIDForKill(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()
	id := uuid.Must(uuid.NewV7()).String()
	// No pid argument: startFakeWorker defaults to advertising THIS process's
	// pid, which is precisely the hazard under test.
	startFakeWorker(t, id, func(reqID json.RawMessage) any {
		return jsonRPCResult(reqID, daemon.HelloResult{
			BinaryVersion:      "router-build",
			WireVersion:        daemon.WireVersion,
			ACPProtocolVersion: 1,
		})
	})

	sup, err := New(Config{Root: root, Version: "router-build", NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	h, ok := sup.get(id)
	if !ok {
		t.Fatalf("worker %s was not adopted", id)
	}
	if h.pid != 0 {
		t.Fatalf("adopted handle pid = %d, want 0: the endpoint advertised the router's own pid (%d), which must never be signalled", h.pid, os.Getpid())
	}
	// Survives the kill path: with pid 0 this is a no-op, and reaching the next
	// line at all is the assertion.
	killWorkers(sup)
	killHandleProcess(h)
}

// TestAdoptWireSkewedWorkerRefusesNewWork is the INVERSION this slice ships, and
// the reason the old "post-dial wire skew leaves the worker unadopted" row is
// gone: a wire-skewed worker is now ADOPTED — it holds a live session that must
// stay observable, replyable, and finishable (design §6's compat window) — and
// the router instead refuses to give it NEW work.
//
// So the assertion moves from "is it in the roster" to "what will the router ask
// of it": adopted and classified skewWire, with Send and SetModel refused as a
// typed ErrWorkerSkewed.
func TestAdoptWireSkewedWorkerRefusesNewWork(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()
	id := uuid.Must(uuid.NewV7()).String()
	// A REAL parked pid, not the test binary's own: the adopted kill path at the
	// end of this test SIGKILLs whatever the endpoint file advertised.
	pid, waitKilled := parkedPID(t)
	startFakeWorker(t, id, func(reqID json.RawMessage) any {
		return jsonRPCResult(reqID, daemon.HelloResult{
			BinaryVersion:      "future",
			WireVersion:        daemon.WireVersion + 1,
			ACPProtocolVersion: 1,
		})
	}, pid)

	sup, err := New(Config{Root: root, Version: "router-build", NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	t.Cleanup(func() {
		killWorkers(sup)
		_ = sup.Close()
	})

	h, ok := sup.get(id)
	if !ok {
		t.Fatalf("wire-skewed worker %s was not adopted; a skewed worker must stay observable and finishable", id)
	}
	if h.skew != skewWire {
		t.Errorf("adopted handle skew = %v, want skewWire", h.skew)
	}
	if h.wireVersion != daemon.WireVersion+1 {
		t.Errorf("adopted handle wireVersion = %d, want %d", h.wireVersion, daemon.WireVersion+1)
	}
	if h.binaryVersion != "future" {
		t.Errorf("adopted handle binaryVersion = %q, want %q", h.binaryVersion, "future")
	}
	// Its endpoint is left in place — it is a live worker, not stale residue.
	if !endpointExists(t, id) {
		t.Errorf("endpoint for adopted worker %s was GC'd", id)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sup.Send(ctx, id, "another turn"); !errors.Is(err, ErrWorkerSkewed) {
		t.Errorf("Send to a wire-skewed worker: err = %v, want errors.Is ErrWorkerSkewed", err)
	}
	// A model change is NEW WORK, refused identically.
	if err := sup.SetModel(ctx, id, "faux"); !errors.Is(err, ErrWorkerSkewed) {
		t.Errorf("SetModel on a wire-skewed worker: err = %v, want errors.Is ErrWorkerSkewed", err)
	}
	// Permission replies are part of the observe/reply/finish subset the compat
	// window guarantees, so they must NOT be refused: the fake worker takes the
	// notification without complaint.
	if err := sup.Reply(id, event.PermissionReply{ID: "call-1", Verdict: event.VerdictAllow}); err != nil {
		t.Errorf("Reply on a wire-skewed worker: err = %v, want nil (replies stay routable)", err)
	}

	// The ADOPTED kill path, end to end: killHandleProcess's cmd==nil branch must
	// SIGKILL the pid the endpoint file advertised. A skewed worker still gets
	// killed — refusing NEW work never means refusing to shut it down.
	if h.pid != pid {
		t.Fatalf("adopted handle pid = %d, want the endpoint-advertised %d", h.pid, pid)
	}
	killWorkers(sup)
	waitKilled()
}

// TestAdoptBinarySkewedWorkerIsFullyRoutable is the other half of the policy
// split: a worker whose BINARY differs but whose wire matches is adopted AND
// keeps taking new work. Refusing here would strand every live session on every
// daemon upgrade, because Resume cannot yet spawn a replacement worker (Phase 4).
func TestAdoptBinarySkewedWorkerIsFullyRoutable(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()
	id := uuid.Must(uuid.NewV7()).String()
	pid, waitKilled := parkedPID(t)
	startFakeWorker(t, id, func(reqID json.RawMessage) any {
		return jsonRPCResult(reqID, daemon.HelloResult{
			BinaryVersion:      "older-build",
			WireVersion:        daemon.WireVersion,
			ACPProtocolVersion: 1,
		})
	}, pid)

	sup, err := New(Config{Root: root, Version: "router-build", NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	t.Cleanup(func() {
		killWorkers(sup)
		_ = sup.Close()
	})

	h, ok := sup.get(id)
	if !ok {
		t.Fatalf("binary-skewed worker %s was not adopted", id)
	}
	if h.skew != skewBinary {
		t.Errorf("adopted handle skew = %v, want skewBinary", h.skew)
	}
	if err := h.refuseNewWork("send"); err != nil {
		t.Errorf("binary skew refused new work (%v); it must stay fully routable — session pinning, not a hazard", err)
	}

	// The adopted kill path again, on the fully-routable class this time.
	if h.pid != pid {
		t.Fatalf("adopted handle pid = %d, want the endpoint-advertised %d", h.pid, pid)
	}
	killWorkers(sup)
	waitKilled()
}

// TestAdoptLiveWorker is the startup-adoption happy path plus the adopted-handle
// (cmd==nil) lifecycle: a router spawns a detached worker and shuts down WITHOUT
// killing it (a restart); a fresh router then adopts the still-alive worker by
// scan, serves it as a live roster entry, and — because an adopted handle has no
// *exec.Cmd — proves its reaper fires off the client connection closing, and its
// Kill path SIGKILLs the endpoint pid without nil-derefing the absent cmd.
func TestAdoptLiveWorker(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()

	// Router 1 spawns a real detached faux worker.
	sup1, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router1.New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	info, err := sup1.Create(ctx, "", supervisor.CreateOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sid := info.ID

	// Simulate a router restart: Close abandons the detached worker (design §3),
	// it keeps running with its endpoint/socket/lock on disk.
	if err := sup1.Close(); err != nil {
		t.Fatalf("router1.Close: %v", err)
	}

	// Router 2 adopts on construction.
	sup2, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router2.New: %v", err)
	}
	t.Cleanup(func() {
		killWorkers(sup2)
		_ = sup2.Close()
	})

	h, ok := sup2.get(sid)
	if !ok {
		t.Fatalf("router2 did not adopt live worker %s", sid)
	}
	if h.cmd != nil {
		t.Errorf("adopted handle has non-nil cmd; want nil (adopted, not spawned)")
	}
	if h.pid <= 0 {
		t.Errorf("adopted handle pid = %d, want the endpoint-advertised pid", h.pid)
	}

	// The adopted session serves in the live roster.
	rows, err := sup2.Roster(ctx)
	if err != nil {
		t.Fatalf("Roster: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.ID == sid && r.Live {
			found = true
		}
	}
	if !found {
		t.Errorf("adopted session %s not present+live in roster", sid)
	}

	// Kill the adopted worker: exercises killHandleProcess's cmd==nil branch
	// (best-effort SIGKILL by endpoint pid) and the reaper firing off the client
	// connection close — neither may nil-deref the absent cmd.
	if err := sup2.Kill(ctx, sid); err != nil {
		t.Fatalf("Kill adopted worker: %v", err)
	}
	waitOffline(t, sup2, sid)
}

// TestAdoptClosedRouterIsSafe verifies adoption never registers into a roster a
// concurrent Close has already drained: New's scan runs before the router is
// returned, so this simply asserts a router constructed over a live-worker
// endpoint and then closed leaves nothing dangling (no panic, clean Close).
func TestAdoptClosedRouterIsSafe(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()

	sup1, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router1.New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	info, err := sup1.Create(ctx, "", supervisor.CreateOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_ = sup1.Close()

	sup2, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router2.New: %v", err)
	}
	h, ok := sup2.get(info.ID)
	if !ok {
		t.Fatalf("router2 did not adopt %s", info.ID)
	}
	// The worker is real and DETACHED, so this test must reap it itself. Grab the
	// handle up front and kill by handle: Close abandons the worker (design §3)
	// AND drains the roster, so a killWorkers() cleanup running after Close finds
	// no handles left and silently leaks the process to pid 1.
	t.Cleanup(func() { killHandleProcess(h) })
	if err := sup2.Close(); err != nil {
		t.Fatalf("router2.Close: %v", err)
	}
}
