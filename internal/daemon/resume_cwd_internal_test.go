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
	"encoding/json"
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

// TestResolveLoadCwdMissingPersistedDirIsTyped replaces the assertion this test
// used to make — that a deleted persisted directory FELL BACK to the daemon's
// own working directory. That fallback was the bug (jedwards1230/gofer#326): it
// reported success while reopening the session against a directory the user
// never chose, so every cwd-scoped input (project config, user commands, skills,
// file resolution) came from a stranger's project.
//
// The recorded-but-gone case must now answer with the typed
// [codeSessionCwdMissing] error CARRYING the missing path structurally, so a
// client can name it while offering somewhere else to reopen the session.
func TestResolveLoadCwdMissingPersistedDirIsTyped(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "deleted-project")
	const id = "sess-1"
	d := newCwdDaemon(supervisor.SessionInfo{ID: id, Cwd: gone})

	got, rerr := resolveLoadCwd(d, context.Background(), id, "")
	if rerr == nil {
		t.Fatalf("resolveLoadCwd with a deleted persisted dir returned %q and no error — it substituted a directory", got)
	}
	if got != "" {
		t.Errorf("resolveLoadCwd returned cwd %q alongside an error, want none", got)
	}
	if rerr.Code != codeSessionCwdMissing {
		t.Errorf("error code = %d, want %d (session cwd missing)", rerr.Code, codeSessionCwdMissing)
	}
	var data sessionCwdMissingData
	if err := json.Unmarshal(rerr.Data, &data); err != nil {
		t.Fatalf("decode error data %q: %v", rerr.Data, err)
	}
	if data.Cwd != gone {
		t.Errorf("error data cwd = %q, want the recorded directory %q", data.Cwd, gone)
	}
}

