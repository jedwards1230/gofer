package tuibridge_test

// cwd_test.go covers what the IN-PROCESS backend does with the cwd it is asked
// to resume a session in (jedwards1230/gofer#326).
//
// The regression these exist for: the TUI stopped echoing a session's recorded
// cwd back and started sending BLANK for "reopen where it was recorded". The
// daemon path resolves that; this one did not resolve anything at all, so the
// empty string flowed through supervisor.Resume into runner.Options{Cwd: ""} and
// the session's project config, user commands, skills and every file tool rooted
// at whatever directory the gofer PROCESS was started in — silently, and
// reported as a successful resume.
//
// Every assertion here is about the cwd that reaches runner.Options, because
// that is the value with consequences; the supervisor never validates it, so a
// wrong one is invisible until a tool call resolves against a stranger's files.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/provider/faux"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tuibridge"
)

// cwdRecorder is the supervisor plus the cwd every RESUME reached the runner
// with — the one value these tests are about.
type cwdRecorder struct {
	sup *supervisor.Supervisor
	// root is the store root, so a test can corrupt the store under the
	// supervisor's feet (see TestAdapterResumeFailsWhenTheRecordedCwdCannotBeRead).
	root string

	mu         sync.Mutex
	resumeCwds []string
}

func (r *cwdRecorder) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.resumeCwds...)
}

// newCwdRecordingSupervisor builds a real supervisor over a temp store whose
// ResumeSession records opts.Cwd before handing off to the SDK, substituting the
// faux provider so no network is touched.
func newCwdRecordingSupervisor(t *testing.T) *cwdRecorder {
	t.Helper()
	root := t.TempDir()
	store, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		t.Fatalf("session.NewFileStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	rec := &cwdRecorder{}
	sup, err := supervisor.New(supervisor.Config{
		Root:  root,
		Store: store,
		NewSession: func(ctx context.Context, opts runner.Options) (supervisor.Session, error) {
			opts.Store = store
			opts.Model = "faux"
			opts.Provider = faux.New(faux.Default())
			return runner.New(ctx, opts)
		},
		ResumeSession: func(ctx context.Context, id string, opts runner.Options) (supervisor.Session, error) {
			rec.mu.Lock()
			rec.resumeCwds = append(rec.resumeCwds, opts.Cwd)
			rec.mu.Unlock()
			opts.Store = store
			opts.Model = "faux"
			opts.Provider = faux.New(faux.Default())
			return runner.Resume(ctx, id, opts)
		},
	})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })
	rec.sup = sup
	rec.root = root
	return rec
}

// offlineSession creates a session in cwd through a and archives it, so it is on
// disk with a recorded cwd and NOT live — the state a plain resume is for.
func offlineSession(t *testing.T, a tuibridge.Adapter, cwd string) string {
	t.Helper()
	info, err := a.Create(context.Background(), "", tui.CreateOptions{Cwd: cwd})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := a.Archive(context.Background(), info.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	return info.ID
}

// watchCwdMissing registers a handler on a and returns the buffered channel it
// delivers on.
func watchCwdMissing(a tuibridge.Adapter) chan [2]string {
	got := make(chan [2]string, 4)
	a.OnSessionCwdMissing(func(sessionID, cwd string) { got <- [2]string{sessionID, cwd} })
	return got
}

// TestAdapterResumeBlankCwdReopensWhereRecorded is the load-bearing one: a blank
// cwd means "reopen this session where it was RECORDED", and this backend has to
// answer that itself — supervisor.Resume performs no cwd resolution, so a blank
// reaching it becomes the gofer process's own working directory for the tools,
// the project config and the skills, with nothing on screen saying so.
func TestAdapterResumeBlankCwdReopensWhereRecorded(t *testing.T) {
	rec := newCwdRecordingSupervisor(t)
	a := tuibridge.New(rec.sup, func(context.Context) string { return "claude-sonnet-5" })

	cwd := t.TempDir()
	id := offlineSession(t, a, cwd)

	if err := a.Resume(context.Background(), id, ""); err != nil {
		t.Fatalf("Resume with a blank cwd: %v", err)
	}

	got := rec.recorded()
	if len(got) != 1 {
		t.Fatalf("ResumeSession called %d times, want 1", len(got))
	}
	if got[0] != cwd {
		wd, _ := os.Getwd()
		t.Errorf("resume cwd = %q, want the recorded %q (the TUI process's own directory is %q)", got[0], cwd, wd)
	}
}

// TestAdapterResumeNeverSubstitutesTheProcessCwd is the property stated
// negatively, and is the assertion that would have caught the regression: for a
// session with a recorded directory, no Resume may ever reach the runner with
// the empty string or with this process's working directory.
func TestAdapterResumeNeverSubstitutesTheProcessCwd(t *testing.T) {
	rec := newCwdRecordingSupervisor(t)
	a := tuibridge.New(rec.sup, func(context.Context) string { return "claude-sonnet-5" })
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}

	cwd := t.TempDir()
	id := offlineSession(t, a, cwd)
	_ = a.Resume(context.Background(), id, "")

	for _, got := range rec.recorded() {
		if got == "" {
			t.Errorf("a resume reached the runner with an EMPTY cwd — every cwd-scoped input roots at %q", wd)
		}
		if got == wd {
			t.Errorf("a resume reached the runner with the TUI process's own directory %q, not the session's %q", got, cwd)
		}
	}
}

