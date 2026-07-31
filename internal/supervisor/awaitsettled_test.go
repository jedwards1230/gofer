package supervisor_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// TestAwaitSettled_ReturnsImmediately covers the cases AwaitSettled must NOT
// block on: a session with no live writer (offline / unknown id — its on-disk
// journal is already durable) and a session already idle (needs-input). Both
// return nil without waiting, which is what keeps List/diagnostic reads and an
// ordinary settled-session load fast.
func TestAwaitSettled_ReturnsImmediately(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// setup returns the id to await; the session it creates (if any) is left
		// idle, so the wait must return at once.
		setup func(t *testing.T, h *harness) string
	}{
		{
			name:  "unknown id is offline: no live writer to settle",
			setup: func(_ *testing.T, _ *harness) string { return "no-such-session" },
		},
		{
			name: "idle live session is already settled",
			setup: func(t *testing.T, h *harness) string {
				t.Helper()
				entry, err := h.sup.Create(context.Background(), "", supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				waitForStatus(t, h.sup, entry.ID, supervisor.StatusNeedsInput)
				return entry.ID
			},
		},
		{
			// Issue #313, and the case this table was missing: session/load
			// RESUMES before it waits, and a resumed-but-unprompted session
			// derives StatusIdle — not StatusNeedsInput. While settledInRoster
			// recognised only needs-input, this wait was unsatisfiable by
			// construction and burned its full bound (measured: 2.0002s) on
			// every cold attach.
			//
			// It is safe to treat as settled for the same reason the offline
			// case above is: no turn has run here, so there is no async journal
			// append to wait out. Resume makes the row live; it does not create
			// a writer.
			name: "resumed-but-unprompted session is idle with no live writer",
			setup: func(t *testing.T, h *harness) string {
				t.Helper()
				entry, err := h.sup.Resume(context.Background(), "reloaded-id", supervisor.ResumeOptions{Cwd: "/proj", Model: "m"})
				if err != nil {
					t.Fatalf("Resume: %v", err)
				}
				if entry.Status != supervisor.StatusIdle {
					t.Fatalf("resumed session status = %v, want %v — this case no longer exercises the #313 path",
						entry.Status, supervisor.StatusIdle)
				}
				return entry.ID
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			id := tt.setup(t, h)

			// A generous bound would still pass if AwaitSettled blocked, so use a
			// deadline far shorter than these idle cases could ever need and assert
			// it returns well inside it.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			done := make(chan error, 1)
			start := time.Now()
			go func() { done <- h.sup.AwaitSettled(ctx, id) }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("AwaitSettled = %v, want nil", err)
				}
				if elapsed := time.Since(start); elapsed > time.Second {
					t.Fatalf("AwaitSettled took %s on an already-settled session, want near-instant", elapsed)
				}
			case <-ctx.Done():
				t.Fatal("AwaitSettled blocked on an already-settled session")
			}
		})
	}
}

// TestAwaitSettled_BlocksUntilTurnSettles is the core guarantee: while a turn is
// in flight (the pump is inside Session.Prompt, so the session reports working),
// AwaitSettled does NOT return; it unblocks only once the turn finishes and the
// session transitions to needs-input — the observable proxy for "the journal is
// whole" that the issue #137 fix relies on.
func TestAwaitSettled_BlocksUntilTurnSettles(t *testing.T) {
	h := newHarness(t)

	entry, err := h.sup.Create(context.Background(), "", supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)

	if err := h.sup.Send(context.Background(), entry.ID, "hi"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fs.waitStarted(t)
	waitForStatus(t, h.sup, entry.ID, supervisor.StatusWorking)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.sup.AwaitSettled(ctx, entry.ID) }()

	// It must still be waiting while the turn is in flight: a spurious early
	// return here would mean the load could fold a mid-journal transcript.
	select {
	case err := <-done:
		t.Fatalf("AwaitSettled returned %v while the turn was still in flight; want it to block", err)
	case <-time.After(100 * time.Millisecond):
	}

	// Finishing the turn returns the pump to idle; AwaitSettled must then unblock.
	fs.finish(t, nil)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("AwaitSettled after settle = %v, want nil", err)
		}
	case <-ctx.Done():
		t.Fatal("AwaitSettled did not return after the turn settled")
	}
}

// TestAwaitSettled_MidTurnRespectsBound proves the wait is BEST-EFFORT and
// bounded: a session that never reaches needs-input (a turn blocked in flight —
// the shape of an adopted worker stuck on a permission, design §7) must not hang
// the caller. AwaitSettled returns its ctx's deadline error so the load can
// proceed to fold whatever is durable rather than deadlocking.
func TestAwaitSettled_MidTurnRespectsBound(t *testing.T) {
	h := newHarness(t)

	entry, err := h.sup.Create(context.Background(), "", supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)
	if err := h.sup.Send(context.Background(), entry.ID, "hi"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fs.waitStarted(t)
	waitForStatus(t, h.sup, entry.ID, supervisor.StatusWorking)

	// The turn is deliberately never finished. A short bound must be honored.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = h.sup.AwaitSettled(ctx, entry.ID)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("AwaitSettled took %s on a never-settling turn, want it bounded near 150ms", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AwaitSettled = %v, want context.DeadlineExceeded on a never-settling turn", err)
	}

	// Release the blocked pump so Close is clean.
	fs.finish(t, nil)
	waitForStatus(t, h.sup, entry.ID, supervisor.StatusNeedsInput)
}
