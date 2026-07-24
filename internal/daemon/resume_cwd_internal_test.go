package daemon

// resume_cwd_internal_test.go covers the session/load cwd resolution
// (resolveLoadCwd + persistedCwd) directly — it lives in the internal test
// package so it can call the unexported helpers and construct a Daemon around a
// stub Supervisor. The regression under test: an offline (journal-reloaded)
// session resumed with a BLANK cwd used to reopen in the daemon's own working
// directory (typically "/" under launchd/systemd), regrouping it under a bogus
// "/" header. It must instead reopen in the cwd persisted in its journal meta,
// which the daemon reads back through the supervisor's List.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// listCwdSup is a Supervisor whose List returns a fixed row set, for exercising
// persistedCwd/resolveLoadCwd without a real store. Every other method is the
// nil embedded interface — none is called on this path.
type listCwdSup struct {
	Supervisor
	rows []supervisor.SessionInfo
}

func (s listCwdSup) List(context.Context) ([]supervisor.SessionInfo, error) {
	return s.rows, nil
}

func newCwdDaemon(rows ...supervisor.SessionInfo) *Daemon {
	return New(listCwdSup{rows: rows}, Config{ListenAddr: "127.0.0.1:0"})
}

// TestResolveLoadCwdBlankUsesPersistedCwd is the core assertion: a blank
// client cwd resolves to the session's persisted journal cwd, not os.Getwd().
// The persisted dir is a temp dir deliberately different from the test process's
// working directory, so a getwd fallback would return a demonstrably wrong path.
func TestResolveLoadCwdBlankUsesPersistedCwd(t *testing.T) {
	persisted := t.TempDir()
	const id = "sess-1"
	d := newCwdDaemon(supervisor.SessionInfo{ID: id, Cwd: persisted})

	got, rerr := resolveLoadCwd(d, context.Background(), id, "")
	if rerr != nil {
		t.Fatalf("resolveLoadCwd: %v", rerr)
	}
	if got != persisted {
		t.Errorf("resolveLoadCwd(blank) = %q, want persisted journal cwd %q", got, persisted)
	}
	if wd, _ := os.Getwd(); got == wd {
		t.Errorf("resolveLoadCwd returned the daemon working dir %q — the persisted cwd was not substituted", wd)
	}
}

// TestResolveLoadCwdClientCwdWins keeps ACP precedence: a non-blank client cwd
// is authoritative and is NOT overridden by the persisted cwd.
func TestResolveLoadCwdClientCwdWins(t *testing.T) {
	persisted := t.TempDir()
	clientCwd := t.TempDir()
	const id = "sess-1"
	d := newCwdDaemon(supervisor.SessionInfo{ID: id, Cwd: persisted})

	got, rerr := resolveLoadCwd(d, context.Background(), id, clientCwd)
	if rerr != nil {
		t.Fatalf("resolveLoadCwd: %v", rerr)
	}
	if got != clientCwd {
		t.Errorf("resolveLoadCwd(client %q) = %q, want the client cwd", clientCwd, got)
	}
}

// TestResolveLoadCwdMissingPersistedDirFallsBack proves a resume that used to
// succeed is not hard-failed when the persisted directory has since been
// deleted: it falls back to the daemon's working directory rather than erroring.
func TestResolveLoadCwdMissingPersistedDirFallsBack(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "deleted-project")
	const id = "sess-1"
	d := newCwdDaemon(supervisor.SessionInfo{ID: id, Cwd: gone})

	got, rerr := resolveLoadCwd(d, context.Background(), id, "")
	if rerr != nil {
		t.Fatalf("resolveLoadCwd should fall back, not fail: %v", rerr)
	}
	wd, _ := os.Getwd()
	if got != wd {
		t.Errorf("resolveLoadCwd with a deleted persisted dir = %q, want daemon getwd %q", got, wd)
	}
}

// TestResolveLoadCwdUnknownSessionFallsBack covers the no-persisted-cwd case
// (session absent from List): the daemon-getwd default still applies.
func TestResolveLoadCwdUnknownSessionFallsBack(t *testing.T) {
	d := newCwdDaemon() // empty roster

	got, rerr := resolveLoadCwd(d, context.Background(), "nope", "")
	if rerr != nil {
		t.Fatalf("resolveLoadCwd: %v", rerr)
	}
	wd, _ := os.Getwd()
	if got != wd {
		t.Errorf("resolveLoadCwd(unknown) = %q, want daemon getwd %q", got, wd)
	}
}
