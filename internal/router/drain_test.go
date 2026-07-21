package router

// drain_test.go pins [Supervisor.Drain]: entering drain refuses a NEW Create
// with [ErrDraining], drain BLOCKS until every in-flight turn settles and then
// returns, it respects its ctx bound when a session never idles, and it does NOT
// kill workers. It reuses rostercache_test.go's countingWorker harness, driving
// a session's idle/working transitions by pushing turn.started / turn.finished
// events the way a real worker does.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// drainingState reads the router's draining flag under its lock.
func drainingState(s *Supervisor) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draining
}

// TestDrainRefusesNewCreate proves that once the router is draining, a new
// Create is refused with ErrDraining — before anything is forked (the refusal is
// in admit, ahead of the spawn). A worker already idle when Drain runs lets Drain
// return at once, so the flag is set and observable.
func TestDrainRefusesNewCreate(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()
	w := startRosterCacheWorker(t, "0.3.0")
	sup, handles := adoptCountingWorkers(t, root, w)

	// Retire the seeded "working" row to needs-input so Drain sees an idle fleet
	// and returns immediately.
	w.pushEvent(t, map[string]any{
		"type": string(event.KindTurnFinished), "session_id": w.id, "stop_reason": "end_turn",
	})
	awaitSnapshot(t, handles[0], "idle before drain", func(i supervisor.SessionInfo) bool {
		return i.Status == supervisor.StatusNeedsInput
	})

	ctx, cancel := context.WithTimeout(context.Background(), cacheTimeout)
	defer cancel()
	if err := sup.Drain(ctx); err != nil {
		t.Fatalf("Drain over an idle fleet: %v", err)
	}
	if !drainingState(sup) {
		t.Fatal("router not marked draining after Drain returned")
	}

	// A brand-new Create is refused with ErrDraining...
	if _, err := sup.Create(ctx, "", supervisor.CreateOptions{}); !errors.Is(err, ErrDraining) {
		t.Fatalf("Create while draining: err = %v, want errors.Is ErrDraining", err)
	}
	// ...and admit itself refuses without reserving a slot.
	if err := sup.admit(); !errors.Is(err, ErrDraining) {
		t.Fatalf("admit while draining: err = %v, want errors.Is ErrDraining", err)
	}
	if got := pendingCount(sup); got != 0 {
		t.Fatalf("pending reservations = %d after a drained Create, want 0 (nothing reserved)", got)
	}

	// Resuming an EXISTING live session stays allowed while draining — finishing
	// its work is the point of the drain.
	if _, err := sup.Resume(ctx, w.id, supervisor.ResumeOptions{}); err != nil {
		t.Errorf("Resume of an existing session while draining: %v, want nil", err)
	}
}

// TestDrainWaitsForInflightTurn is the settle contract: Drain marks the router
// draining immediately (so Create is refused) but BLOCKS while a turn is in
// flight, and returns only once that turn finishes and the session goes idle.
// The worker never dies — drain is not a kill.
func TestDrainWaitsForInflightTurn(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()
	w := startRosterCacheWorker(t, "0.3.0")
	sup, handles := adoptCountingWorkers(t, root, w)

	// The seeded row is "working": Drain must block on it.
	if got := handles[0].info.Load(); got == nil || got.Status != supervisor.StatusWorking {
		t.Fatalf("seeded status = %+v, want working so Drain blocks", got)
	}

	drainErr := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), cacheTimeout)
	defer cancel()
	go func() { drainErr <- sup.Drain(ctx) }()

	// The flag is set synchronously at Drain's entry; poll the observable until it
	// lands, then confirm Drain is still blocking (a NEW Create is refused).
	waitDraining(t, sup)
	if _, err := sup.Create(ctx, "", supervisor.CreateOptions{}); !errors.Is(err, ErrDraining) {
		t.Fatalf("Create while a drain is in progress: err = %v, want ErrDraining", err)
	}
	select {
	case err := <-drainErr:
		t.Fatalf("Drain returned (%v) while the session was still working; it must block", err)
	case <-time.After(50 * time.Millisecond):
		// Still blocking, as required.
	}

	// Finish the turn: the session goes idle and Drain returns.
	w.pushEvent(t, map[string]any{
		"type": string(event.KindTurnFinished), "session_id": w.id, "stop_reason": "end_turn",
	})
	select {
	case err := <-drainErr:
		if err != nil {
			t.Fatalf("Drain after the turn finished: %v, want nil", err)
		}
	case <-time.After(cacheTimeout):
		t.Fatal("Drain never returned after the in-flight turn finished")
	}

	// Drain does NOT kill workers: the handle is still live and its worker is
	// still reachable.
	if _, ok := sup.get(w.id); !ok {
		t.Fatal("worker handle was dropped by Drain; drain must not kill workers")
	}
	if got := liveWorkerCount(sup); got != 1 {
		t.Fatalf("live workers = %d after Drain, want 1 (drain does not kill)", got)
	}
}

// TestDrainRespectsCtxBound proves Drain is bounded: a session that never idles
// makes Drain return its ctx error rather than hanging, leaving the caller to
// decide whether to proceed. The worker stays live afterward.
func TestDrainRespectsCtxBound(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()
	w := startRosterCacheWorker(t, "0.3.0")
	sup, handles := adoptCountingWorkers(t, root, w)

	// The seeded row is "working" and no turn.finished is ever pushed, so the
	// session never settles — Drain must hit its ctx deadline.
	if got := handles[0].info.Load(); got == nil || got.Status != supervisor.StatusWorking {
		t.Fatalf("seeded status = %+v, want working so Drain blocks", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := sup.Drain(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain over a never-settling session: err = %v, want context.DeadlineExceeded", err)
	}

	// The router is draining and the worker is untouched — a timed-out drain is
	// "proceed to detach", not "kill".
	if !drainingState(sup) {
		t.Fatal("router not marked draining after a timed-out Drain")
	}
	if _, ok := sup.get(w.id); !ok {
		t.Fatal("worker handle dropped by a timed-out Drain; drain must not kill workers")
	}
}

// TestDrainIsIdempotent proves a second Drain is a harmless no-op on an already-
// idle, already-draining router.
func TestDrainIsIdempotent(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()
	w := startRosterCacheWorker(t, "0.3.0")
	sup, handles := adoptCountingWorkers(t, root, w)

	w.pushEvent(t, map[string]any{
		"type": string(event.KindTurnFinished), "session_id": w.id, "stop_reason": "end_turn",
	})
	awaitSnapshot(t, handles[0], "idle before drain", func(i supervisor.SessionInfo) bool {
		return i.Status == supervisor.StatusNeedsInput
	})

	ctx, cancel := context.WithTimeout(context.Background(), cacheTimeout)
	defer cancel()
	if err := sup.Drain(ctx); err != nil {
		t.Fatalf("first Drain: %v", err)
	}
	if err := sup.Drain(ctx); err != nil {
		t.Fatalf("second Drain (idempotent): %v", err)
	}
}

// waitDraining polls until the router reports draining, a bounded wait on the
// flag Drain sets synchronously at entry.
func waitDraining(t *testing.T, sup *Supervisor) {
	t.Helper()
	deadline := time.After(cacheTimeout)
	for {
		if drainingState(sup) {
			return
		}
		select {
		case <-deadline:
			t.Fatal("router never entered draining state")
		case <-time.After(time.Millisecond):
		}
	}
}
