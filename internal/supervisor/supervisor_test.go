package supervisor_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// roster is a Roster(ctx) call that fails the test on error.
func roster(t *testing.T, sup *supervisor.Supervisor) []supervisor.SessionInfo {
	t.Helper()
	r, err := sup.Roster(context.Background())
	if err != nil {
		t.Fatalf("Roster: %v", err)
	}
	return r
}

// TestSupervisor_CreateSubmitComplete drives the base lifecycle: Create with
// no prompt registers an idle session, Send dispatches it, and completion
// returns it to NeedsInput.
func TestSupervisor_CreateSubmitComplete(t *testing.T) {
	h := newHarness(t)

	entry, err := h.sup.Create(context.Background(), "", supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if entry.Status != supervisor.StatusNeedsInput {
		t.Fatalf("initial status = %s, want needs-input", entry.Status)
	}
	if !entry.Live {
		t.Fatalf("initial entry Live = false, want true")
	}
	if r := roster(t, h.sup); len(r) != 1 || r[0].ID != entry.ID {
		t.Fatalf("Roster = %+v, want one entry for %s", r, entry.ID)
	}

	fs := h.session(entry.ID)

	if err := h.sup.Send(context.Background(), entry.ID, "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := fs.waitStarted(t); got != "hello" {
		t.Fatalf("dispatched text = %q, want hello", got)
	}
	waitForStatus(t, h.sup, entry.ID, supervisor.StatusWorking)

	fs.finish(t, nil)
	waitForStatus(t, h.sup, entry.ID, supervisor.StatusNeedsInput)

	if got := fs.callCount(); got != 1 {
		t.Fatalf("Prompt call count = %d, want 1", got)
	}

	// Title is the first prompt's snippet; Cost/Usage come from the fake.
	r := roster(t, h.sup)
	if r[0].Title != "hello" {
		t.Errorf("Title = %q, want hello", r[0].Title)
	}
	if r[0].Usage.OutputTokens != 5 || r[0].Cost.USD != 0.01 {
		t.Errorf("Usage/Cost = %+v / %+v, want canned tally", r[0].Usage, r[0].Cost)
	}
}

// TestSupervisor_CreateWithPrompt asserts Create with a non-empty prompt
// enqueues it as the first turn.
func TestSupervisor_CreateWithPrompt(t *testing.T) {
	h := newHarness(t)

	entry, err := h.sup.Create(context.Background(), "first", supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if entry.Status != supervisor.StatusWorking {
		t.Fatalf("status after create-with-prompt = %s, want working", entry.Status)
	}
	fs := h.session(entry.ID)
	if got := fs.waitStarted(t); got != "first" {
		t.Fatalf("first dispatched text = %q, want first", got)
	}
	fs.finish(t, nil)
	waitForStatus(t, h.sup, entry.ID, supervisor.StatusNeedsInput)
}

// TestSupervisor_QueueFIFO covers queuing while running, QueueList/
// QueueClear, and that queued prompts dispatch in FIFO order once the
// running turn settles.
func TestSupervisor_QueueFIFO(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)

	if err := h.sup.Send(ctx, entry.ID, "a"); err != nil {
		t.Fatalf("Send(a): %v", err)
	}
	fs.waitStarted(t)

	if err := h.sup.Send(ctx, entry.ID, "b"); err != nil {
		t.Fatalf("Send(b): %v", err)
	}
	if err := h.sup.Send(ctx, entry.ID, "c"); err != nil {
		t.Fatalf("Send(c): %v", err)
	}

	ql, err := h.sup.QueueList(ctx, entry.ID)
	if err != nil {
		t.Fatalf("QueueList: %v", err)
	}
	if !reflect.DeepEqual(ql, []string{"b", "c"}) {
		t.Fatalf("QueueList = %v, want [b c]", ql)
	}

	n, err := h.sup.QueueClear(ctx, entry.ID)
	if err != nil {
		t.Fatalf("QueueClear: %v", err)
	}
	if n != 2 {
		t.Fatalf("QueueClear = %d, want 2", n)
	}
	if ql, err := h.sup.QueueList(ctx, entry.ID); err != nil || len(ql) != 0 {
		t.Fatalf("QueueList after clear = %v, %v, want empty", ql, err)
	}

	// "a" is still running (never finished) — QueueClear must not have
	// touched it.
	if got := fs.callCount(); got != 1 {
		t.Fatalf("Prompt call count after clear = %d, want 1 (a still running)", got)
	}

	if err := h.sup.Send(ctx, entry.ID, "b2"); err != nil {
		t.Fatalf("Send(b2): %v", err)
	}
	if err := h.sup.Send(ctx, entry.ID, "c2"); err != nil {
		t.Fatalf("Send(c2): %v", err)
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

	waitForStatus(t, h.sup, entry.ID, supervisor.StatusNeedsInput)
}

// TestSupervisor_Interrupt asserts Interrupt cancels the in-flight turn (the
// fake observes ctx cancellation and returns without a Close call) and that
// queued prompts survive and dispatch afterward.
func TestSupervisor_Interrupt(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)

	if err := h.sup.Send(ctx, entry.ID, "a"); err != nil {
		t.Fatalf("Send(a): %v", err)
	}
	fs.waitStarted(t)
	if err := h.sup.Send(ctx, entry.ID, "b"); err != nil {
		t.Fatalf("Send(b): %v", err)
	}

	if err := h.sup.Interrupt(ctx, entry.ID); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	// "a"'s Prompt call must return via ctx cancellation, not a manual
	// finish — the pump should dispatch the queued "b" on its own.
	if got := fs.waitStarted(t); got != "b" {
		t.Fatalf("dispatch after interrupt = %q, want b (queued prompt must survive)", got)
	}
	fs.finish(t, nil)
	waitForStatus(t, h.sup, entry.ID, supervisor.StatusNeedsInput)

	if got := fs.callCount(); got != 2 {
		t.Fatalf("Prompt call count = %d, want 2 (a interrupted, b ran)", got)
	}
}

// TestSupervisor_InterruptIdleIsNoop asserts Interrupt on an idle session
// returns nil and changes nothing.
func TestSupervisor_InterruptIdleIsNoop(t *testing.T) {
	h := newHarness(t)
	entry, err := h.sup.Create(context.Background(), "", supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := h.sup.Interrupt(context.Background(), entry.ID); err != nil {
		t.Fatalf("Interrupt on idle session: %v", err)
	}
}

// TestSupervisor_Kill asserts Kill emits session.killed, drops the session
// from the roster (subsequent ops fail with ErrNotLive), and never deletes
// the on-disk journal.
func TestSupervisor_Kill(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
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

	sub, err := h.sup.Subscribe(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err := h.sup.Kill(ctx, entry.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	assertEventKind(t, sub, event.KindSessionKilled)

	if r := roster(t, h.sup); len(r) != 0 {
		t.Fatalf("Roster after Kill = %+v, want empty", r)
	}
	if err := h.sup.Send(ctx, entry.ID, "x"); !errors.Is(err, supervisor.ErrNotLive) {
		t.Fatalf("Send after Kill = %v, want ErrNotLive", err)
	}
	if err := h.sup.Kill(ctx, "does-not-exist"); !errors.Is(err, supervisor.ErrNotLive) {
		t.Fatalf("Kill unknown id = %v, want ErrNotLive", err)
	}

	if _, err := os.Stat(fs.path); err != nil {
		t.Fatalf("journal file missing after Kill: %v", err)
	}
}

// TestSupervisor_SubscribeLive mirrors TestSupervisor_Kill's Subscribe usage:
// SubscribeLive returns a working subscription for a live session (it
// observes an event published after the call, same as Subscribe would), and
// errors the same way Subscribe does — [ErrNotLive] — for an unknown id.
func TestSupervisor_SubscribeLive(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	sub, err := h.sup.SubscribeLive(ctx, entry.ID)
	if err != nil {
		t.Fatalf("SubscribeLive: %v", err)
	}

	if err := h.sup.Kill(ctx, entry.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	assertEventKind(t, sub, event.KindSessionKilled)

	if _, err := h.sup.SubscribeLive(ctx, "does-not-exist"); !errors.Is(err, supervisor.ErrNotLive) {
		t.Fatalf("SubscribeLive unknown id = %v, want ErrNotLive", err)
	}
}

// TestSupervisor_SubscribeLiveSkipsRetainedBacklog is the supervisor-level
// regression test for the bug this package's EventsLive/SubscribeLive exist
// to fix: a must-deliver event published BEFORE a caller subscribes is
// replayed into a new [Supervisor.Subscribe] subscription (by design, for
// mid-session attach) but must NOT be replayed into a new
// [Supervisor.SubscribeLive] one — otherwise a caller driving a fresh turn
// would observe a stale retained event (e.g. a prior turn's turn.finished)
// as if it belonged to the turn it just started.
func TestSupervisor_SubscribeLiveSkipsRetainedBacklog(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)

	// Emit a must-deliver event and let it settle into the fake's retained
	// backlog before either subscription is opened.
	fs.Emit(event.NewSessionKilled(entry.ID))

	live, err := h.sup.SubscribeLive(ctx, entry.ID)
	if err != nil {
		t.Fatalf("SubscribeLive: %v", err)
	}
	select {
	case e := <-live.C:
		t.Fatalf("SubscribeLive replayed a retained event: %s", e.Kind())
	case <-time.After(100 * time.Millisecond):
	}

	// Subscribe (with replay), by contrast, still delivers it — confirming
	// the backlog genuinely exists and SubscribeLive's silence above isn't
	// just an empty broker.
	replayed, err := h.sup.Subscribe(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	assertEventKind(t, replayed, event.KindSessionKilled)
}

// TestSupervisor_Archive asserts Archive rejects a running session with
// ErrRunning, and archives (emitting session.archived, dropping from the
// roster) an idle one.
func TestSupervisor_Archive(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	entry, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)

	if err := h.sup.Send(ctx, entry.ID, "a"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fs.waitStarted(t)

	if err := h.sup.Archive(ctx, entry.ID); !errors.Is(err, supervisor.ErrRunning) {
		t.Fatalf("Archive while running = %v, want ErrRunning", err)
	}

	fs.finish(t, nil)
	waitForStatus(t, h.sup, entry.ID, supervisor.StatusNeedsInput)

	sub, err := h.sup.Subscribe(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err := h.sup.Archive(ctx, entry.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	assertEventKind(t, sub, event.KindSessionArchived)

	if r := roster(t, h.sup); len(r) != 0 {
		t.Fatalf("Roster after Archive = %+v, want empty", r)
	}
}

// TestSupervisor_ResumeIdempotent asserts resuming an already-live id
// returns the existing snapshot without building a second runner.
func TestSupervisor_ResumeIdempotent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	opts := supervisor.ResumeOptions{Cwd: "/proj", Model: "m"}
	entry1, err := h.sup.Resume(ctx, "existing-id", opts)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := h.resN.Load(); got != 1 {
		t.Fatalf("ResumeSession seam called %d times, want 1", got)
	}

	entry2, err := h.sup.Resume(ctx, "existing-id", opts)
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
	ctx := context.Background()

	first, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	second, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	r := roster(t, h.sup)
	if len(r) != 2 {
		t.Fatalf("Roster len = %d, want 2", len(r))
	}
	if r[0].ID != second.ID || r[1].ID != first.ID {
		t.Fatalf("Roster order = [%s %s], want [%s %s] (newest first)", r[0].ID, r[1].ID, second.ID, first.ID)
	}
}

// TestSupervisor_List enumerates on-disk sessions and overlays live state.
func TestSupervisor_List(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	live, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
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

	infos, err := h.sup.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	var gotLive, gotOffline bool
	for _, info := range infos {
		switch info.ID {
		case live.ID:
			gotLive = true
			if !info.Live || info.Status != supervisor.StatusNeedsInput {
				t.Errorf("live entry = %+v, want Live=true Status=needs-input", info)
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

// assertEventKind drains sub until it observes an event of the given kind or
// the bound elapses.
func assertEventKind(t *testing.T, sub *event.Subscription, kind string) {
	t.Helper()
	for {
		select {
		case e, ok := <-sub.C:
			if !ok {
				t.Fatalf("subscription closed before delivering %s", kind)
			}
			if e.Kind() == kind {
				return
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s", kind)
		}
	}
}

// awaitSessionError drains sub for up to 2s and returns the first
// event.SessionError it sees, or nil if the stream goes quiet first. It exists
// because the pump's failure path is only observable as an event — nothing
// reads the lastErr field it also writes.
func awaitSessionError(t *testing.T, sub *event.Subscription) *event.SessionError {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case e, ok := <-sub.C:
			if !ok {
				return nil
			}
			if se, isErr := e.(event.SessionError); isErr {
				return &se
			}
		case <-deadline:
			return nil
		}
	}
}

// TestSupervisor_PromptErrorEmitsSessionError asserts a failed turn reaches
// subscribers as a session.error on the session's own stream.
//
// This is the only delivery path for a journal write failure. The SDK reports
// that class of error solely as Prompt's return value — never as an event of
// its own — and since agent-sdk-go v0.14.1, Prompt takes and CLEARS it at the
// turn boundary, so Close no longer reports it either. If the pump drops the
// error, a session keeps serving a normal-looking transcript while entries are
// missing from the JSONL, surfacing only later, on resume, as agent amnesia.
func TestSupervisor_PromptErrorEmitsSessionError(t *testing.T) {
	h := newHarness(t)

	entry, err := h.sup.Create(context.Background(), "", supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)
	sub := fs.Events()
	defer sub.Close()

	if err := h.sup.Send(context.Background(), entry.ID, "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := fs.waitStarted(t); got != "hello" {
		t.Fatalf("dispatched text = %q, want hello", got)
	}

	// The shape of a journal write failure as Prompt returns it.
	fs.finish(t, errors.New("runner: append user message: disk full"))

	se := awaitSessionError(t, sub)
	if se == nil {
		t.Fatal("no session.error emitted for a failed turn: the error reached no subscriber, and nothing reads lastErr")
	}
	if !strings.Contains(se.Err, "disk full") {
		t.Errorf("session.error Err = %q, want it to carry the underlying failure", se.Err)
	}
	if se.Fatal {
		t.Error("session.error Fatal = true, want false: a failed turn does not end the session")
	}

	// The session stays live and usable — a failed turn is reported, not terminal.
	waitForStatus(t, h.sup, entry.ID, supervisor.StatusNeedsInput)
}

// TestSupervisor_InterruptEmitsNoSessionError is the negative half: a
// cancelled turn is the expected outcome of Interrupt/Kill/Archive, not a
// failure, so it must not be reported as one. Without this, the emit above
// could be written unconditionally and every Ctrl-C would raise a spurious
// error to every attached client.
func TestSupervisor_InterruptEmitsNoSessionError(t *testing.T) {
	h := newHarness(t)

	entry, err := h.sup.Create(context.Background(), "", supervisor.CreateOptions{Cwd: "/proj", Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(entry.ID)
	sub := fs.Events()
	defer sub.Close()

	if err := h.sup.Send(context.Background(), entry.ID, "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fs.waitStarted(t)

	// Interrupt cancels turnCtx, so the fake's Prompt returns context.Canceled.
	if err := h.sup.Interrupt(context.Background(), entry.ID); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	waitForStatus(t, h.sup, entry.ID, supervisor.StatusNeedsInput)

	if se := awaitSessionError(t, sub); se != nil {
		t.Errorf("session.error emitted for a cancelled turn (Err=%q); cancellation is expected, not a failure", se.Err)
	}
}
