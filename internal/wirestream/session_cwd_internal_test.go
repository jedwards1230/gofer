package wirestream

// session_cwd_internal_test.go covers sessionCwd's persistent-overview fallback
// directly (it lives in the internal test package so it can call the unexported
// method). The regression: sessionCwd used to consult only the live gofer/roster,
// which never lists an OFFLINE (journal-reloaded) session, so it returned "" and
// the session/load it feeds fell back to the daemon's own working directory. It
// must now resolve the cwd persisted in the session's journal, read back through
// the daemon's gofer/overview.

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// seedOfflineJournal writes a read-only on-disk journal (meta entry carrying cwd,
// plus a first user message) under root and returns its id — the shape a daemon
// restart leaves behind for a previously-run session.
func seedOfflineJournal(t *testing.T, root, cwd, firstMsg string) string {
	t.Helper()
	store, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		t.Fatalf("session.NewFileStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	j, err := store.Create(context.Background(), session.Slugify(cwd))
	if err != nil {
		t.Fatalf("store.Create: %v", err)
	}
	if _, err := j.Append(session.NewMetaEntry(cwd)); err != nil {
		t.Fatalf("append meta entry: %v", err)
	}
	if _, err := j.Append(session.NewMessageEntry(provider.UserText(firstMsg))); err != nil {
		t.Fatalf("append message entry: %v", err)
	}
	id := j.ID()
	if err := j.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
	return id
}

func TestSessionCwdResolvesOfflineViaOverview(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	id := seedOfflineJournal(t, root, cwd, "investigate the flaky build")

	sup, err := supervisor.New(supervisor.Config{Root: root})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	d := daemon.New(sup, daemon.Config{})
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)

	c, err := daemon.Dial(context.Background(), "ws"+srv.URL[len("http"):], "")
	if err != nil {
		t.Fatalf("daemon.Dial: %v", err)
	}
	r := New(c)
	t.Cleanup(func() { _ = r.Close() })

	ctx := context.Background()

	// Precondition: the session is offline — absent from the live roster — so the
	// old live-only lookup would have returned "".
	if live, err := r.Roster(ctx); err != nil {
		t.Fatalf("Roster: %v", err)
	} else if findByID(live, id) != nil {
		t.Fatalf("precondition: %s should be offline, but is in the live roster", id)
	}

	if got := r.sessionCwd(ctx, id); got != cwd {
		t.Errorf("sessionCwd(offline) = %q, want persisted journal cwd %q", got, cwd)
	}
}

func findByID(dtos []SessionInfo, id string) *SessionInfo {
	for i := range dtos {
		if dtos[i].ID == id {
			return &dtos[i]
		}
	}
	return nil
}
