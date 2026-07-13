package daemon_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/acp"
)

// TestSessionNewCwdResolution covers session/new's cwd handling end-to-end
// (see handlers.go's resolveSessionCwd): tilde expansion, the empty-cwd
// default, and the reject cases (relative, nonexistent, not-a-directory) that
// previously created a session whose every tool call silently failed — the
// live bug this guards against (an ACP client sending the literal,
// unexpanded string "~/orchestration" as cwd).
func TestSessionNewCwdResolution(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	t.Run("tilde-slash expands under the daemon home", func(t *testing.T) {
		sub := filepath.Join(fakeHome, "myproject")
		if err := os.Mkdir(sub, 0o755); err != nil {
			t.Fatalf("Mkdir: %v", err)
		}
		resp := c.request(acp.MethodSessionNew, acp.NewSessionRequest{Cwd: "~/myproject"})
		if resp.Error != nil {
			t.Fatalf("session/new error: %+v", resp.Error)
		}
		sid := decodeSessionID(t, resp)

		psResp := c.request("gofer/ps", nil)
		if psResp.Error != nil {
			t.Fatalf("gofer/ps error: %+v", psResp.Error)
		}
		listResp := c.request(acp.MethodSessionList, acp.ListSessionsRequest{})
		if listResp.Error != nil {
			t.Fatalf("session/list error: %+v", listResp.Error)
		}
		var got acp.ListSessionsResponse
		if err := json.Unmarshal(listResp.Result, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		var found bool
		for _, s := range got.Sessions {
			if s.SessionID == sid {
				found = true
				if s.Cwd != sub {
					t.Errorf("cwd = %q, want expanded %q", s.Cwd, sub)
				}
			}
		}
		if !found {
			t.Fatalf("session %s missing from session/list: %+v", sid, got.Sessions)
		}
	})

	t.Run("bare tilde expands to the daemon home", func(t *testing.T) {
		resp := c.request(acp.MethodSessionNew, acp.NewSessionRequest{Cwd: "~"})
		if resp.Error != nil {
			t.Fatalf("session/new error: %+v", resp.Error)
		}
		sid := decodeSessionID(t, resp)
		if sid == "" {
			t.Fatal("session/new: empty sessionId")
		}
	})

	t.Run("empty cwd defaults to the daemon's working directory", func(t *testing.T) {
		wantCwd, err := os.Getwd()
		if err != nil {
			t.Fatalf("os.Getwd: %v", err)
		}
		resp := c.request(acp.MethodSessionNew, acp.NewSessionRequest{Cwd: ""})
		if resp.Error != nil {
			t.Fatalf("session/new error: %+v", resp.Error)
		}
		sid := decodeSessionID(t, resp)

		listResp := c.request(acp.MethodSessionList, acp.ListSessionsRequest{})
		if listResp.Error != nil {
			t.Fatalf("session/list error: %+v", listResp.Error)
		}
		var got acp.ListSessionsResponse
		if err := json.Unmarshal(listResp.Result, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		var found bool
		for _, s := range got.Sessions {
			if s.SessionID == sid {
				found = true
				if s.Cwd != wantCwd {
					t.Errorf("cwd = %q, want daemon working directory %q", s.Cwd, wantCwd)
				}
			}
		}
		if !found {
			t.Fatalf("session %s missing from session/list: %+v", sid, got.Sessions)
		}
	})

	t.Run("relative path is rejected", func(t *testing.T) {
		before := psCount(t, c)
		resp := c.request(acp.MethodSessionNew, acp.NewSessionRequest{Cwd: "orchestration"})
		if resp.Error == nil {
			t.Fatal("session/new with relative cwd: want an error, got none")
		}
		if resp.Error.Code != -32602 {
			t.Errorf("error code = %d, want -32602 (invalid params)", resp.Error.Code)
		}
		if psCount(t, c) != before {
			t.Errorf("ps count changed after rejected session/new: want no new session created")
		}
	})

	t.Run("nonexistent literal-tilde path is rejected (the live bug)", func(t *testing.T) {
		// The exact shape of the live bug: an ACP client sends the literal,
		// unexpanded string "~/orchestration" and $HOME/orchestration does not
		// exist on this daemon.
		before := psCount(t, c)
		resp := c.request(acp.MethodSessionNew, acp.NewSessionRequest{Cwd: "~/orchestration"})
		if resp.Error == nil {
			t.Fatal("session/new with nonexistent cwd: want an error, got none")
		}
		if resp.Error.Code != -32602 {
			t.Errorf("error code = %d, want -32602 (invalid params)", resp.Error.Code)
		}
		if psCount(t, c) != before {
			t.Errorf("ps count changed after rejected session/new: want no new session created")
		}
	})

	t.Run("file (not directory) path is rejected", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "not-a-dir")
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		before := psCount(t, c)
		resp := c.request(acp.MethodSessionNew, acp.NewSessionRequest{Cwd: file})
		if resp.Error == nil {
			t.Fatal("session/new with file cwd: want an error, got none")
		}
		if resp.Error.Code != -32602 {
			t.Errorf("error code = %d, want -32602 (invalid params)", resp.Error.Code)
		}
		if psCount(t, c) != before {
			t.Errorf("ps count changed after rejected session/new: want no new session created")
		}
	})
}

