package router

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// countingSeam wraps a NewWorkerCmd seam with an invocation counter, so a test
// can prove a refused Create never reached the fork.
func countingSeam(inner func(ctx context.Context, sessionID, model, cwd string) *exec.Cmd, n *atomic.Int64) func(ctx context.Context, sessionID, model, cwd string) *exec.Cmd {
	return func(ctx context.Context, sessionID, model, cwd string) *exec.Cmd {
		n.Add(1)
		return inner(ctx, sessionID, model, cwd)
	}
}

// exitingWorkerSeam re-execs the test binary as a faux worker but WITHOUT the
// pinned session id it requires, so the child exits immediately (see
// runFauxWorker). It is a real fork/exec that never advertises an endpoint —
// the cheapest way to prove admission let a Create through to the spawn without
// paying for a full worker handshake.
func exitingWorkerSeam() func(ctx context.Context, sessionID, model, cwd string) *exec.Cmd {
	return func(ctx context.Context, _, _, _ string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0])
		cmd.Env = append(os.Environ(), "GOFER_ROUTER_TEST_WORKER=1")
		return cmd
	}
}

// addFakeWorkers registers n inert handles so a test can drive the MaxWorkers
// occupancy count without paying for n real worker processes. They are removed
// before the test's own router cleanup runs (t.Cleanup is LIFO), because Close
// would dereference their nil reconstruction cores.
func addFakeWorkers(t *testing.T, s *Supervisor, n int) {
	t.Helper()
	ids := make([]string, 0, n)
	s.mu.Lock()
	for range n {
		id := uuid.Must(uuid.NewV7()).String()
		s.workers[id] = &workerHandle{id: id}
		ids = append(ids, id)
	}
	s.mu.Unlock()
	t.Cleanup(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, id := range ids {
			delete(s.workers, id)
		}
	})
}

// pendingCount reads the in-flight-spawn reservation counter.
func pendingCount(s *Supervisor) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending
}

// endpointFileCount reports how many worker endpoint files exist — the "did a
// refused Create leave an artifact behind" probe.
func endpointFileCount(t *testing.T) int {
	t.Helper()
	entries, err := daemon.ListWorkerEndpoints()
	if err != nil {
		t.Fatalf("ListWorkerEndpoints: %v", err)
	}
	return len(entries)
}

// TestMaxWorkersDefaultIsUnlimited proves the default ([DefaultMaxWorkers] = 0)
// admits a Create no matter how many workers are already live: a router that
// never sets the knob behaves exactly as it did before admission control
// existed. The Create still fails — its worker exits without advertising — but
// it fails PAST admission, at the spawn, which is the point: the seam ran and
// the error is not ErrAtCapacity.
func TestMaxWorkersDefaultIsUnlimited(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()

	var spawns atomic.Int64
	sup, err := New(Config{Root: root, NewWorkerCmd: countingSeam(exitingWorkerSeam(), &spawns)})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	addFakeWorkers(t, sup, 25)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err = sup.Create(ctx, "", supervisor.CreateOptions{})
	if err == nil {
		t.Fatal("Create succeeded against a worker that never advertises; want a spawn failure")
	}
	if errors.Is(err, ErrAtCapacity) {
		t.Fatalf("Create was refused for capacity with the default (unlimited) cap: %v", err)
	}
	if got := spawns.Load(); got != 1 {
		t.Fatalf("worker spawns = %d, want 1 (admission must not gate the default)", got)
	}
	if got := pendingCount(sup); got != 0 {
		t.Fatalf("pending reservations = %d after a failed Create, want 0", got)
	}
}

