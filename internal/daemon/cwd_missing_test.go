package daemon_test

// cwd_missing_test.go covers session/load against a session whose RECORDED
// working directory has been deleted, end-to-end over the real wire
// (jedwards1230/gofer#326).
//
// The daemon used to answer that case by resolving the load into its OWN working
// directory. The resume reported success, so nothing on screen said the session
// had been reopened somewhere else entirely — while every cwd-scoped input
// (project config, <cwd>/.gofer/commands, skills, file resolution) now came from
// a directory the user never chose. It must instead answer with a typed,
// operand-carrying error the client can turn into "this directory is gone; where
// should I reopen the session?".
//
// These are integration tests rather than direct resolveLoadCwd calls
// (resume_cwd_internal_test.go covers that layer) because the properties that
// matter here are not the return value: whether the daemon RESUMED anything,
// whether it kept any state pending, and whether the journal moved.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// codeSessionCwdMissing mirrors internal/daemon's unexported constant of the
// same name. It is redeclared (rather than exported for tests) because it IS the
// daemon's public wire contract — the number a client branches on — so a test in
// the external package asserting the literal is asserting exactly what a client
// would see.
const codeSessionCwdMissing = -32001

// deletedCwdFixture is an offline session whose recorded working directory no
// longer exists, served by a live daemon: the state an operator reaches by
// deleting a project directory and then attaching to a session that ran there.
type deletedCwdFixture struct {
	sup *supervisor.Supervisor
	d   *daemon.Daemon
	url string
	// sid is the offline session's id.
	sid string
	// gone is the directory the session's journal records — deleted from disk.
	gone string
	// journal is sid's journal path, for reading its meta entry back directly.
	journal string
}

// newDeletedCwdFixture creates a session in a real directory through a first
// daemon, tears that daemon and supervisor down (a restart keeps nothing in
// memory), deletes the directory, and brings a SECOND daemon up over the same
// store root. The session is then offline, with a recorded cwd that no longer
// exists — reached the way it happens in production rather than by hand-writing
// a journal.
func newDeletedCwdFixture(t *testing.T) deletedCwdFixture {
	t.Helper()
	root := t.TempDir()
	cwd := t.TempDir()

	sup1 := newTestSupervisorAtRoot(t, root, fauxProvider)
	d1 := daemon.New(sup1, daemon.Config{DefaultModel: "faux"})
	srv1 := httptest.NewServer(d1.Handler())
	c1 := dial(t, context.Background(), "ws"+srv1.URL[len("http"):], nil)
	sid := newSession(t, c1, cwd)
	srv1.Close()
	if err := sup1.Close(); err != nil {
		t.Fatalf("sup1.Close: %v", err)
	}

	if err := os.RemoveAll(cwd); err != nil {
		t.Fatalf("delete the session's cwd: %v", err)
	}

	sup2 := newTestSupervisorAtRoot(t, root, fauxProvider)
	d2, url := newTestDaemon(t, sup2, "")

	info := findInfoT(t, mustList(t, sup2), sid)
	if info.Live {
		t.Fatalf("precondition: session %s should be offline on the second daemon", sid)
	}
	if info.Cwd != cwd {
		t.Fatalf("precondition: journal cwd = %q, want the deleted directory %q", info.Cwd, cwd)
	}
	return deletedCwdFixture{sup: sup2, d: d2, url: url, sid: sid, gone: cwd, journal: info.JournalPath}
}

// mustList is supervisor.List with the error folded into a test failure.
func mustList(t *testing.T, sup *supervisor.Supervisor) []supervisor.SessionInfo {
	t.Helper()
	infos, err := sup.List(context.Background())
	if err != nil {
		t.Fatalf("supervisor.List: %v", err)
	}
	return infos
}