// TestResolveLoadCwdNeverSubstitutesAMissingRecordedDir is the no-silent-
// substitution gate, tabled across EVERY resolveLoadCwd branch: whenever a
// session has a recorded working directory that no longer resolves, no branch
// may answer with a different directory. Either the recorded one comes back, or
// an error does.
//
// It is deliberately phrased as a property over the whole function rather than
// as one case's expected value: the bug it guards was a fall-through added for
// one branch that silently applied to a request no client ever made until every
// client stopped sending an explicit cwd. A new branch that "helpfully" defaults
// somewhere fails here without anyone remembering to extend a case list.
func TestResolveLoadCwdNeverSubstitutesAMissingRecordedDir(t *testing.T) {
	daemonWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir: %v", err)
	}

	// A directory that is neither the daemon's working directory nor its home,
	// so the explicit-cwd case below can only pass by actually honoring it.
	explicitDir := t.TempDir()

	tests := []struct {
		name string
		// recorded is the session's journal cwd; blank means the session has no
		// recorded cwd at all (and is absent from the roster).
		recorded string
		// live marks the recorded session as already running, the one branch
		// that resolves nothing at all.
		live bool
		// reqCwd is what the client sent on session/load.
		reqCwd string
		// wantErr is whether this branch must refuse rather than resolve.
		wantErr bool
		// wantCwd is the only acceptable resolved directory when wantErr is false.
		wantCwd string
	}{
		{
			name:     "blank cwd, recorded dir gone: refuses",
			recorded: filepath.Join(t.TempDir(), "deleted-project"),
			wantErr:  true,
		},
		{
			name:     "blank cwd, recorded dir is a FILE: refuses",
			recorded: writeTempFile(t),
			wantErr:  true,
		},
		{
			name:     "blank cwd, recorded dir is relative: refuses",
			recorded: "deleted-project",
			wantErr:  true,
		},
		{
			// reqCwd is a temp dir, NOT the daemon's own working directory: with
			// the two equal, a mutation that dropped the explicit branch and fell
			// through to resolveSessionCwd("") would answer the identical value,
			// and the ambient guard below (skipped when want == ambient) would be
			// disabled at the same time. The case could not fail for its stated
			// reason.
			name:     "explicit cwd, recorded dir gone: honors the explicit dir",
			recorded: filepath.Join(t.TempDir(), "deleted-project"),
			reqCwd:   explicitDir,
			wantCwd:  explicitDir,
		},
		{
			name:    "explicit cwd that is itself gone: refuses",
			reqCwd:  filepath.Join(t.TempDir(), "never-existed"),
			wantErr: true,
		},
		{
			name:     "blank cwd, recorded dir exists: resolves to it",
			recorded: t.TempDir(),
		},
		{
			name:    "no recorded cwd at all: daemon getwd (pre-existing, nothing to substitute away from)",
			wantCwd: daemonWd,
		},
		{
			// A LIVE session resolves nothing: Resume ignores the cwd for an
			// already-running session, so its own (gone) directory comes straight
			// back. Still no substitution — that IS where it is running.
			name:     "blank cwd, session is live with a gone dir: passes its own dir through",
			recorded: filepath.Join(t.TempDir(), "deleted-worktree"),
			live:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			const id = "sess-1"
			var d *Daemon
			if tc.recorded == "" {
				d = newCwdDaemon()
			} else {
				d = newCwdDaemon(supervisor.SessionInfo{ID: id, Cwd: tc.recorded, Live: tc.live})
			}

			got, rerr := resolveLoadCwd(d, context.Background(), id, tc.reqCwd)
			if tc.wantErr {
				if rerr == nil {
					t.Fatalf("resolveLoadCwd = %q, want an error — a directory was substituted for the unusable recorded one", got)
				}
				if got != "" {
					t.Errorf("resolveLoadCwd returned cwd %q alongside its error, want none", got)
				}
				return
			}
			if rerr != nil {
				t.Fatalf("resolveLoadCwd: unexpected error %+v", rerr)
			}
			want := tc.wantCwd
			if want == "" {
				want = tc.recorded
			}
			if got != want {
				t.Fatalf("resolveLoadCwd = %q, want %q", got, want)
			}
			// The substitution guard proper: whatever the branch, the answer is
			// never one of the ambient directories the daemon could reach for
			// (its own working directory, the daemon's home) unless that IS the
			// directory this case legitimately expects.
			for _, ambient := range []string{daemonWd, home} {
				if got == ambient && want != ambient {
					t.Errorf("resolveLoadCwd substituted the ambient directory %q for the session's own %q", ambient, want)
				}
			}
		})
	}
}

// writeTempFile creates a regular file in a fresh temp dir and returns its path
// — a "recorded cwd" that exists but is not a directory.
func writeTempFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// TestResolveLoadCwdLiveSessionResolvesNothing pins the already-live branch.
//
// [supervisor.Supervisor.Resume] returns the existing snapshot for a live id
// without building a second runner, so the cwd handed to it is ignored. Deciding
// where to START a session that is already running is a question with no answer,
// and answering it anyway produced two bad outcomes: a session whose directory
// was deleted underneath it became unattachable, and a client that offered to
// re-init it somewhere else had its chosen directory silently discarded while
// its UI stated the session had been reopened there.
//
// The assertion is the DELETED directory coming back unchanged: it is the one
// value that proves no resolution ran (resolveSessionCwd would have refused it)
// and no substitute was invented.
func TestResolveLoadCwdLiveSessionResolvesNothing(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "deleted-worktree")
	const id = "sess-1"
	d := newCwdDaemon(supervisor.SessionInfo{ID: id, Cwd: gone, Live: true})

	got, rerr := resolveLoadCwd(d, context.Background(), id, "")
	if rerr != nil {
		t.Fatalf("resolveLoadCwd for a LIVE session whose dir was deleted: %+v — it is already running, "+
			"so there is no directory to decide and nothing to refuse", rerr)
	}
	if got != gone {
		t.Errorf("resolveLoadCwd = %q, want the live session's own %q", got, gone)
	}
	if wd, _ := os.Getwd(); got == wd {
		t.Errorf("resolveLoadCwd substituted the daemon working dir %q", wd)
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