// TestMaxWorkersRefusesAtCapacity is the admission-control contract end to end
// against REAL worker processes: at capacity, Create returns the typed
// ErrAtCapacity — which the daemon's session/new handler surfaces to the client
// as a JSON-RPC application error (handleSessionNew wraps every Create error
// with appError) rather than a transport failure — WITHOUT forking, dialing, or
// leaving an artifact. Killing a session frees the slot and the next Create
// spawns again.
func TestMaxWorkersRefusesAtCapacity(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()

	var spawns atomic.Int64
	sup, err := New(Config{
		Root:         root,
		MaxWorkers:   1,
		NewWorkerCmd: countingSeam(fauxWorkerSeam(root), &spawns),
	})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	t.Cleanup(func() {
		killWorkers(sup)
		_ = sup.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	first, err := sup.Create(ctx, "", supervisor.CreateOptions{})
	if err != nil {
		t.Fatalf("Create (first, under cap): %v", err)
	}
	if got := spawns.Load(); got != 1 {
		t.Fatalf("worker spawns = %d after the first Create, want 1", got)
	}
	artifacts := endpointFileCount(t)

	// At capacity: refused before anything is forked.
	_, err = sup.Create(ctx, "", supervisor.CreateOptions{})
	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("Create at capacity: err = %v, want errors.Is ErrAtCapacity", err)
	}
	if !strings.Contains(err.Error(), "max 1") {
		t.Fatalf("capacity error %q does not report the configured cap", err)
	}
	if got := spawns.Load(); got != 1 {
		t.Fatalf("worker spawns = %d after a refused Create, want 1 (nothing may be forked)", got)
	}
	if got := endpointFileCount(t); got != artifacts {
		t.Fatalf("endpoint files = %d after a refused Create, want %d (no artifacts)", got, artifacts)
	}
	if got := pendingCount(sup); got != 0 {
		t.Fatalf("pending reservations = %d after a refused Create, want 0", got)
	}

	// Dropping back below the cap re-opens admission.
	if err := sup.Kill(ctx, first.ID); err != nil {
		t.Fatalf("Kill(%s): %v", first.ID, err)
	}
	second, err := sup.Create(ctx, "", supervisor.CreateOptions{})
	if err != nil {
		t.Fatalf("Create after freeing a slot: %v", err)
	}
	if second.ID == first.ID {
		t.Fatal("second Create reused the killed session id")
	}
	if got := spawns.Load(); got != 2 {
		t.Fatalf("worker spawns = %d after the freed-slot Create, want 2", got)
	}
}

// TestMaxWorkersNegativeNormalizesToDefault proves a nonsensical cap cannot
// brick a router into refusing every session.
func TestMaxWorkersNegativeNormalizesToDefault(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()

	sup, err := New(Config{Root: root, MaxWorkers: -1, NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })
	if sup.maxWorkers != DefaultMaxWorkers {
		t.Fatalf("maxWorkers = %d, want DefaultMaxWorkers (%d)", sup.maxWorkers, DefaultMaxWorkers)
	}
	if err := sup.admit(); err != nil {
		t.Fatalf("admit with a negative cap: %v", err)
	}
	sup.releaseSlot()
}

// TestAdmitReservesSlotsForInFlightSpawns pins the reservation semantics the
// cap depends on: concurrent Creates that have been admitted but not yet
// registered still occupy capacity, so N admissions against a cap of N leave no
// room for an N+1th — the overshoot a plain live-handle count would allow.
func TestAdmitReservesSlotsForInFlightSpawns(t *testing.T) {
	shortRuntimeDir(t)
	sup, err := New(Config{Root: t.TempDir(), MaxWorkers: 2, NewWorkerCmd: fauxWorkerSeam(t.TempDir())})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	if err := sup.admit(); err != nil {
		t.Fatalf("admit 1: %v", err)
	}
	if err := sup.admit(); err != nil {
		t.Fatalf("admit 2: %v", err)
	}
	if err := sup.admit(); !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("admit 3: err = %v, want errors.Is ErrAtCapacity", err)
	}
	// Releasing one in-flight reservation re-opens exactly one slot.
	sup.releaseSlot()
	if err := sup.admit(); err != nil {
		t.Fatalf("admit after release: %v", err)
	}
	sup.releaseSlot()
	sup.releaseSlot()
}

// TestAdmitRefusesClosedRouter keeps the pre-existing closed-router contract
// intact now that the closed check lives inside admit.
func TestAdmitRefusesClosedRouter(t *testing.T) {
	shortRuntimeDir(t)
	sup, err := New(Config{Root: t.TempDir(), NewWorkerCmd: fauxWorkerSeam(t.TempDir())})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	if err := sup.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := sup.Create(ctx, "", supervisor.CreateOptions{}); !errors.Is(err, ErrNotLive) {
		t.Fatalf("Create on a closed router: err = %v, want errors.Is ErrNotLive", err)
	}
}