// journalCwd reads a session's RECORDED working directory straight out of its
// journal's root meta entry — the durable value, not the roster's view of it.
// [session.MetaPayload]'s Cwd is written exactly once, by runner.New at creation
// (runner.Resume opens an existing journal and never appends a meta entry), so
// this is the assertion that a resume — at any cwd — moved nothing on disk.
func journalCwd(t *testing.T, path string) string {
	t.Helper()
	entries, err := session.ReadEntries(path)
	if err != nil {
		t.Fatalf("read journal %s: %v", path, err)
	}
	if len(entries) == 0 || entries[0].Type != session.EntryMeta {
		t.Fatalf("journal %s has no root meta entry", path)
	}
	meta, err := entries[0].Meta()
	if err != nil {
		t.Fatalf("decode journal meta: %v", err)
	}
	return meta.Cwd
}

// liveIDs returns the ids on the daemon's LIVE roster — gofer/roster, not
// gofer/ps: ps lists every session on disk (offline ones with Live=false), so a
// presence check against it would pass no matter what the load did.
func liveIDs(t *testing.T, c *wsClient) map[string]bool {
	t.Helper()
	resp := c.request("gofer/roster", nil)
	if resp.Error != nil {
		t.Fatalf("gofer/roster error: %+v", resp.Error)
	}
	var roster []sessionInfoWire
	if err := json.Unmarshal(resp.Result, &roster); err != nil {
		t.Fatalf("unmarshal roster: %v", err)
	}
	out := make(map[string]bool, len(roster))
	for _, row := range roster {
		if row.Live {
			out[row.ID] = true
		}
	}
	return out
}

// loadMissingCwd sends a blank-cwd session/load for fx.sid and asserts the reply
// is the typed cwd-missing error carrying the recorded directory as structured
// data. It returns nothing: every caller asserts on the SIDE EFFECTS instead.
func loadMissingCwd(t *testing.T, fx deletedCwdFixture, c *wsClient) {
	t.Helper()
	resp := c.request(acp.MethodSessionLoad, acp.LoadSessionRequest{SessionID: fx.sid})
	if resp.Error == nil {
		t.Fatalf("session/load with a deleted recorded cwd succeeded — the daemon substituted a directory")
	}
	if resp.Error.Code != codeSessionCwdMissing {
		t.Fatalf("error code = %d (%s), want %d (session cwd missing)", resp.Error.Code, resp.Error.Message, codeSessionCwdMissing)
	}
	var data struct {
		Cwd string `json:"cwd"`
	}
	if err := json.Unmarshal(resp.Error.Data, &data); err != nil {
		t.Fatalf("decode error data %q: %v", resp.Error.Data, err)
	}
	if data.Cwd != fx.gone {
		t.Errorf("error data cwd = %q, want the recorded directory %q", data.Cwd, fx.gone)
	}
}

// TestSessionLoadRecordedCwdMissingIsTypedAndResumesNothing is the primary
// assertion: attaching to a session whose recorded directory is gone answers
// with the typed signal AND leaves the session exactly where it was — not live,
// no peer attached, nothing replayed.
func TestSessionLoadRecordedCwdMissingIsTypedAndResumesNothing(t *testing.T) {
	fx := newDeletedCwdFixture(t)
	c := dial(t, context.Background(), fx.url, nil)

	loadMissingCwd(t, fx, c)

	if live := liveIDs(t, c); live[fx.sid] {
		t.Errorf("session %s is live after a failed load — the daemon resumed it anyway", fx.sid)
	}
	if n := fx.d.PeersForSessionCount(fx.sid); n != 0 {
		t.Errorf("%d peers attached to %s after a failed load, want 0", n, fx.sid)
	}
	// A load that got as far as replaying would have written its history
	// notifications to this connection BEFORE the response we already read (see
	// handleSessionLoad), so anything buffered here is a partially-run load.
	select {
	case n := <-c.notifications:
		t.Errorf("a failed load pushed %s to the client, want no replay at all", n.Method)
	default:
	}
}

