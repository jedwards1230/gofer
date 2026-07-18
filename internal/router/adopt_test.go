package router

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

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
// live worker: a dead pid and a live-pid-but-dead-socket both GC the endpoint;
// a wire-version skew leaves it in place (the worker is presumed alive, just
// unroutable by this slice).
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
			name:       "version skew is left unadopted (not GC'd)",
			pid:        func(t *testing.T) int { return os.Getpid() },
			wire:       daemon.WireVersion + 1,
			addr:       func(id string) string { return "unix:///nonexistent/" + id + ".sock" },
			wantExists: true,
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
	// The worker is real and detached; kill it via the adopted handle before Close.
	t.Cleanup(func() { killWorkers(sup2) })
	if _, ok := sup2.get(info.ID); !ok {
		t.Fatalf("router2 did not adopt %s", info.ID)
	}
	if err := sup2.Close(); err != nil {
		t.Fatalf("router2.Close: %v", err)
	}
}
