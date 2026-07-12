package supervisor_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// recvSnapshot waits for one snapshot on ch or fails on the deadline.
func recvSnapshot(t *testing.T, ch <-chan []supervisor.SessionInfo) []supervisor.SessionInfo {
	t.Helper()
	select {
	case snap, ok := <-ch:
		if !ok {
			t.Fatal("WatchRoster channel closed unexpectedly")
		}
		return snap
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a roster snapshot")
		return nil
	}
}

// waitForSnapshot drains ch until a snapshot satisfies pred or the deadline
// passes. WatchRoster is coalescing, so a test asserts on convergence to a
// desired state rather than on an exact sequence of intermediate snapshots.
func waitForSnapshot(t *testing.T, ch <-chan []supervisor.SessionInfo, pred func([]supervisor.SessionInfo) bool) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case snap, ok := <-ch:
			if !ok {
				t.Fatal("WatchRoster channel closed before predicate held")
			}
			if pred(snap) {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for a matching roster snapshot")
		}
	}
}

// TestWatchRoster_InitialAndChanges asserts a watcher gets an initial
// snapshot on subscribe, a fresh snapshot after a create, and another after
// a kill.
func TestWatchRoster_InitialAndChanges(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := h.sup.WatchRoster(ctx)
	if err != nil {
		t.Fatalf("WatchRoster: %v", err)
	}

	// Initial snapshot: empty roster.
	if snap := recvSnapshot(t, ch); len(snap) != 0 {
		t.Fatalf("initial snapshot = %+v, want empty", snap)
	}

	entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	waitForSnapshot(t, ch, func(snap []supervisor.SessionInfo) bool {
		return len(snap) == 1 && snap[0].ID == entry.ID
	})

	if err := h.sup.Kill(ctx, entry.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	waitForSnapshot(t, ch, func(snap []supervisor.SessionInfo) bool {
		return len(snap) == 0
	})
}

// TestWatchRoster_SlowConsumerDoesNotStall asserts a watcher that never reads
// its channel cannot block the supervisor: creates, sends, and kills all
// proceed while a slow watcher is registered, and a second attentive watcher
// still converges to the latest snapshot.
func TestWatchRoster_SlowConsumerDoesNotStall(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A slow watcher: subscribe and never receive. Drop-old delivery must
	// keep the supervisor unblocked regardless.
	if _, err := h.sup.WatchRoster(ctx); err != nil {
		t.Fatalf("WatchRoster (slow): %v", err)
	}

	// An attentive watcher to prove liveness end-to-end.
	fast, err := h.sup.WatchRoster(ctx)
	if err != nil {
		t.Fatalf("WatchRoster (fast): %v", err)
	}
	recvSnapshot(t, fast) // initial

	// Drive several roster changes; none may block on the slow watcher.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5; i++ {
			entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
			if err != nil {
				t.Errorf("Create: %v", err)
				return
			}
			if err := h.sup.Kill(ctx, entry.ID); err != nil {
				t.Errorf("Kill: %v", err)
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor stalled behind a slow WatchRoster consumer")
	}

	// The fast watcher converges to the final empty roster.
	waitForSnapshot(t, fast, func(snap []supervisor.SessionInfo) bool {
		return len(snap) == 0
	})
}

// TestWatchRoster_ClosesOnCtxCancel asserts a watcher's channel closes when
// its ctx is cancelled, and its goroutine exits.
func TestWatchRoster_ClosesOnCtxCancel(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())

	ch, err := h.sup.WatchRoster(ctx)
	if err != nil {
		t.Fatalf("WatchRoster: %v", err)
	}
	recvSnapshot(t, ch) // initial

	cancel()

	// Drain until closed.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return // closed — success
			}
		case <-deadline:
			t.Fatal("WatchRoster channel did not close after ctx cancel")
		}
	}
}

// TestWatchRoster_ClosedSupervisor asserts WatchRoster on a closed supervisor
// returns ErrClosed, and that Close closes an existing watcher's channel.
func TestWatchRoster_ClosedSupervisor(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	ch, err := h.sup.WatchRoster(ctx)
	if err != nil {
		t.Fatalf("WatchRoster: %v", err)
	}
	recvSnapshot(t, ch) // initial

	if err := h.sup.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The existing watcher's channel is closed by Close.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				goto closed
			}
		case <-deadline:
			t.Fatal("Close did not close the watcher channel")
		}
	}
closed:
	if _, err := h.sup.WatchRoster(ctx); !errors.Is(err, supervisor.ErrClosed) {
		t.Fatalf("WatchRoster after Close = %v, want ErrClosed", err)
	}
}