// TestSessionLoadLiveSessionWithDeletedCwdStillAttaches is the branch that has
// no prompt at all, and must not grow one.
//
// A session RUNNING in a directory the user then deletes (rm -rf on a worktree)
// is reached the same way as any other attach: a blank-cwd session/load. But the
// supervisor's Resume returns the existing snapshot for a live id without
// building a second runner, so the cwd it is handed is ignored outright. Two
// things follow, and both are asserted here: attaching must simply WORK (the
// session is running; watching it is exactly what the user wants at that
// moment), and it must not raise the typed signal — because the prompt that
// signal opens would offer a re-init whose chosen directory that same early
// return discards, while the UI states the session was reopened there. A
// directory that is not where the session runs must never be stated.
func TestSessionLoadLiveSessionWithDeletedCwdStillAttaches(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	d, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	cwd := t.TempDir()
	sid := newSession(t, c, cwd)
	if err := os.RemoveAll(cwd); err != nil {
		t.Fatalf("delete the live session's cwd: %v", err)
	}

	if resp := c.request(acp.MethodSessionLoad, acp.LoadSessionRequest{SessionID: sid}); resp.Error != nil {
		t.Fatalf("session/load of a LIVE session whose cwd was deleted: %+v — it is already running, so there is "+
			"no directory to decide and nothing to refuse", resp.Error)
	}
	if live := liveIDs(t, c); !live[sid] {
		t.Errorf("session %s left the live roster after attaching to it", sid)
	}
	if n := d.PeersForSessionCount(sid); n != 1 {
		t.Errorf("%d peers attached to %s after a successful load, want 1", n, sid)
	}
	// And its recorded directory is untouched — the load resolved nothing, so
	// there was nothing to write anywhere.
	if got := findInfoT(t, mustList(t, sup), sid).Cwd; got != cwd {
		t.Errorf("live session cwd = %q, want the unchanged %q", got, cwd)
	}
}

// TestSessionLoadCwdMissingClientDisconnectChangesNothing is the "cancel is the
// default on every dismissal path" proof at the only layer that can carry it.
// The client-side prompt this error drives is local TUI state, so the guarantee
// is a NEGATIVE one about the daemon: having answered the typed error it holds
// nothing pending, and a client that dies with its prompt open therefore mutates
// nothing by dying.
//
// The disconnect is made OBSERVABLE rather than assumed: the same connection
// also attaches to a healthy session, so the test can wait for the daemon to
// deregister that peer and know the close was processed before asserting the
// deleted-cwd session is untouched. Without that, closing and immediately
// re-asserting would pass even if the daemon never noticed.
func TestSessionLoadCwdMissingClientDisconnectChangesNothing(t *testing.T) {
	fx := newDeletedCwdFixture(t)
	ctx := context.Background()
	c1 := dial(t, ctx, fx.url, nil)

	loadMissingCwd(t, fx, c1)

	// A second, healthy session on the SAME connection, loaded successfully, so
	// this connection holds a real registration whose removal marks the
	// disconnect as processed.
	healthy := newSession(t, c1, t.TempDir())
	if resp := c1.request(acp.MethodSessionLoad, acp.LoadSessionRequest{SessionID: healthy}); resp.Error != nil {
		t.Fatalf("session/load (healthy session): %+v", resp.Error)
	}
	if n := fx.d.PeersForSessionCount(healthy); n != 1 {
		t.Fatalf("healthy session has %d attached peers, want 1 — this test's disconnect probe is stale", n)
	}

	c1.close()

	deadline := time.Now().Add(defaultWait)
	for fx.d.PeersForSessionCount(healthy) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("daemon never deregistered the closed connection's peer")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The disconnect is processed. Nothing about the cwd-missing session moved.
	c2 := dial(t, ctx, fx.url, nil)
	if live := liveIDs(t, c2); live[fx.sid] {
		t.Errorf("session %s went live across the disconnect", fx.sid)
	}
	if n := fx.d.PeersForSessionCount(fx.sid); n != 0 {
		t.Errorf("%d peers attached to %s after the disconnect, want 0", n, fx.sid)
	}
	if got := journalCwd(t, fx.journal); got != fx.gone {
		t.Errorf("journal cwd = %q, want the original recorded %q — the disconnect rewrote it", got, fx.gone)
	}
	// The daemon memoized nothing: the identical request answers identically on
	// a fresh connection, rather than having consumed a one-shot state.
	loadMissingCwd(t, fx, c2)

	// And the session is not wedged: an explicit directory still resumes it.
	fresh := t.TempDir()
	if resp := c2.request(acp.MethodSessionLoad, acp.LoadSessionRequest{SessionID: fx.sid, Cwd: fresh}); resp.Error != nil {
		t.Fatalf("session/load with an explicit cwd after the disconnect: %+v", resp.Error)
	}
	if live := liveIDs(t, c2); !live[fx.sid] {
		t.Errorf("session %s did not resume at the explicitly chosen %q", fx.sid, fresh)
	}
}

