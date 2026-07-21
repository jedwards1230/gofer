package daemonbridge_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/tui"
)

// TestListSessionsSeesCreatedSession asserts ListSessions issues the ACP
// session/list Call and decodes its response into the TUI's picker row: a
// session created through the bridge comes back with its id and the cwd it was
// created in, which is only possible if the request reached the daemon's
// handleSessionList and the response's fields were mapped across.
//
// The compile-time `var _ tui.Supervisor = (*Supervisor)(nil)` assertion in
// bridge.go covers interface satisfaction; this covers the wire behavior.
func TestListSessionsSeesCreatedSession(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	url := newTestDaemon(t, sup)
	b := newBridge(t, url)

	cwd := t.TempDir()
	info, err := b.Create(context.Background(), "", tui.CreateOptions{Cwd: cwd, Model: "faux"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	refs, err := b.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	var found *tui.SessionRef
	for i := range refs {
		if refs[i].ID == info.ID {
			found = &refs[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("session %s missing from ListSessions: %+v", info.ID, refs)
	}
	if found.Cwd != cwd {
		t.Errorf("Cwd = %q, want %q — session/list's cwd did not reach the picker row", found.Cwd, cwd)
	}
	if found.Updated.IsZero() {
		t.Errorf("Updated is zero for %s; session/list's updatedAt was not decoded", info.ID)
	}
}

// TestListSessionsWalksEveryPage pins the pagination loop: the daemon pages
// session/list at 50 rows, so a store holding more than that returns a
// NextCursor the bridge must follow. Reading only the first page would silently
// hide every older session from the picker, with nothing on screen to say so.
func TestListSessionsWalksEveryPage(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	url := newTestDaemon(t, sup)
	b := newBridge(t, url)

	// One more than the daemon's page size (internal/daemon's
	// sessionListPageSize = 50), so exactly two pages are required.
	const want = 51
	cwd := t.TempDir()
	for range want {
		if _, err := sup.Create(context.Background(), "", supervisor.CreateOptions{Cwd: cwd, Model: "faux"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	refs, err := b.ListSessions(context.Background())
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	ids := map[string]bool{}
	for _, r := range refs {
		ids[r.ID] = true
	}
	if len(ids) != want {
		t.Errorf("ListSessions returned %d distinct sessions, want %d — the second page was not walked", len(ids), want)
	}
}

// TestListSessionsHonorsCancellation pins that the pagination walk cannot
// outlive its context. [daemon.Client.Call] selects on ctx.Done() and returns
// ctx.Err(), so a cancelled context aborts at the very next page request and
// the loop returns the error instead of walking on — no separate ctx.Err()
// check between iterations is needed for termination, and this is the test that
// says so rather than a comment.
func TestListSessionsHonorsCancellation(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	url := newTestDaemon(t, sup)
	b := newBridge(t, url)

	cwd := t.TempDir()
	// More than one page (internal/daemon's sessionListPageSize = 50), so a walk
	// that ignored cancellation would have a second request to make.
	for range 51 {
		if _, err := sup.Create(context.Background(), "", supervisor.CreateOptions{Cwd: cwd, Model: "faux"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	refs, err := b.ListSessions(ctx)
	if err == nil {
		t.Fatalf("ListSessions with a cancelled context returned %d rows and no error", len(refs))
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	if refs != nil {
		t.Errorf("refs = %+v, want nil on a cancelled walk", refs)
	}
}

// TestResumeLoadsSession asserts Resume issues session/load with the right
// params: the daemon answers it by calling its supervisor's own Resume, so a
// session that was NOT live before the call is on the roster after it. That is
// only true if sessionId and cwd both reached the daemon.
func TestResumeLoadsSession(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	url := newTestDaemon(t, sup)
	cwd := t.TempDir()

	// Create through one bridge, then archive so the session is on disk but off
	// the roster — the offline state /resume exists for.
	first := newBridge(t, url)
	info, err := first.Create(context.Background(), "", tui.CreateOptions{Cwd: cwd, Model: "faux"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := first.Archive(context.Background(), info.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	b := newBridge(t, url)
	roster, err := b.Roster(context.Background())
	if err != nil {
		t.Fatalf("Roster: %v", err)
	}
	for _, r := range roster {
		if r.ID == info.ID {
			t.Fatalf("session %s is still on the roster after Archive; this test's premise is stale", info.ID)
		}
	}

	if err := b.Resume(context.Background(), info.ID, cwd); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	roster, err = b.Roster(context.Background())
	if err != nil {
		t.Fatalf("Roster after Resume: %v", err)
	}
	var live bool
	for _, r := range roster {
		if r.ID == info.ID {
			live = true
			if r.Cwd != cwd {
				t.Errorf("resumed session Cwd = %q, want %q — session/load's cwd did not reach the supervisor", r.Cwd, cwd)
			}
		}
	}
	if !live {
		t.Fatalf("session %s is not on the roster after Resume: %+v", info.ID, roster)
	}
}

// TestResumeUnknownSession asserts a Resume against an id the daemon has never
// seen surfaces as a plain error through the bridge — the path /resume's
// unknown-id status note is built on.
func TestResumeUnknownSession(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	url := newTestDaemon(t, sup)
	b := newBridge(t, url)

	if err := b.Resume(context.Background(), "does-not-exist", t.TempDir()); err == nil {
		t.Fatal("Resume on unknown session: want an error, got none")
	}
}