// TestAdapterResumeSignalsWhenTheRecordedDirIsGone is the in-process half of the
// three-way prompt: the recorded directory no longer exists, so the resume must
// FAIL with the signal raised — never silently reopen the session somewhere
// else. Without it the daemonless TUI is the one backend where the prompt could
// not appear, and the substitution it prevents happens instead.
func TestAdapterResumeSignalsWhenTheRecordedDirIsGone(t *testing.T) {
	rec := newCwdRecordingSupervisor(t)
	a := tuibridge.New(rec.sup, func(context.Context) string { return "claude-sonnet-5" })
	got := watchCwdMissing(a)

	cwd := t.TempDir()
	id := offlineSession(t, a, cwd)
	if err := os.RemoveAll(cwd); err != nil {
		t.Fatalf("delete the session's cwd: %v", err)
	}

	if err := a.Resume(context.Background(), id, ""); err == nil {
		t.Fatal("Resume with a deleted recorded cwd succeeded — a directory was substituted")
	}
	if n := len(rec.recorded()); n != 0 {
		t.Errorf("ResumeSession called %d times for a session whose directory is gone, want 0: %q", n, rec.recorded())
	}
	select {
	case call := <-got:
		if call[0] != id || call[1] != cwd {
			t.Errorf("handler called with %+v, want [%s %s]", call, id, cwd)
		}
	default:
		t.Fatal("the in-process backend never raised the cwd-missing signal, so the TUI shows a bare error instead of the prompt")
	}
}

// TestAdapterResumeExplicitBadCwdIsRejectedNotSignalled keeps the two failures
// apart, exactly as the daemon does: a directory the USER picked in the re-init
// prompt that does not exist is a rejected choice — a plain error — not another
// "your directory is gone" signal, which would loop the prompt on itself.
func TestAdapterResumeExplicitBadCwdIsRejectedNotSignalled(t *testing.T) {
	rec := newCwdRecordingSupervisor(t)
	a := tuibridge.New(rec.sup, func(context.Context) string { return "claude-sonnet-5" })
	got := watchCwdMissing(a)

	id := offlineSession(t, a, t.TempDir())
	bad := filepath.Join(t.TempDir(), "never-existed")

	err := a.Resume(context.Background(), id, bad)
	if err == nil {
		t.Fatal("Resume with an explicit nonexistent cwd succeeded, want an error")
	}
	if !strings.Contains(err.Error(), bad) {
		t.Errorf("error %q does not name the directory the user picked (%q)", err, bad)
	}
	if n := len(rec.recorded()); n != 0 {
		t.Errorf("ResumeSession called %d times with a nonexistent explicit cwd, want 0: %q", n, rec.recorded())
	}
	select {
	case call := <-got:
		t.Errorf("an explicitly chosen bad directory raised the cwd-missing signal: %+v", call)
	default:
	}
}

// TestAdapterResumeExplicitCwdWins pins ACP's precedence on this backend too: a
// non-blank cwd is authoritative and is NOT overridden by the recorded one. It is
// what the re-init prompt commits, so a session really does reopen where the user
// said.
func TestAdapterResumeExplicitCwdWins(t *testing.T) {
	rec := newCwdRecordingSupervisor(t)
	a := tuibridge.New(rec.sup, func(context.Context) string { return "claude-sonnet-5" })

	id := offlineSession(t, a, t.TempDir())
	rebased := t.TempDir()

	if err := a.Resume(context.Background(), id, rebased); err != nil {
		t.Fatalf("Resume with an explicit cwd: %v", err)
	}
	got := rec.recorded()
	if len(got) != 1 || got[0] != rebased {
		t.Errorf("resume cwds = %q, want [%s]", got, rebased)
	}
}