// TestSessionLoadCwdBranchesLeaveTheJournalUnchanged pins the constraint every
// branch of this feature rests on: the journal is never rewritten, so no branch —
// including a successful re-init in a DIFFERENT directory — can lose where the
// session was originally recorded.
//
// The mechanism is [session.MetaPayload]'s Cwd being written exactly once by
// runner.New; the point of asserting it is that the re-init path deliberately
// adds no persistence, so a future change that "helpfully" records the new
// directory would silently make the original unrecoverable.
func TestSessionLoadCwdBranchesLeaveTheJournalUnchanged(t *testing.T) {
	fx := newDeletedCwdFixture(t)
	c := dial(t, context.Background(), fx.url, nil)

	if got := journalCwd(t, fx.journal); got != fx.gone {
		t.Fatalf("precondition: journal cwd = %q, want %q", got, fx.gone)
	}

	// Branch 1: blank cwd, recorded directory gone — the typed refusal.
	loadMissingCwd(t, fx, c)
	if got := journalCwd(t, fx.journal); got != fx.gone {
		t.Errorf("after the typed refusal, journal cwd = %q, want %q", got, fx.gone)
	}

	// Branch 2: an explicit directory that does not exist — the -32602 refusal.
	bad := filepath.Join(t.TempDir(), "never-existed")
	if resp := c.request(acp.MethodSessionLoad, acp.LoadSessionRequest{SessionID: fx.sid, Cwd: bad}); resp.Error == nil {
		t.Fatal("session/load with an explicit nonexistent cwd succeeded, want -32602")
	} else if resp.Error.Code != -32602 {
		t.Errorf("explicit bad cwd: code = %d, want -32602", resp.Error.Code)
	}
	if got := journalCwd(t, fx.journal); got != fx.gone {
		t.Errorf("after the invalid-params refusal, journal cwd = %q, want %q", got, fx.gone)
	}

	// Branch 3: the re-init the prompt drives — an explicit, existing directory.
	// It changes where the session RUNS and nothing else.
	rebased := t.TempDir()
	if resp := c.request(acp.MethodSessionLoad, acp.LoadSessionRequest{SessionID: fx.sid, Cwd: rebased}); resp.Error != nil {
		t.Fatalf("session/load with the re-init cwd: %+v", resp.Error)
	}
	if got := findInfoT(t, mustList(t, fx.sup), fx.sid).Cwd; got != rebased {
		t.Errorf("live roster cwd = %q, want the re-init directory %q", got, rebased)
	}
	if got := journalCwd(t, fx.journal); got != fx.gone {
		t.Errorf("after a successful re-init at %q, journal cwd = %q, want the ORIGINAL %q — the re-init rewrote the journal",
			rebased, got, fx.gone)
	}
}
