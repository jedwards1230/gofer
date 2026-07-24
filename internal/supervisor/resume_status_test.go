package supervisor_test

import (
	"context"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// TestSupervisor_ResumeUntouchedIsIdle is the core of the reloaded-session fix:
// a session brought back off disk that has NOT been prompted derives StatusIdle,
// not StatusNeedsInput. Merely opening a reloaded row must not present it as
// awaiting the user (which is what would move the overview's awaiting-input
// counter). Both the immediate Resume snapshot and the polled roster must agree.
func TestSupervisor_ResumeUntouchedIsIdle(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	entry, err := h.sup.Resume(ctx, "reloaded-id", supervisor.ResumeOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if entry.Status != supervisor.StatusIdle {
		t.Fatalf("resume snapshot status = %s, want idle", entry.Status)
	}
	if !entry.Live {
		t.Fatalf("resumed entry Live = false, want true")
	}

	r := roster(t, h.sup)
	if len(r) != 1 || r[0].ID != entry.ID {
		t.Fatalf("Roster = %+v, want one entry for %s", r, entry.ID)
	}
	if r[0].Status != supervisor.StatusIdle {
		t.Fatalf("roster status = %s, want idle", r[0].Status)
	}
}

// TestSupervisor_ResumePromptRestoresNormalDerivation covers constraint (c): once
// a resumed session is actually prompted, status derivation is UNCHANGED from
// today — it goes StatusWorking while the turn runs and back to StatusNeedsInput
// once that turn settles with an empty queue. The idle-at-rest state is a
// property of a resumed-but-untouched session only, and the first prompt ends it.
func TestSupervisor_ResumePromptRestoresNormalDerivation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	entry, err := h.sup.Resume(ctx, "reloaded-id", supervisor.ResumeOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if entry.Status != supervisor.StatusIdle {
		t.Fatalf("resume snapshot status = %s, want idle", entry.Status)
	}

	fs := h.session(entry.ID)
	if err := h.sup.Send(ctx, entry.ID, "keep going"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := fs.waitStarted(t); got != "keep going" {
		t.Fatalf("dispatched text = %q, want %q", got, "keep going")
	}
	// A prompted session is working, never idle.
	waitForStatus(t, h.sup, entry.ID, supervisor.StatusWorking)

	fs.finish(t, nil)
	// Turn settled, empty queue: back to NeedsInput exactly as an ordinary
	// session — the resumed-untouched idle state does not come back.
	waitForStatus(t, h.sup, entry.ID, supervisor.StatusNeedsInput)
}

// TestSupervisor_ResumeWithPendingRequestStaysNeedsInput covers constraint (b):
// a genuine pending request on a resumed-untouched session must STILL read as
// StatusNeedsInput, never be hidden behind the at-rest StatusIdle. A session
// killed mid-approval replays its retained permission.requested when it comes
// back live, so a resumed row can legitimately carry a real pending prompt — and
// that prompt must remain visible.
func TestSupervisor_ResumeWithPendingRequestStaysNeedsInput(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	entry, err := h.sup.Resume(ctx, "reloaded-id", supervisor.ResumeOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if entry.Status != supervisor.StatusIdle {
		t.Fatalf("pre-condition: resume snapshot status = %s, want idle", entry.Status)
	}

	// Stand in for the retained permission.requested a mid-approval kill replays
	// onto the reconstructed stream: the watcher (subscribed at registration)
	// folds it into the live pending count without any turn running.
	fs := h.session(entry.ID)
	fs.Emit(event.NewPermissionRequested(entry.ID, "call-1", "bash", map[string]any{"command": "rm -rf /"}, []string{"rule: ask"}))
	waitForPending(t, h.sup, entry.ID, 1)

	r := roster(t, h.sup)
	if len(r) != 1 {
		t.Fatalf("Roster len = %d, want 1", len(r))
	}
	if r[0].Status != supervisor.StatusNeedsInput {
		t.Fatalf("resumed row with a pending request: status = %s, want needs-input (not idle)", r[0].Status)
	}
}

// TestSupervisor_CreateUntouchedStaysNeedsInput pins the boundary of the fix:
// only a RESUMED session derives StatusIdle. A freshly created empty session is
// genuinely awaiting its first prompt, so it still reads StatusNeedsInput —
// new-session behavior is unchanged.
func TestSupervisor_CreateUntouchedStaysNeedsInput(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if entry.Status != supervisor.StatusNeedsInput {
		t.Fatalf("created (no prompt) status = %s, want needs-input", entry.Status)
	}
}
