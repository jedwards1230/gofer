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

// TestSessionListFleetGlobalVisibility asserts session/list is fleet-global
// (M3 fan-out): a shared daemon several clients attach to must surface EVERY
// session regardless of its cwd, so a laptop's roster shows a session a phone
// created in a different directory. It creates sessions across two distinct
// cwds and asserts both are returned — with their own Cwd metadata intact —
// whether the request carries no cwd, one session's cwd, or a cwd matching
// none of them. req.Cwd is a label, never a hiding filter. (This replaces the
// former cwd-filter regression test: the filter it guarded was removed because
// it made a shared daemon's sessions invisible to a client in another
// directory.)
func TestSessionListFleetGlobalVisibility(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	cwdA := t.TempDir()
	cwdB := t.TempDir()
	sidA := newSession(t, c, cwdA)
	sidB := newSession(t, c, cwdB)

	// cwdByID lists sessions with the given req.Cwd and returns id->cwd for
	// every returned session, so a caller can assert both presence and the
	// per-session cwd metadata in one shot.
	cwdByID := func(t *testing.T, reqCwd string) map[string]string {
		t.Helper()
		resp := c.request(acp.MethodSessionList, acp.ListSessionsRequest{Cwd: reqCwd})
		if resp.Error != nil {
			t.Fatalf("session/list req.Cwd=%q: unexpected error %+v", reqCwd, resp.Error)
		}
		var got acp.ListSessionsResponse
		if err := json.Unmarshal(resp.Result, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		out := make(map[string]string, len(got.Sessions))
		for _, s := range got.Sessions {
			out[s.SessionID] = s.Cwd
		}
		return out
	}

	// Both sessions are returned regardless of req.Cwd, each carrying its own
	// cwd — never the requested one.
	for _, reqCwd := range []string{"", cwdA, filepath.Join(t.TempDir(), "unrelated")} {
		got := cwdByID(t, reqCwd)
		if got[sidA] != cwdA {
			t.Errorf("req.Cwd=%q: sidA cwd = %q, want %q", reqCwd, got[sidA], cwdA)
		}
		if got[sidB] != cwdB {
			t.Errorf("req.Cwd=%q: sidB cwd = %q, want %q", reqCwd, got[sidB], cwdB)
		}
	}
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
