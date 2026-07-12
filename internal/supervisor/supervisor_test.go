package supervisor_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// TestSupervisor_CreateSubmitComplete drives the base lifecycle: Create
// registers an idle session, Submit dispatches it, and completion returns it
// to idle.
func TestSupervisor_CreateSubmitComplete(t *testing.T) {
	h := newHarness(t)

	entry, err := h.sup.Create(context.Background(), supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if entry.State != supervisor.StateIdle {
		t.Fatalf("initial state = %s, want idle", entry.State)
	}
	if roster := h.sup.Roster(); len(roster) != 1 || roster[0].ID != entry.ID {
		t.Fatalf("Roster = %+v, want one entry for %s", roster, entry.ID)
	}

	fs := h.session(entry.ID)

	pos, err := h.sup.Submit(entry.ID, "hello")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if pos != 0 {
		t.Fatalf("Submit pos = %d, want 0", pos)
	}

	if got := fs.waitStarted(t); got != "hello" {
		t.Fatalf("dispatched text = %q, want hello", got)
	}
	waitForState(t, h.sup, entry.ID, supervisor.StateRunning)

	fs.finish(t, nil)
	waitForState(t, h.sup, entry.ID, supervisor.StateIdle)

	if got := fs.callCount(); got != 1 {
		t.Fatalf("Prompt call count = %d, want 1", got)
	}
}

// TestSupervisor_QueueFIFO covers queuing while running, QueueList/
// QueueClear, and that queued prompts dispatch in FIFO order once the
// running turn settles.
func TestSupervisor_QueueFIFO(t *testing.T) {
	h := newHarness(t)

	entry, err := h.sup.Create(context.Background(), supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)

	if pos, err := h.sup.Submit(entry.ID, "a"); err != nil || pos != 0 {
		t.Fatalf("Submit(a) = %d, %v, want 0, nil", pos, err)
	}
	fs.waitStarted(t)

	if pos, err := h.sup.Submit(entry.ID, "b"); err != nil || pos != 1 {
		t.Fatalf("Submit(b) = %d, %v, want 1, nil", pos, err)
	}
	if pos, err := h.sup.Submit(entry.ID, "c"); err != nil || pos != 2 {
		t.Fatalf("Submit(c) = %d, %v, want 2, nil", pos, err)
	}

	ql, err := h.sup.QueueList(entry.ID)
	if err != nil {
		t.Fatalf("QueueList: %v", err)
	}
	if !reflect.DeepEqual(ql, []string{"b", "c"}) {
		t.Fatalf("QueueList = %v, want [b c]", ql)
	}

	n, err := h.sup.QueueClear(entry.ID)
	if err != nil {
		t.Fatalf("QueueClear: %v", err)
	}
	if n != 2 {
		t.Fatalf("QueueClear = %d, want 2", n)
	}
	if ql, err := h.sup.QueueList(entry.ID); err != nil || len(ql) != 0 {
		t.Fatalf("QueueList after clear = %v, %v, want empty", ql, err)
	}

	// "a" is still running (never finished) — QueueClear must not have
	// touched it.
	if got := fs.callCount(); got != 1 {
		t.Fatalf("Prompt call count after clear = %d, want 1 (a still running)", got)
	}

	if _, err := h.sup.Submit(entry.ID, "b2"); err != nil {
		t.Fatalf("Submit(b2): %v", err)
	}
	if _, err := h.sup.Submit(entry.ID, "c2"); err != nil {
		t.Fatalf("Submit(c2): %v", err)
	}

	fs.finish(t, nil) // "a" settles
	if got := fs.waitStarted(t); got != "b2" {
		t.Fatalf("next dispatch = %q, want b2", got)
	}
	fs.finish(t, nil)
	if got := fs.waitStarted(t); got != "c2" {
		t.Fatalf("next dispatch = %q, want c2", got)
	}
	fs.finish(t, nil)

	waitForState(t, h.sup, entry.ID, supervisor.StateIdle)
}

// TestSupervisor_Interrupt asserts Interrupt cancels the in-flight turn (the
// fake observes ctx cancellation and returns without a Close call) and that
// queued prompts survive and dispatch afterward.
func TestSupervisor_Interrupt(t *testing.T) {
	h := newHarness(t)

	entry, err := h.sup.Create(context.Background(), supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)

	if _, err := h.sup.Submit(entry.ID, "a"); err != nil {
		t.Fatalf("Submit(a): %v", err)
	}
	fs.waitStarted(t)
	if _, err := h.sup.Submit(entry.ID, "b"); err != nil {
		t.Fatalf("Submit(b): %v", err)
	}

	if err := h.sup.Interrupt(entry.ID); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	// "a"'s Prompt call must return via ctx cancellation, not a manual
	// finish — the pump should dispatch the queued "b" on its own.
	if got := fs.waitStarted(t); got != "b" {
		t.Fatalf("dispatch after interrupt = %q, want b (queued prompt must survive)", got)
	}
	fs.finish(t, nil)
	waitForState(t, h.sup, entry.ID, supervisor.StateIdle)

	if got := fs.callCount(); got != 2 {
		t.Fatalf("Prompt call count = %d, want 2 (a interrupted, b ran)", got)
	}
}

// TestSupervisor_InterruptIdleIsNoop asserts Interrupt on an idle session
// returns nil and changes nothing.
func TestSupervisor_InterruptIdleIsNoop(t *testing.T) {
	h := newHarness(t)
	entry, err := h.sup.Create(context.Background(), supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := h.sup.Interrupt(entry.ID); err != nil {
		t.Fatalf("Interrupt on idle session: %v", err)
	}
}

// TestSupervisor_Kill asserts Kill emits session.killed, drops the session
// from the roster (subsequent ops fail with ErrNotLive), and never deletes
// the on-disk journal.
func TestSupervisor_Kill(t *testing.T) {
	h := newHarness(t)

	entry, err := h.sup.Create(context.Background(), supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)

	if err := os.MkdirAll(filepath.Dir(fs.path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(fs.path, []byte("journal"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	sub, err := h.sup.Subscribe(entry.ID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err := h.sup.Kill(entry.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	select {
	case e, ok := <-sub.C:
		if !ok {
			t.Fatal("subscription closed before delivering session.killed")
		}
		if _, ok := e.(event.SessionKilled); !ok {
			t.Fatalf("event = %T, want event.SessionKilled", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for session.killed")
	}

	if roster := h.sup.Roster(); len(roster) != 0 {
		t.Fatalf("Roster after Kill = %+v, want empty", roster)
	}
	if _, err := h.sup.Submit(entry.ID, "x"); !errors.Is(err, supervisor.ErrNotLive) {
		t.Fatalf("Submit after Kill = %v, want ErrNotLive", err)
	}
	if err := h.sup.Kill("does-not-exist"); !errors.Is(err, supervisor.ErrNotLive) {
		t.Fatalf("Kill unknown id = %v, want ErrNotLive", err)
	}

	if _, err := os.Stat(fs.path); err != nil {
		t.Fatalf("journal file missing after Kill: %v", err)
	}
}

// TestSupervisor_Archive asserts Archive rejects a running session with
// ErrRunning, and archives (emitting session.archived, dropping from the
// roster) an idle one.
func TestSupervisor_Archive(t *testing.T) {
	h := newHarness(t)

	entry, err := h.sup.Create(context.Background(), supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)

	if _, err := h.sup.Submit(entry.ID, "a"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	fs.waitStarted(t)

	if err := h.sup.Archive(entry.ID); !errors.Is(err, supervisor.ErrRunning) {
		t.Fatalf("Archive while running = %v, want ErrRunning", err)
	}

	fs.finish(t, nil)
	waitForState(t, h.sup, entry.ID, supervisor.StateIdle)

	sub, err := h.sup.Subscribe(entry.ID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err := h.sup.Archive(entry.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	select {
	case e, ok := <-sub.C:
		if !ok {
			t.Fatal("subscription closed before delivering session.archived")
		}
		if _, ok := e.(event.SessionArchived); !ok {
			t.Fatalf("event = %T, want event.SessionArchived", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for session.archived")
	}

	if roster := h.sup.Roster(); len(roster) != 0 {
		t.Fatalf("Roster after Archive = %+v, want empty", roster)
	}
}

// TestSupervisor_ResumeIdempotent asserts resuming an already-live id
// returns the existing roster entry without building a second runner.
func TestSupervisor_ResumeIdempotent(t *testing.T) {
	h := newHarness(t)

	opts := supervisor.ResumeOptions{Cwd: "/proj", Model: "m"}
	entry1, err := h.sup.Resume(context.Background(), "existing-id", opts)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := h.resN.Load(); got != 1 {
		t.Fatalf("ResumeSession seam called %d times, want 1", got)
	}

	entry2, err := h.sup.Resume(context.Background(), "existing-id", opts)
	if err != nil {
		t.Fatalf("Resume (again): %v", err)
	}
	if got := h.resN.Load(); got != 1 {
		t.Fatalf("ResumeSession seam called %d times after idempotent Resume, want 1", got)
	}
	if entry1.ID != entry2.ID {
		t.Fatalf("entry IDs differ: %q vs %q", entry1.ID, entry2.ID)
	}
}

// TestSupervisor_RosterOrder asserts Roster is newest-first.
func TestSupervisor_RosterOrder(t *testing.T) {
	h := newHarness(t)

	first, err := h.sup.Create(context.Background(), supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	second, err := h.sup.Create(context.Background(), supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	roster := h.sup.Roster()
	if len(roster) != 2 {
		t.Fatalf("Roster len = %d, want 2", len(roster))
	}
	if roster[0].ID != second.ID || roster[1].ID != first.ID {
		t.Fatalf("Roster order = [%s %s], want [%s %s] (newest first)", roster[0].ID, roster[1].ID, second.ID, first.ID)
	}
}

// TestSupervisor_List enumerates on-disk sessions and overlays live state.
func TestSupervisor_List(t *testing.T) {
	h := newHarness(t)

	live, err := h.sup.Create(context.Background(), supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(live.ID)
	if err := os.MkdirAll(filepath.Dir(fs.path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(fs.path, []byte("{}"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	offlineID := "offline-id"
	offlinePath := filepath.Join(filepath.Dir(fs.path), offlineID+".jsonl")
	if err := os.WriteFile(offlinePath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	infos, err := h.sup.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	var gotLive, gotOffline bool
	for _, info := range infos {
		switch info.ID {
		case live.ID:
			gotLive = true
			if !info.Live || info.State != supervisor.StateIdle {
				t.Errorf("live entry = %+v, want Live=true State=idle", info)
			}
		case offlineID:
			gotOffline = true
			if info.Live {
				t.Errorf("offline entry = %+v, want Live=false", info)
			}
		}
	}
	if !gotLive || !gotOffline {
		t.Fatalf("List missing entries (live=%v offline=%v): %+v", gotLive, gotOffline, infos)
	}
}
