package wirestream_test

// load_no_precall_rpc_test.go is the RPC-count regression pinned by
// jedwards1230/gofer#317. The issue described loadHistory issuing two
// blocking RPCs — gofer/roster then gofer/overview — to resolve a session's
// cwd before every session/load. That bug is already gone on main, fixed as
// an unlabeled side effect of PR #349 (9c4b8c2): loadHistory now sends a
// BLANK cwd and lets the daemon resolve it server-side (see
// reconstruct.go's loadHistory doc). load_cwd_test.go already covers the
// resulting wire *behavior* (blank cwd, the cwd-missing signal, retry on a
// second reference) — this file adds the one assertion #317's definition of
// done actually asked for and that suite doesn't make: that attaching never
// issues gofer/roster or gofer/overview at all.
//
// The assertion is log-line-based, not wall-clock or call-count-on-a-mock.
// Every wirestream test in this package dials a REAL daemon over a real
// websocket — daemon.Client is a concrete struct, not an interface, so there
// is no fake to instrument with a call counter. The daemon's own structured
// per-request log (internal/daemon/peer.go's handleFrame, `method=<name>
// outcome=<ok|err>`) is therefore the deterministic proxy for "was this RPC
// issued": it is written synchronously for every inbound frame, high-frequency
// reads included (demoted to DEBUG by isHighFrequencyRead, which is exactly
// why this test dials the daemon at slog.LevelDebug — an INFO-level buffer
// would silently miss an ok gofer/roster call and pass vacuously).
import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/provider/faux"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// syncLogBuf is a concurrency-safe bytes.Buffer, the same shape as
// internal/daemon/logging_test.go's syncBuffer: slog serializes its own
// Write calls, but this test also reads the buffer's contents from the test
// goroutine while a request-handler goroutine may still be writing.
type syncLogBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncLogBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncLogBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newLoadCwdDaemonWithDebugLog is newLoadCwdDaemon (load_cwd_test.go) plus a
// DEBUG-level slog.Logger wired to a concurrency-safe buffer, following
// internal/daemon/logging_test.go's newLoggingTestDaemon pattern: the
// daemon logs handleFrame's per-request outcome line from a handler
// goroutine while this test reads the buffer back from the test goroutine,
// so a bare bytes.Buffer would race.
func newLoadCwdDaemonWithDebugLog(t *testing.T, root string) (string, *supervisor.Supervisor, *syncLogBuf) {
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

	buf := &syncLogBuf{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d := daemon.New(sup, daemon.Config{DefaultModel: "faux", Logger: logger})
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	return "ws" + srv.URL[len("http"):], sup, buf
}

// TestLoadOfOfflineSessionIssuesNoRosterOrOverviewCall is the RPC-count half
// of jedwards1230/gofer#317's definition of done. Before PR #349, attaching
// to an offline session — the exact scenario here, via seedOfflineJournal —
// unconditionally issued gofer/roster then gofer/overview before
// session/load, to resolve a cwd to echo back. Those two RPCs are gone on
// current main; this test proves it and keeps it proven by asserting the
// daemon's own request log carries neither method name anywhere in the
// attach, and — so the test cannot pass vacuously because the load itself
// silently failed — that the session actually came up in its recorded
// directory (mirroring TestLoadSendsBlankCwdAndReopensWhereRecorded).
func TestLoadOfOfflineSessionIssuesNoRosterOrOverviewCall(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	id := seedOfflineJournal(t, root, cwd, "investigate the flaky build")
	url, sup, buf := newLoadCwdDaemonWithDebugLog(t, root)

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

	logs := buf.String()
	rosterCalls := strings.Count(logs, "method=gofer/roster")
	overviewCalls := strings.Count(logs, "method=gofer/overview")
	if rosterCalls != 0 || overviewCalls != 0 {
		t.Errorf("attaching to an offline session issued gofer/roster=%d gofer/overview=%d calls, want 0 of each "+
			"(jedwards1230/gofer#317 — loadHistory must send a blank cwd, not resolve one via roster/overview "+
			"first):\n%s", rosterCalls, overviewCalls, logs)
	}
}
