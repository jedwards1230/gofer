package wirestream_test

// load_cwd_test.go covers what this core sends as session/load's cwd, and the
// one load failure it routes onward (jedwards1230/gofer#326).
//
// The core used to look a session's cwd up first — gofer/roster, then
// gofer/overview — and send that value back as the load's cwd. It was an ECHO of
// the journal, and the daemon could not tell it apart from a directory the USER
// had chosen: a session whose project directory had been deleted came back as a
// bare invalid-params rejection ("cwd does not exist"), with no way for a client
// to offer the only useful remedy. Sending BLANK restores the distinction the
// wire field already had — blank means "reopen where recorded" — and lets the
// daemon answer the deleted case with a typed, actionable signal.
//
// These are external tests: the behavior only exists over a real session/load
// against a real daemon, which is also the only way to prove what was on the
// wire (a blank cwd and an echoed one produce DIFFERENT daemon replies for a
// deleted directory — -32001 versus -32602).

import (
	"context"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/provider/faux"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/wirestream"
)

const loadCwdWait = 5 * time.Second

// cwdMissingCall is one observed [wirestream.CwdMissingSink] invocation.
type cwdMissingCall struct {
	sessionID string
	cwd       string
}

// seedOfflineJournal writes a read-only on-disk journal (meta entry carrying
// cwd, plus a first user message) under root and returns its id — the shape a
// daemon restart leaves behind for a previously-run session.
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

// newLoadCwdDaemon stands up an in-process daemon over root with a real
// resuming supervisor, and returns its ws:// URL alongside the supervisor (so a
// test can read the roster back without going through the wire).
func newLoadCwdDaemon(t *testing.T, root string) (string, *supervisor.Supervisor) {
	t.Helper()
	store, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		t.Fatalf("session.NewFileStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	build := func(opts runner.Options) runner.Options {
		opts.Store = store
		opts.Model = "faux"
		opts.Provider = faux.New(faux.Default())
		opts.Guard, opts.Approver, opts.Tools = nil, nil, nil
		return opts
	}
	sup, err := supervisor.New(supervisor.Config{
		Root:  root,
		Store: store,
		NewSession: func(ctx context.Context, opts runner.Options) (supervisor.Session, error) {
			return runner.New(ctx, build(opts))
		},
		ResumeSession: func(ctx context.Context, id string, opts runner.Options) (supervisor.Session, error) {
			return runner.Resume(ctx, id, build(opts))
		},
	})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	d := daemon.New(sup, daemon.Config{DefaultModel: "faux"})
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	return "ws" + srv.URL[len("http"):], sup
}

// dialCoreWithCwdSink dials url and returns a Reconstructor with a cwd-missing
// sink installed, plus the channel it delivers on. The channel is buffered so
// the sink never blocks the load goroutine it runs on.
func dialCoreWithCwdSink(t *testing.T, url string) (*wirestream.Reconstructor, chan cwdMissingCall) {
	t.Helper()
	c, err := daemon.Dial(context.Background(), url, "")
	if err != nil {
		t.Fatalf("daemon.Dial: %v", err)
	}
	calls := make(chan cwdMissingCall, 4)
	r := wirestream.New(c, wirestream.WithCwdMissingSink(func(sessionID, cwd string) {
		calls <- cwdMissingCall{sessionID: sessionID, cwd: cwd}
	}))
	t.Cleanup(func() { _ = r.Close() })
	return r, calls
}

// rosterCwd returns sessionID's cwd on the supervisor's roster, or "" if absent.
func rosterCwd(t *testing.T, sup *supervisor.Supervisor, sessionID string) (string, bool) {
	t.Helper()
	infos, err := sup.List(context.Background())
	if err != nil {
		t.Fatalf("supervisor.List: %v", err)
	}
	for _, info := range infos {
		if info.ID == sessionID {
			return info.Cwd, info.Live
		}
	}
	return "", false
}

// TestLoadSendsBlankCwdAndReopensWhereRecorded is the unchanged-behavior half:
// an offline session whose recorded directory still exists must reopen IN that
// directory. The core no longer reads the cwd itself — it sends blank and the
// daemon resolves the journal's — so this pins that the outcome an operator sees
// is identical while the round trip that produced it is gone.
func TestLoadSendsBlankCwdAndReopensWhereRecorded(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	id := seedOfflineJournal(t, root, cwd, "investigate the flaky build")
	url, sup := newLoadCwdDaemon(t, root)

	r, calls := dialCoreWithCwdSink(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), loadCwdWait)
	defer cancel()
	if err := r.Load(ctx, id); err != nil {
		t.Fatalf("Load: %v", err)
	}

	got, live := rosterCwd(t, sup, id)
	if !live {
		t.Fatalf("session %s is not live after Load — the load did not resume it", id)
	}
	if got != cwd {
		t.Errorf("resumed session cwd = %q, want the recorded %q", got, cwd)
	}
	select {
	case call := <-calls:
		t.Errorf("cwd-missing sink fired for a healthy session: %+v", call)
	default:
	}
}