// TestSessionListCwdFilterMatchesTildeAndResolvedForms is the regression test
// for the session/list cwd filter bug: sessions are stored with an absolute,
// tilde-expanded cwd (see resolveSessionCwd), but the filter used to compare
// req.Cwd against it RAW, so a client filtering by the same "~/<sub>" form it
// created the session with (the natural phone-client flow) could never match
// — the list came back empty for a live session. Both the tilde form and the
// already-resolved absolute form must match; a relative or nonexistent filter
// must match nothing, without erroring.
func TestSessionListCwdFilterMatchesTildeAndResolvedForms(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)

	sub := filepath.Join(fakeHome, "myproject")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	sid := newSession(t, c, "~/myproject")

	t.Run("tilde form matches", func(t *testing.T) {
		got := listSessionIDs(t, c, "~/myproject")
		if !containsID(got, sid) {
			t.Errorf("session/list cwd=%q: got %v, want it to include %s", "~/myproject", got, sid)
		}
	})

	t.Run("resolved absolute form matches", func(t *testing.T) {
		got := listSessionIDs(t, c, sub)
		if !containsID(got, sid) {
			t.Errorf("session/list cwd=%q: got %v, want it to include %s", sub, got, sid)
		}
	})

	t.Run("relative filter matches nothing, no error", func(t *testing.T) {
		resp := c.request(acp.MethodSessionList, acp.ListSessionsRequest{Cwd: "myproject"})
		if resp.Error != nil {
			t.Fatalf("session/list with relative cwd filter: unexpected error %+v", resp.Error)
		}
		got := listSessionIDs(t, c, "myproject")
		if containsID(got, sid) {
			t.Errorf("session/list cwd=%q: got %v, want it to NOT include %s", "myproject", got, sid)
		}
	})

	t.Run("nonexistent filter matches nothing, no error", func(t *testing.T) {
		resp := c.request(acp.MethodSessionList, acp.ListSessionsRequest{Cwd: filepath.Join(fakeHome, "does-not-exist")})
		if resp.Error != nil {
			t.Fatalf("session/list with nonexistent cwd filter: unexpected error %+v", resp.Error)
		}
		got := listSessionIDs(t, c, filepath.Join(fakeHome, "does-not-exist"))
		if containsID(got, sid) {
			t.Errorf("session/list cwd=%q: got %v, want it to NOT include %s", "does-not-exist", got, sid)
		}
	})
}

// listSessionIDs issues session/list filtered by cwd and returns the ids of
// every returned session.
func listSessionIDs(t *testing.T, c *wsClient, cwd string) []string {
	t.Helper()
	resp := c.request(acp.MethodSessionList, acp.ListSessionsRequest{Cwd: cwd})
	if resp.Error != nil {
		t.Fatalf("session/list error: %+v", resp.Error)
	}
	var got acp.ListSessionsResponse
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ids := make([]string, 0, len(got.Sessions))
	for _, s := range got.Sessions {
		ids = append(ids, s.SessionID)
	}
	return ids
}

func containsID(ids []string, id string) bool {
	for _, got := range ids {
		if got == id {
			return true
		}
	}
	return false
}

// TestSessionLoadCwdResolution asserts session/load rejects a bad cwd the
// same way session/new does (see resolveSessionCwd), rather than resuming a
// session into a nonexistent working directory.
func TestSessionLoadCwdResolution(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	sid := newSession(t, c, t.TempDir())

	resp := c.request(acp.MethodSessionLoad, acp.LoadSessionRequest{SessionID: sid, Cwd: "relative/path"})
	if resp.Error == nil {
		t.Fatal("session/load with relative cwd: want an error, got none")
	}
	if resp.Error.Code != -32602 {
		t.Errorf("error code = %d, want -32602 (invalid params)", resp.Error.Code)
	}
}

// decodeSessionID unmarshals resp as a NewSessionResponse and returns its id.
func decodeSessionID(t *testing.T, resp rpcFrame) string {
	t.Helper()
	var sess acp.NewSessionResponse
	if err := json.Unmarshal(resp.Result, &sess); err != nil {
		t.Fatalf("unmarshal NewSessionResponse: %v", err)
	}
	if sess.SessionID == "" {
		t.Fatal("session/new: empty sessionId")
	}
	return sess.SessionID
}

// psCount returns the current gofer/ps entry count, used to assert a
// rejected session/new never reached supervisor.Create.
func psCount(t *testing.T, c *wsClient) int {
	t.Helper()
	resp := c.request("gofer/ps", nil)
	if resp.Error != nil {
		t.Fatalf("gofer/ps error: %+v", resp.Error)
	}
	var ps []sessionInfoWire
	if err := json.Unmarshal(resp.Result, &ps); err != nil {
		t.Fatalf("unmarshal ps: %v", err)
	}
	return len(ps)
}
