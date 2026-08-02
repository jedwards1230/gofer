package supervisor_test

import (
	"context"
	"errors"
	"sync"
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

// statusIn returns id's status in snap, and whether id was present at all.
func statusIn(snap []supervisor.SessionInfo, id string) (supervisor.SessionStatus, bool) {
	for _, s := range snap {
		if s.ID == id {
			return s.Status, true
		}
	}
	return 0, false
}

// awaitStatus drains ch until id reports want, failing with why on the
// deadline. Unlike [waitForSnapshot] it names the status it never saw, because
// the failure it guards against is specifically "the watcher converged to the
// WRONG terminal status and then went quiet".
func awaitStatus(t *testing.T, ch <-chan []supervisor.SessionInfo, id string, want supervisor.SessionStatus, why string) {
	t.Helper()
	last := "none observed"
	deadline := time.After(2 * time.Second)
	for {
		select {
		case snap, ok := <-ch:
			if !ok {
				t.Fatalf("watcher channel closed before %s reported %s\n  %s", id, want, why)
			}
			if st, present := statusIn(snap, id); present {
				last = st.String()
				if st == want {
					return
				}
			} else {
				last = "absent from roster"
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s to report %s (last status seen: %s)\n  %s",
				id, want, last, why)
		}
	}
}

// TestWatchRoster_SeedNeverOverwritesANewerSnapshot pins the ordering rule that
// makes latest-wins delivery correct: a snapshot is PUSHED in the order it was
// COMPUTED, so the last push is never stale.
//
// It is the regression test for gofer#138. Delivery coalesces to `latest`
// (watcher.push), so whichever push lands last decides what the watcher sees.
// When WatchRoster computed its seed OUTSIDE watchMu, a subscriber that was
// descheduled between the two could push a snapshot older than one a concurrent
// notify had already delivered — and because a settled session makes no further
// roster change, nothing ever corrected it. The watcher sat on a permanently
// stale "working" row: a 5s timeout for every waiter on the terminal transition
// (Supervisor.AwaitSettled, session/load's settle barrier, the TUI roster).
//
// The interleaving has no input that provokes it, so the seed is parked by
// watchSeedTestHook rather than raced for; see that hook's doc.
func TestWatchRoster_SeedNeverOverwritesANewerSnapshot(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entry, err := h.sup.Create(ctx, "hi", supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)
	fs.waitStarted(t) // the turn is in flight, so any snapshot now reads "working"

	// The witness subscribes normally, before the hook is armed: it is how the
	// test observes whether a notify managed to push while the seed was parked.
	witness, err := h.sup.WatchRoster(ctx)
	if err != nil {
		t.Fatalf("WatchRoster (witness): %v", err)
	}

	parked, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer supervisor.SetWatchSeedTestHook(func() {
		once.Do(func() {
			close(parked)
			<-release
		})
	})()

	type subscription struct {
		ch  <-chan []supervisor.SessionInfo
		err error
	}
	subCh := make(chan subscription, 1)
	go func() {
		ch, err := h.sup.WatchRoster(ctx)
		subCh <- subscription{ch, err}
	}()
	<-parked // the seed snapshot is computed ("working") but not yet pushed

	// Settle the turn while the seed is parked. This is the session's LAST
	// roster change, so a watcher that loses this transition never gets another
	// chance to learn it.
	fs.finish(t, nil)

	// While a seed is mid-flight, no notify may push: computing and pushing
	// share one critical section, so the pump's needs-input snapshot cannot
	// overtake the older seed. The grace period only decides how reliably this
	// catches the unserialized case — it is never what makes the serialized case
	// correct.
	grace := time.After(200 * time.Millisecond)
racing:
	for {
		select {
		case snap, ok := <-witness:
			if !ok {
				t.Fatal("witness channel closed mid-test")
			}
			if st, present := statusIn(snap, entry.ID); present && st == supervisor.StatusNeedsInput {
				t.Fatal("a notify pushed while a seed snapshot was mid-flight: snapshots are " +
					"computed outside the push lock, so a stale seed can overwrite it and the " +
					"watcher is left on a permanently wrong status (gofer#138)")
			}
		case <-grace:
			break racing
		}
	}

	close(release)

	sub := <-subCh
	if sub.err != nil {
		t.Fatalf("WatchRoster (parked): %v", sub.err)
	}
	awaitStatus(t, sub.ch, entry.ID, supervisor.StatusNeedsInput,
		"the seed overwrote the newer needs-input snapshot, and a settled session publishes nothing further")
	awaitStatus(t, witness, entry.ID, supervisor.StatusNeedsInput,
		"the pump's needs-input notify never reached an already-registered watcher")
}
