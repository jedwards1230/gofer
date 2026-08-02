package daemonbridge_test

// cwd_missing_test.go covers the bridge's half of jedwards1230/gofer#326: the
// cwd a Resume puts on the wire, and the single seam a consumer consumes the
// "recorded directory is gone" signal through
// ([daemonbridge.Supervisor.OnSessionCwdMissing]).
//
// The seam exists because the signal arrives on TWO different attach paths — the
// /resume command, which calls Resume directly, and roster-Enter, where the load
// is issued by the reconstruction core on first reference and its error never
// reaches a caller at all. A consumer that had to handle those separately would
// handle one of them.

import (
	"context"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/daemonbridge"
	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/tui"
)

const cwdMissingWait = 5 * time.Second

// deletedCwdSession creates a session in a real directory, takes it OFFLINE, and
// then deletes that directory — returning the daemon URL, the session id, and
// the now-missing directory the daemon has recorded for it.
//
// The offline step is not incidental. The signal these tests are about only
// applies to a session that has to be STARTED somewhere: a session that is
// already live is returned by the supervisor's own already-live early return
// with the requested cwd ignored entirely, so the daemon resolves nothing for it
// and attaching to it keeps working however its directory has changed (that
// branch is covered by internal/daemon's
// TestSessionLoadLiveSessionWithDeletedCwdStillAttaches). Archiving is the
// cheapest way to reach "on disk, not live" without restarting the daemon.
func deletedCwdSession(t *testing.T) (url, sessionID, gone string) {
	t.Helper()
	sup := newTestSupervisor(t, fauxProvider)
	url = newTestDaemon(t, sup)

	cwd := t.TempDir()
	b := newBridge(t, url)
	info, err := b.Create(context.Background(), "", tui.CreateOptions{Cwd: cwd, Model: "faux"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.Archive(context.Background(), info.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if err := os.RemoveAll(cwd); err != nil {
		t.Fatalf("delete the session's cwd: %v", err)
	}
	return url, info.ID, cwd
}

// watchCwdMissing registers a handler on b and returns the channel it delivers
// on. Buffered, because the handler runs on a background goroutine that must not
// block (see OnSessionCwdMissing's doc).
func watchCwdMissing(b *daemonbridge.Supervisor) chan [2]string {
	got := make(chan [2]string, 4)
	b.OnSessionCwdMissing(func(sessionID, cwd string) { got <- [2]string{sessionID, cwd} })
	return got
}

// TestResumeBlankCwdReopensWhereRecorded pins the meaning a blank cwd now
// carries through this seam: "reopen the session where it was recorded". The TUI
// passes blank for a plain resume instead of echoing a roster row's cwd back,
// and the session must still come up in its own directory rather than the
// daemon's.
func TestResumeBlankCwdReopensWhereRecorded(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	url := newTestDaemon(t, sup)
	cwd := t.TempDir()

	first := newBridge(t, url)
	info, err := first.Create(context.Background(), "", tui.CreateOptions{Cwd: cwd, Model: "faux"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := first.Archive(context.Background(), info.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	b := newBridge(t, url)
	if err := b.Resume(context.Background(), info.ID, ""); err != nil {
		t.Fatalf("Resume with a blank cwd: %v", err)
	}

	roster, err := b.Roster(context.Background())
	if err != nil {
		t.Fatalf("Roster: %v", err)
	}
	var found bool
	for _, r := range roster {
		if r.ID == info.ID {
			found = true
			if r.Cwd != cwd {
				t.Errorf("resumed session cwd = %q, want the recorded %q — a blank cwd did not resolve to the journal's", r.Cwd, cwd)
			}
		}
	}
	if !found {
		t.Fatalf("session %s is not on the roster after a blank-cwd Resume: %+v", info.ID, roster)
	}
}

// TestResumeRelaysCwdMissingToTheHandler covers the /resume path: the typed
// error both reaches the registered handler (with the missing directory) and is
// returned wrapped, so a caller may branch on either without ever reading the
// message text.
func TestResumeRelaysCwdMissingToTheHandler(t *testing.T) {
	url, sessionID, gone := deletedCwdSession(t)
	b := newBridge(t, url)
	got := watchCwdMissing(b)

	err := b.Resume(context.Background(), sessionID, "")
	if err == nil {
		t.Fatal("Resume with a deleted recorded cwd succeeded — a directory was substituted")
	}
	missing, ok := daemon.SessionCwdMissing(err)
	if !ok {
		t.Fatalf("Resume error is not the typed cwd-missing signal: %v", err)
	}
	if missing != gone {
		t.Errorf("typed error cwd = %q, want %q", missing, gone)
	}

	select {
	case call := <-got:
		if call[0] != sessionID || call[1] != gone {
			t.Errorf("handler called with %+v, want [%s %s]", call, sessionID, gone)
		}
	case <-time.After(cwdMissingWait):
		t.Fatal("the /resume path never reached the registered cwd-missing handler")
	}
}

// TestSubscribeRelaysCwdMissingToTheHandler covers the roster-Enter path, where
// nothing returns an error to anyone: the load is issued by the reconstruction
// core on first reference and its result is discarded. Without this relay the
// operator gets an attach that renders an empty transcript and says nothing.
func TestSubscribeRelaysCwdMissingToTheHandler(t *testing.T) {
	url, sessionID, gone := deletedCwdSession(t)
	b := newBridge(t, url)
	got := watchCwdMissing(b)

	sub, err := b.Subscribe(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	select {
	case call := <-got:
		if call[0] != sessionID || call[1] != gone {
			t.Errorf("handler called with %+v, want [%s %s]", call, sessionID, gone)
		}
	case <-time.After(cwdMissingWait):
		t.Fatal("the roster-Enter (Subscribe) path never reached the registered cwd-missing handler")
	}
}

// TestExplicitBadCwdIsNotACwdMissingSignal keeps the two failures apart. A
// directory the USER chose that does not exist is a rejected choice — it must
// surface as a plain error, not as another "your directory is gone" prompt, or
// the re-init flow would loop on itself instead of telling the user their pick
// was bad.
func TestExplicitBadCwdIsNotACwdMissingSignal(t *testing.T) {
	url, sessionID, gone := deletedCwdSession(t)
	b := newBridge(t, url)
	got := watchCwdMissing(b)

	err := b.Resume(context.Background(), sessionID, gone+"-a-different-bad-pick")
	if err == nil {
		t.Fatal("Resume with an explicit nonexistent cwd succeeded, want an error")
	}
	if missing, ok := daemon.SessionCwdMissing(err); ok {
		t.Errorf("an explicitly chosen bad directory reported as cwd-missing (%q); it must stay a plain rejection", missing)
	}
	select {
	case call := <-got:
		t.Errorf("handler fired for an explicitly chosen bad directory: %+v", call)
	default:
	}
}

// loadCountingSup wraps a real supervisor to record the cwd of every
// session/load that got as far as resuming — i.e. one entry per load the daemon
// actually accepted, in order. Embedding the daemon's own consumer interface
// (satisfied by *supervisor.Supervisor) means only Resume is overridden and
// every other method passes through untouched.
type loadCountingSup struct {
	daemon.Supervisor

	mu      sync.Mutex
	resumes []string
}

func (s *loadCountingSup) Resume(ctx context.Context, id string, opts supervisor.ResumeOptions) (supervisor.SessionInfo, error) {
	s.mu.Lock()
	s.resumes = append(s.resumes, opts.Cwd)
	s.mu.Unlock()
	return s.Supervisor.Resume(ctx, id, opts)
}

func (s *loadCountingSup) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.resumes...)
}

// TestCwdMissingRetryIsArmedOncePerAttachAndSupersededByAReInit walks the whole
// remedy the way the TUI drives it, and pins BOTH halves of the retry at once —
// which is the point of doing it in one test: each half alone is satisfiable by
// breaking the other.
//
//	attach            → blank load, recorded dir gone → signal #1, nothing resumed
//	attach again      → the retry: a SECOND load, signal #2 (the remedy is not
//	                    one-shot — cancel then Enter again must re-raise it)
//	re-init(explicit) → exactly ONE resume, at the directory the user picked
//	attach after that → NOTHING further: no second load, no duplicate replay
//
// The last line is the defect this test was written for. A history load is armed
// for retry when it fails this way, and the arming used to be consumed by ANY
// reference to the session — including the demuxer's per-frame lookup, which the
// re-init's own history replay drives, and including the consumer's follow-up
// Subscribe after the resume succeeds. Either one fired a second, blank-cwd
// session/load; that load now SUCCEEDS (the session is live by then) and replays
// the session's whole history onto the same broker again — the double-render
// internal/tui's resumeSession exists to avoid, on every successful re-init.
//
// Counting resumes rather than raw frames is what makes it precise: a load that
// fails cwd resolution never reaches Resume, so `resumes` counts exactly the
// loads the daemon accepted, and `signals` counts exactly the ones it refused.
func TestCwdMissingRetryIsArmedOncePerAttachAndSupersededByAReInit(t *testing.T) {
	cwd := t.TempDir()

	counting := &loadCountingSup{Supervisor: newTestSupervisor(t, fauxProvider)}
	d := daemon.New(counting, daemon.Config{DefaultModel: "faux"})
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	url := "ws" + srv.URL[len("http"):]

	setup := newBridge(t, url)
	info, err := setup.Create(context.Background(), "", tui.CreateOptions{Cwd: cwd, Model: "faux"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := setup.Archive(context.Background(), info.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if err := os.RemoveAll(cwd); err != nil {
		t.Fatalf("delete the session's cwd: %v", err)
	}

	b := newBridge(t, url)
	signals := watchCwdMissing(b)

	// 1. The first attach fails and raises the prompt.
	first, err := b.Subscribe(context.Background(), info.ID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	awaitCwdMissing(t, signals, info.ID, cwd, "the first attach")
	first.Close()

	// 2. The user cancelled and pressed Enter on the same row again. This must
	//    retry — a memoized load would show a silent empty attach instead.
	second, err := b.Subscribe(context.Background(), info.ID)
	if err != nil {
		t.Fatalf("second Subscribe: %v", err)
	}
	awaitCwdMissing(t, signals, info.ID, cwd, "the second attach (the retry)")
	second.Close()

	if got := counting.recorded(); len(got) != 0 {
		t.Fatalf("resumes = %q after two REFUSED loads, want none", got)
	}

	// 3. The re-init the prompt drives: an explicit directory the user picked.
	rebased := t.TempDir()
	if err := b.Resume(context.Background(), info.ID, rebased); err != nil {
		t.Fatalf("re-init Resume: %v", err)
	}

	// 4. What the TUI does next on a successful resume: attach into the session
	//    (App.switchSession → Subscribe). It must issue NO further load.
	after, err := b.Subscribe(context.Background(), info.ID)
	if err != nil {
		t.Fatalf("Subscribe after the re-init: %v", err)
	}
	defer after.Close()

	// Give any spurious retry the same window the real ones needed to land.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(counting.recorded()) > 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	got := counting.recorded()
	if len(got) != 1 {
		t.Fatalf("resumes = %q, want exactly one (the re-init at %q) — a second load replays the session's "+
			"whole history onto the broker again, which the attach transcript then renders twice", got, rebased)
	}
	if got[0] != rebased {
		t.Errorf("resume cwd = %q, want the directory the user picked (%q)", got[0], rebased)
	}
	select {
	case call := <-signals:
		t.Errorf("a cwd-missing signal fired after a SUCCESSFUL re-init: %+v", call)
	default:
	}
}

// awaitCwdMissing waits for one signal naming sessionID and gone, failing with
// which step of the walk above was expecting it.
func awaitCwdMissing(t *testing.T, signals chan [2]string, sessionID, gone, step string) {
	t.Helper()
	select {
	case call := <-signals:
		if call[0] != sessionID || call[1] != gone {
			t.Fatalf("%s signalled %+v, want [%s %s]", step, call, sessionID, gone)
		}
	case <-time.After(cwdMissingWait):
		t.Fatalf("%s issued no session/load: the failed load was memoized, so the prompt never re-opens", step)
	}
}

// TestOnSessionCwdMissingNilClearsTheHandler pins the registration's off switch,
// so a consumer tearing its prompt down cannot leave a stale closure being
// called from a background goroutine.
func TestOnSessionCwdMissingNilClearsTheHandler(t *testing.T) {
	url, sessionID, _ := deletedCwdSession(t)
	b := newBridge(t, url)
	got := watchCwdMissing(b)
	b.OnSessionCwdMissing(nil)

	if err := b.Resume(context.Background(), sessionID, ""); err == nil {
		t.Fatal("Resume with a deleted recorded cwd succeeded — a directory was substituted")
	}
	select {
	case call := <-got:
		t.Errorf("handler fired after being cleared: %+v", call)
	default:
	}
}
