package daemon_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/acp"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// TestSessionListSurvivesDaemonRestart is the user's repro: a session's cwd,
// title, and updated time must survive a daemon restart, not just live in
// the in-memory roster.
//
// It drives a session/new + session/prompt against a first daemon/supervisor
// pair, closes both (a daemon restart never keeps the old process' roster —
// see [supervisor.Supervisor.Close]), then builds a second daemon/supervisor
// pair over the exact same on-disk root ([newTestSupervisorAtRoot]) and
// re-runs session/list. Before the [supervisor.Supervisor.List] enrichment
// fix, the disk-only SessionInfo this second daemon rediscovers the session
// as carried a zero-value Cwd — so a cwd-filtered session/list silently
// excluded it, and even the unfiltered list showed a bare id with no title.
func TestSessionListSurvivesDaemonRestart(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()

	sup1 := newTestSupervisorAtRoot(t, root, fauxProvider)
	d1 := daemon.New(sup1, daemon.Config{DefaultModel: "faux"})
	srv1 := httptest.NewServer(d1.Handler())
	c1 := dial(t, context.Background(), "ws"+srv1.URL[len("http"):], nil)

	sid := newSession(t, c1, cwd)
	promptResp := c1.request(acp.MethodSessionPrompt, acp.PromptRequest{
		SessionID: sid,
		Prompt:    []acp.ContentBlock{acp.TextBlock("investigate the flaky build")},
	})
	if promptResp.Error != nil {
		t.Fatalf("session/prompt error: %+v", promptResp.Error)
	}

	// Simulate a daemon restart: tear down everything bound to the first
	// supervisor (its WebSocket server, then the supervisor itself, which
	// kills the live session and closes its journal) before rebuilding fresh
	// instances over the same root. No in-memory state carries over.
	srv1.Close()
	if err := sup1.Close(); err != nil {
		t.Fatalf("sup1.Close: %v", err)
	}

	sup2 := newTestSupervisorAtRoot(t, root, fauxProvider)
	ctx := context.Background()

	// supervisor.List directly: the session must come back disk-only, with
	// Cwd/Title/Updated read from its journal.
	infos, err := sup2.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := findInfoT(t, infos, sid)
	if got.Live {
		t.Errorf("Live = true, want false (not yet resumed by the new process)")
	}
	if got.Cwd != cwd {
		t.Errorf("Cwd = %q, want %q", got.Cwd, cwd)
	}
	if got.Title == "" {
		t.Error("Title is empty, want the journaled prompt's snippet")
	}
	if got.Updated.IsZero() {
		t.Error("Updated is zero, want the last journal entry's time")
	}

	// Through the daemon: a cwd-filtered session/list must now return the
	// session (before the fix, every disk-only entry's Cwd was "" and never
	// matched a real filter).
	d2 := daemon.New(sup2, daemon.Config{DefaultModel: "faux"})
	srv2 := httptest.NewServer(d2.Handler())
	t.Cleanup(srv2.Close)
	c2 := dial(t, ctx, "ws"+srv2.URL[len("http"):], nil)

	filtered := c2.request(acp.MethodSessionList, acp.ListSessionsRequest{Cwd: cwd})
	if filtered.Error != nil {
		t.Fatalf("session/list (filtered) error: %+v", filtered.Error)
	}
	var filteredResp acp.ListSessionsResponse
	if err := json.Unmarshal(filtered.Result, &filteredResp); err != nil {
		t.Fatalf("unmarshal filtered response: %v", err)
	}
	wireGot := findWireT(t, filteredResp.Sessions, sid)
	if wireGot.Cwd != cwd {
		t.Errorf("filtered session/list Cwd = %q, want %q", wireGot.Cwd, cwd)
	}
	if wireGot.Title == "" {
		t.Error("filtered session/list Title is empty, want the journaled prompt's snippet")
	}
	if wireGot.UpdatedAt == "" {
		t.Error("filtered session/list UpdatedAt is empty, want a populated RFC3339 timestamp")
	}

	// Unfiltered session/list must also surface the same Cwd/Title/UpdatedAt.
	unfiltered := c2.request(acp.MethodSessionList, acp.ListSessionsRequest{})
	if unfiltered.Error != nil {
		t.Fatalf("session/list (unfiltered) error: %+v", unfiltered.Error)
	}
	var unfilteredResp acp.ListSessionsResponse
	if err := json.Unmarshal(unfiltered.Result, &unfilteredResp); err != nil {
		t.Fatalf("unmarshal unfiltered response: %v", err)
	}
	wireGot = findWireT(t, unfilteredResp.Sessions, sid)
	if wireGot.Cwd != cwd {
		t.Errorf("unfiltered session/list Cwd = %q, want %q", wireGot.Cwd, cwd)
	}
	if wireGot.Title == "" {
		t.Error("unfiltered session/list Title is empty, want the journaled prompt's snippet")
	}
}

// findInfoT returns the [supervisor.SessionInfo] for id in infos, failing the
// test if it is absent.
func findInfoT(t *testing.T, infos []supervisor.SessionInfo, id string) supervisor.SessionInfo {
	t.Helper()
	for _, info := range infos {
		if info.ID == id {
			return info
		}
	}
	t.Fatalf("List missing session %s: %+v", id, infos)
	return supervisor.SessionInfo{}
}

// findWireT returns the [acp.SessionInfo] for id in sessions, failing the
// test if it is absent.
func findWireT(t *testing.T, sessions []acp.SessionInfo, id string) acp.SessionInfo {
	t.Helper()
	for _, s := range sessions {
		if s.SessionID == id {
			return s
		}
	}
	t.Fatalf("session/list missing session %s: %+v", id, sessions)
	return acp.SessionInfo{}
}