// TestAdapterResumeLiveSessionWithDeletedCwdStillAttaches is the live-session
// branch. supervisor.Resume returns the existing snapshot for a session that is
// already running without building a second runner, so the cwd it is handed is
// ignored — which means resolving one can only invent a failure. A session
// running in a directory the user has since deleted (rm -rf on a worktree) must
// still be attachable: it IS running, and watching it is exactly what the user
// wants at that moment.
func TestAdapterResumeLiveSessionWithDeletedCwdStillAttaches(t *testing.T) {
	rec := newCwdRecordingSupervisor(t)
	a := tuibridge.New(rec.sup, func(context.Context) string { return "claude-sonnet-5" })
	got := watchCwdMissing(a)

	cwd := t.TempDir()
	info, err := a.Create(context.Background(), "", tui.CreateOptions{Cwd: cwd})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.RemoveAll(cwd); err != nil {
		t.Fatalf("delete the live session's cwd: %v", err)
	}

	if err := a.Resume(context.Background(), info.ID, ""); err != nil {
		t.Fatalf("Resume of a LIVE session whose cwd was deleted: %v — it is already running; attaching must work", err)
	}
	if n := len(rec.recorded()); n != 0 {
		t.Errorf("ResumeSession called %d times for an already-live session, want 0", n)
	}
	select {
	case call := <-got:
		t.Errorf("attaching a live session raised the cwd-missing signal (%+v) — the prompt it opens would offer a "+
			"re-init the supervisor's already-live early return then discards", call)
	default:
	}
}

// TestAdapterResumeFailsWhenTheRecordedCwdCannotBeRead pins that an unreadable
// session store fails the resume rather than degrading into the branch for a
// session with NO recorded directory — which passes "" through to the
// supervisor and roots the session in the gofer process's own directory.
//
// "The store read failed" and "this session has no recorded directory" are
// different facts, and only the second one is safe to resume on. Collapsing them
// turns a transient disk error into a silent substitution, which is the one
// thing this whole change exists to prevent.
//
// The failure is injected by replacing the store's sessions/ DIRECTORY with a
// regular file: os.ReadDir then fails with ENOTDIR for any user, including root
// (a permissions-based injection would silently not fail under a root test
// runner, and there is no t.Skip to fall back on).
func TestAdapterResumeFailsWhenTheRecordedCwdCannotBeRead(t *testing.T) {
	rec := newCwdRecordingSupervisor(t)
	a := tuibridge.New(rec.sup, func(context.Context) string { return "claude-sonnet-5" })

	id := offlineSession(t, a, t.TempDir())

	sessionsDir := filepath.Join(rec.root, "sessions")
	if err := os.RemoveAll(sessionsDir); err != nil {
		t.Fatalf("remove the sessions dir: %v", err)
	}
	if err := os.WriteFile(sessionsDir, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write a file over the sessions dir: %v", err)
	}

	err := a.Resume(context.Background(), id, "")
	if err == nil {
		t.Fatal("Resume succeeded with an unreadable session store — the recorded directory was unknown, " +
			"so a directory was substituted")
	}
	if n := len(rec.recorded()); n != 0 {
		t.Errorf("ResumeSession called %d times (cwds %q) despite the store read failing, want 0 — the resume "+
			"reached the runner with a directory nobody could verify", n, rec.recorded())
	}
}

// TestAdapterOnSessionCwdMissingNilClearsTheHandler pins the registration's off
// switch, mirroring the daemon-backed bridge's: a consumer tearing its prompt
// down must not leave a stale closure being called from a background goroutine.
func TestAdapterOnSessionCwdMissingNilClearsTheHandler(t *testing.T) {
	rec := newCwdRecordingSupervisor(t)
	a := tuibridge.New(rec.sup, func(context.Context) string { return "claude-sonnet-5" })
	got := watchCwdMissing(a)
	a.OnSessionCwdMissing(nil)

	cwd := t.TempDir()
	id := offlineSession(t, a, cwd)
	if err := os.RemoveAll(cwd); err != nil {
		t.Fatalf("delete the session's cwd: %v", err)
	}
	if err := a.Resume(context.Background(), id, ""); err == nil {
		t.Fatal("Resume with a deleted recorded cwd succeeded — a directory was substituted")
	}
	select {
	case call := <-got:
		t.Errorf("handler fired after being cleared: %+v", call)
	default:
	}
}

// TestAdapterSatisfiesTheCwdMissingNotifier is the seam check the TUI's own
// type assertion depends on. [tui.App] asserts its Supervisor against an
// unexported interface with this method and degrades SILENTLY when it does not
// hold, so a signature drift here would not fail to compile anywhere — it would
// just stop the prompt from ever opening on the in-process backend, invisibly.
func TestAdapterSatisfiesTheCwdMissingNotifier(t *testing.T) {
	rec := newCwdRecordingSupervisor(t)
	var sup tui.Supervisor = tuibridge.New(rec.sup, nil)
	if _, ok := sup.(interface {
		OnSessionCwdMissing(func(sessionID, cwd string))
	}); !ok {
		t.Fatal("tuibridge.Adapter no longer satisfies the TUI's cwd-missing notifier seam; " +
			"the in-process backend silently loses the three-way prompt")
	}
}
