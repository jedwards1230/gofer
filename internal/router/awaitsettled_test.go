package router

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// newAwaitSettledRouter builds a bare router (no workers) for the white-box
// AwaitSettled tests. It bypasses worker spawn/adoption entirely — the tests
// insert a hand-built handle with a controlled cached status — so it exercises
// the settle logic in isolation from the reconstruction machinery.
func newAwaitSettledRouter(t *testing.T) *Supervisor {
	t.Helper()
	sup, err := New(Config{Root: t.TempDir(), SelfExe: "gofer-test"})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })
	return sup
}

// putHandle inserts a bare handle with the given cached status (nil status means
// an unseeded cache, i.e. no info snapshot). It registers a cleanup that removes
// the handle before the router's own Close cleanup runs (LIFO order), since this
// hand-built handle has no reconstruction core for Close to tear down.
func putHandle(t *testing.T, s *Supervisor, id string, status *supervisor.SessionStatus) *workerHandle {
	t.Helper()
	h := &workerHandle{id: id, seeded: make(chan struct{}), settleCh: make(chan struct{}, 1)}
	if status != nil {
		info := supervisor.SessionInfo{ID: id, Live: true, Status: *status}
		h.info.Store(&info)
	}
	s.mu.Lock()
	s.workers[id] = h
	s.mu.Unlock()
	t.Cleanup(func() { _, _ = s.take(id) })
	return h
}

// settle publishes a needs-input snapshot onto h and wakes any waiter, mirroring
// what applyRosterEvent + pokeSettle do on a real TurnFinished.
func settle(h *workerHandle) {
	info := supervisor.SessionInfo{ID: h.id, Live: true, Status: supervisor.StatusNeedsInput}
	h.info.Store(&info)
	h.pokeSettle()
}

// TestRouterAwaitSettled_ReturnsImmediately covers the non-blocking cases: no
// live handle (offline — the on-disk journal is durable), an already-idle
// handle, and an unseeded cache (the degraded path, where settledness cannot be
// observed so the load must not block on it).
func TestRouterAwaitSettled_ReturnsImmediately(t *testing.T) {
	needsInput := supervisor.StatusNeedsInput

	tests := []struct {
		name  string
		setup func(t *testing.T, s *Supervisor) string
	}{
		{
			name:  "no live handle is offline",
			setup: func(_ *testing.T, _ *Supervisor) string { return "no-such-session" },
		},
		{
			name: "idle live handle is already settled",
			setup: func(t *testing.T, s *Supervisor) string {
				putHandle(t, s, "idle-sess", &needsInput)
				return "idle-sess"
			},
		},
		{
			name: "unseeded cache does not block the load",
			setup: func(t *testing.T, s *Supervisor) string {
				putHandle(t, s, "unseeded-sess", nil)
				return "unseeded-sess"
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newAwaitSettledRouter(t)
			id := tt.setup(t, s)

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- s.AwaitSettled(ctx, id) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("AwaitSettled = %v, want nil", err)
				}
			case <-ctx.Done():
				t.Fatal("AwaitSettled blocked when it should have returned at once")
			}
		})
	}
}

// TestRouterAwaitSettled_BlocksUntilSettle proves AwaitSettled waits while the
// worker's cached row reports working and unblocks when its watcher folds the
// terminal TurnFinished into a needs-input row (and pokes settleCh).
func TestRouterAwaitSettled_BlocksUntilSettle(t *testing.T) {
	s := newAwaitSettledRouter(t)
	working := supervisor.StatusWorking
	h := putHandle(t, s, "busy-sess", &working)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.AwaitSettled(ctx, h.id) }()

	select {
	case err := <-done:
		t.Fatalf("AwaitSettled returned %v while the worker was still working; want it to block", err)
	case <-time.After(100 * time.Millisecond):
	}

	settle(h)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("AwaitSettled after settle = %v, want nil", err)
		}
	case <-ctx.Done():
		t.Fatal("AwaitSettled did not return after the worker settled")
	}
}

// TestRouterAwaitSettled_NeverSettlesRespectsBound proves the wait is bounded: a
// worker stuck working (an adopted worker blocked on a permission, design §7)
// never reaches needs-input, so AwaitSettled must honor its ctx deadline rather
// than hang the load.
func TestRouterAwaitSettled_NeverSettlesRespectsBound(t *testing.T) {
	s := newAwaitSettledRouter(t)
	working := supervisor.StatusWorking
	h := putHandle(t, s, "stuck-sess", &working)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := s.AwaitSettled(ctx, h.id)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("AwaitSettled took %s on a never-settling worker, want it bounded near 150ms", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AwaitSettled = %v, want context.DeadlineExceeded", err)
	}
}