// TestLoadRoutesRecordedCwdMissingToTheSink is the new signal. The recorded
// directory is deleted before the load, so the daemon answers with its typed
// cwd-missing error — which is only reachable BECAUSE the core sent a blank cwd.
// Had it echoed the journal's value back (the old behavior), the daemon would
// have read it as an explicitly chosen directory and answered invalid-params
// instead, and this sink would never fire.
func TestLoadRoutesRecordedCwdMissingToTheSink(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	id := seedOfflineJournal(t, root, cwd, "investigate the flaky build")
	url, sup := newLoadCwdDaemon(t, root)
	if err := os.RemoveAll(cwd); err != nil {
		t.Fatalf("delete the session's cwd: %v", err)
	}

	r, calls := dialCoreWithCwdSink(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), loadCwdWait)
	defer cancel()
	// A failed load still settles — attach is not aborted outright, the
	// pre-existing behavior loadHistory documents.
	if err := r.Load(ctx, id); err != nil {
		t.Fatalf("Load: %v", err)
	}

	select {
	case call := <-calls:
		if call.sessionID != id {
			t.Errorf("sink session id = %q, want %q", call.sessionID, id)
		}
		if call.cwd != cwd {
			t.Errorf("sink cwd = %q, want the recorded (deleted) directory %q", call.cwd, cwd)
		}
	case <-time.After(loadCwdWait):
		t.Fatal("the recorded-cwd-is-gone signal never reached the sink")
	}

	if _, live := rosterCwd(t, sup, id); live {
		t.Errorf("session %s went live despite the missing recorded cwd", id)
	}
}

// TestSecondReferenceAfterCwdMissingLoadsAgain pins that the remedy is not
// ONE-SHOT. A history load is memoized per session id — the map entry is what
// stops a second Subscribe re-loading — and for every other outcome that is
// correct. For this one it is not: the load did not just fail, it raised a
// signal a human answers, and the obvious answer to a prompt is "cancel, then
// try again". With the load memoized, that second attach finds the entry,
// issues no session/load, raises no signal and shows an empty attach screen
// saying nothing — the exact silence the whole change removes, back again on the
// second try.
//
// The second reference here is a Subscribe, the call the TUI's roster-Enter
// makes ([App.enter] → [App.switchSession] → Subscribe).
func TestSecondReferenceAfterCwdMissingLoadsAgain(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	id := seedOfflineJournal(t, root, cwd, "investigate the flaky build")
	url, _ := newLoadCwdDaemon(t, root)
	if err := os.RemoveAll(cwd); err != nil {
		t.Fatalf("delete the session's cwd: %v", err)
	}

	r, calls := dialCoreWithCwdSink(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), loadCwdWait)
	defer cancel()

	first, err := r.Subscribe(ctx, id)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	select {
	case <-calls:
	case <-time.After(loadCwdWait):
		t.Fatal("the first attach never signalled; this test would prove nothing")
	}
	first.Close()

	// The user cancelled the prompt and pressed Enter on the same row again.
	second, err := r.Subscribe(ctx, id)
	if err != nil {
		t.Fatalf("second Subscribe: %v", err)
	}
	select {
	case call := <-calls:
		if call.sessionID != id || call.cwd != cwd {
			t.Errorf("second attach signalled %+v, want [%s %s]", call, id, cwd)
		}
	case <-time.After(loadCwdWait):
		t.Fatal("the second attach issued no session/load: the failed load was memoized, so re-attaching " +
			"the same session shows an empty screen and never re-raises the prompt")
	}

	// The retry must not have orphaned the session's broker: Close still reaps
	// it, so this subscription observes a clean close rather than hanging for
	// the process's life (the failure mode of "just drop the map entry").
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case _, ok := <-second.C:
		if ok {
			// An event is fine; the channel closing is what matters, and a
			// pending event is drained before it.
			select {
			case <-second.C:
			case <-time.After(loadCwdWait):
				t.Fatal("the retried session's broker was never closed — its subscription hangs forever")
			}
		}
	case <-time.After(loadCwdWait):
		t.Fatal("the retried session's broker was never closed — its subscription hangs forever")
	}
	second.Close()
}

// TestLoadDiscardsOtherLoadErrors pins how NARROW the routing is. A load that
// fails for any other reason — here an id the daemon has never heard of — is
// still discarded exactly as it always was (jedwards1230/gofer#325 is
// deliberately untouched), so the sink stays a signal a consumer can act on
// rather than a general error channel it has to triage.
func TestLoadDiscardsOtherLoadErrors(t *testing.T) {
	url, _ := newLoadCwdDaemon(t, t.TempDir())

	r, calls := dialCoreWithCwdSink(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), loadCwdWait)
	defer cancel()
	if err := r.Load(ctx, "0192a1b2-0000-7000-8000-000000000042"); err != nil {
		t.Fatalf("Load of an unknown session: %v", err)
	}

	select {
	case call := <-calls:
		t.Errorf("cwd-missing sink fired for an unrelated load failure: %+v", call)
	default:
	}
}
