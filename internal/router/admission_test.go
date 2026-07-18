package router

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
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
		cmd.Env = append(os.Environ(), envWorkerMode+"=1")
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

// liveWorkerCount reads the registered live-handle count.
func liveWorkerCount(s *Supervisor) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.workers)
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
	root := t.TempDir()
	sup, err := New(Config{Root: root, MaxWorkers: 2, NewWorkerCmd: fauxWorkerSeam(root)})
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

// TestMaxWorkersAdmitsExactlyTheCapConcurrently is the headline claim under
// contention: N clients calling session/new at once against a cap of k admit
// EXACTLY k, refuse the other N-k with ErrAtCapacity, and quiesce with no
// leaked reservations. It is the test that catches the reservation pair
// (pending-- and workers[id] = h) drifting out from under a single lock —
// admission would then either overshoot the cap or strand a slot forever. Run
// it under -race.
func TestMaxWorkersAdmitsExactlyTheCapConcurrently(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()

	const (
		limit    = 3
		contend  = 12
		spawnBud = 90 * time.Second
	)

	var spawns atomic.Int64
	sup, err := New(Config{
		Root:         root,
		MaxWorkers:   limit,
		NewWorkerCmd: countingSeam(fauxWorkerSeam(root), &spawns),
	})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	t.Cleanup(func() {
		killWorkers(sup)
		_ = sup.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), spawnBud)
	defer cancel()

	// Release every goroutine from one barrier so the Creates genuinely race
	// through admit rather than trickling in as the goroutines are scheduled.
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, contend)
	for i := range contend {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, cerr := sup.Create(ctx, "", supervisor.CreateOptions{})
			errs[i] = cerr
		}()
	}
	close(start)
	wg.Wait()

	var admitted, refused int
	for i, cerr := range errs {
		switch {
		case cerr == nil:
			admitted++
		case errors.Is(cerr, ErrAtCapacity):
			refused++
		default:
			t.Fatalf("Create %d: unexpected error %v (want nil or ErrAtCapacity)", i, cerr)
		}
	}
	if admitted != limit {
		t.Fatalf("successful Creates = %d, want exactly %d (the cap)", admitted, limit)
	}
	if refused != contend-limit {
		t.Fatalf("ErrAtCapacity refusals = %d, want %d", refused, contend-limit)
	}
	if got := spawns.Load(); got != int64(limit) {
		t.Fatalf("worker spawns = %d, want %d (a refused Create must fork nothing)", got, limit)
	}
	if got := pendingCount(sup); got != 0 {
		t.Fatalf("pending reservations = %d once quiesced, want 0", got)
	}
	if got := liveWorkerCount(sup); got != limit {
		t.Fatalf("live workers = %d, want %d", got, limit)
	}
}

// TestAdmitRefusesClosedRouter keeps the pre-existing closed-router contract
// intact now that the closed check lives inside admit.
func TestAdmitRefusesClosedRouter(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()
	sup, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
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

// TestCreateHelloFailureReleasesReservation guards the reservation invariant
// around the gofer/hello handshake Create performs between admit() and handle
// registration: EXACTLY ONE release per admit.
//
// The Hello call added a new early return in that window. If it released the
// slot manually (on top of Create's central deferred release) the counter would
// go negative and the router would over-admit past MaxWorkers forever; if it
// released nothing the counter would leak upward and eventually refuse every
// spawn with a spurious ErrAtCapacity. Both bugs pass build/vet/test/lint/race
// and only surface after hours of uptime, so the counter is asserted directly.
//
// The failure is injected by planting a fake worker that answers gofer/hello
// with an application error at the endpoint the router is about to poll for, so
// Create gets past admission, spawn, discovery and dial, and fails at exactly
// the handshake step.
func TestCreateHelloFailureReleasesReservation(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()

	var spawns atomic.Int64
	inner := exitingWorkerSeam()
	// The seam runs INSIDE Create, after admission and with the router-pinned
	// session uuid in hand — the only place a test can bind that uuid's socket
	// before the endpoint poll reads it.
	seam := func(ctx context.Context, sessionID, model, cwd string) *exec.Cmd {
		startFakeWorker(t, sessionID, func(reqID json.RawMessage) any {
			return jsonRPCError(reqID, -32000, "worker refuses to say hello")
		})
		return inner(ctx, sessionID, model, cwd)
	}

	sup, err := New(Config{Root: root, MaxWorkers: 1, NewWorkerCmd: countingSeam(seam, &spawns)})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	before := pendingCount(sup)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := sup.Create(ctx, "", supervisor.CreateOptions{}); err == nil {
		t.Fatal("Create succeeded against a worker whose gofer/hello errors; want a handshake failure")
	} else if !strings.Contains(err.Error(), "gofer/hello") {
		t.Fatalf("Create failed at the wrong step: %v (want a gofer/hello failure)", err)
	}
	if got := spawns.Load(); got != 1 {
		t.Fatalf("worker spawns = %d, want 1 (the Create must reach the spawn)", got)
	}

	// The invariant: the admission counter is exactly where it started.
	if got := pendingCount(sup); got != before {
		t.Fatalf("pending reservations = %d after a Create that failed at gofer/hello, want %d", got, before)
	}
	if got := liveWorkerCount(sup); got != 0 {
		t.Fatalf("live workers = %d after a failed Create, want 0", got)
	}
	// And capacity is genuinely reusable: a leaked reservation against MaxWorkers
	// 1 would refuse this admit with ErrAtCapacity.
	if err := sup.admit(); err != nil {
		t.Fatalf("admit after a failed Create: %v (a leaked reservation would refuse it)", err)
	}
	sup.releaseSlot()
}
