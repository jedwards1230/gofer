package supervisor_test

import (
	"context"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// waitForPending polls Roster until id's Pending reaches want or the deadline
// passes. watchPermissions updates the count asynchronously to Reply/Prompt, so
// tests observe the settled count through this rather than asserting inline.
func waitForPending(t *testing.T, sup *supervisor.Supervisor, id string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last int
	for time.Now().Before(deadline) {
		roster, err := sup.Roster(context.Background())
		if err != nil {
			t.Fatalf("Roster: %v", err)
		}
		for _, e := range roster {
			if e.ID == id {
				last = e.Pending
				if e.Pending == want {
					return
				}
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waitForPending: %s pending=%d did not reach %d within the deadline", id, last, want)
}

// TestReplyRoutesToGateAndPendingCount is the supervisor-level approval
// round-trip: a turn that asks bumps Pending to 1 and blocks on the gate;
// Reply(allow) unblocks it, the turn finishes, and Pending falls back to 0.
func TestReplyRoutesToGateAndPendingCount(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	info, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: h.root, Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	fs := h.session(info.ID)
	fs.setPermReq("call-1")

	if err := h.sup.Send(ctx, info.ID, "rm -rf /"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fs.waitStarted(t) // Prompt entered: permission.requested emitted, awaiting the gate.

	waitForPending(t, h.sup, info.ID, 1)

	if err := h.sup.Reply(info.ID, event.PermissionReply{ID: "call-1", Verdict: event.VerdictAllow}); err != nil {
		t.Fatalf("Reply: %v", err)
	}

	waitForStatus(t, h.sup, info.ID, supervisor.StatusNeedsInput)
	waitForPending(t, h.sup, info.ID, 0)
}

// TestReplyDenyResolves mirrors the allow path with a deny verdict: the gate
// still unblocks and the request resolves (Pending returns to 0), but with a
// deny — the loop would block the tool rather than run it.
func TestReplyDenyResolves(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	info, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: h.root, Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(info.ID)
	fs.setPermReq("call-1")

	if err := h.sup.Send(ctx, info.ID, "curl evil.example"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fs.waitStarted(t)
	waitForPending(t, h.sup, info.ID, 1)

	if err := h.sup.Reply(info.ID, event.PermissionReply{ID: "call-1", Verdict: event.VerdictDeny}); err != nil {
		t.Fatalf("Reply: %v", err)
	}
	waitForStatus(t, h.sup, info.ID, supervisor.StatusNeedsInput)
	waitForPending(t, h.sup, info.ID, 0)
}

// TestCancelReleasesAwaitNoLeak verifies a cancelled turn cleanly releases the
// approval Await with no leaked waiter: interrupting a turn blocked on the gate
// unblocks Prompt (Await returns ctx.Err), the request resolves (Pending back to
// 0), and the session returns to idle. Kill then returns promptly — stop() joins
// both the pump and the watchPermissions goroutines, so a leaked waiter would
// hang this test rather than pass it.
func TestCancelReleasesAwaitNoLeak(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	info, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: h.root, Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(info.ID)
	fs.setPermReq("call-1")

	if err := h.sup.Send(ctx, info.ID, "sleep forever"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fs.waitStarted(t)
	waitForPending(t, h.sup, info.ID, 1)

	// Cancel the in-flight turn: the gate's Await returns ctx.Err, Prompt
	// unwinds, and the request resolves — no reply ever arrives.
	if err := h.sup.Interrupt(ctx, info.ID); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	waitForStatus(t, h.sup, info.ID, supervisor.StatusNeedsInput)
	waitForPending(t, h.sup, info.ID, 0)

	// Kill joins both per-session goroutines; a leaked Await waiter or watcher
	// would deadlock here rather than return.
	done := make(chan error, 1)
	go func() { done <- h.sup.Kill(ctx, info.ID) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Kill: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Kill did not return — a session goroutine leaked")
	}
}

// TestReplyUnknownSession errors rather than silently dropping a reply for a
// session that is not live.
func TestReplyUnknownSession(t *testing.T) {
	h := newHarness(t)
	if err := h.sup.Reply("does-not-exist", event.PermissionReply{ID: "x", Verdict: event.VerdictAllow}); err == nil {
		t.Fatal("Reply(unknown): want error, got nil")
	}
}
