package daemon_test

// session_new_meta_test.go covers the daemon half of issue #162's defect 2:
// session/new must tell the client which model it ASSIGNED, not leave the
// client guessing from what it asked for.
//
// ACP's own NewSessionResponse carries only the session id, so the assigned
// model rides in the response's `_meta` — ACP's reserved extension point for
// implementation-specific data — under the gofer-namespaced key "gofer/model".
// Asserted against the RAW JSON rather than a Go struct so the wire key itself
// is pinned: a client (internal/daemonbridge, and any other ACP client) decodes
// that exact key, and renaming it silently would break them all.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/acp"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// TestSessionNewResponseCarriesAssignedModel pins both resolution paths onto
// the wire: the daemon's own default when the client names no model (the
// normal path, and the one that used to be unanswerable), and the client's
// model when it does. The two differ, so neither assertion can pass by
// accident.
//
// It also pins that the response stays a superset of ACP's — sessionId is
// still there, at the top level, where an unaware ACP client reads it.
func TestSessionNewResponseCarriesAssignedModel(t *testing.T) {
	const defaultModel = "claude-sonnet-4-5"
	const requestedModel = "claude-haiku-4-5"

	sup := newTestSupervisor(t, fauxProvider)
	d := daemon.New(sup, daemon.Config{DefaultModel: defaultModel})
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	c := dial(t, context.Background(), "ws"+srv.URL[len("http"):], nil)

	tests := []struct {
		name      string
		reqModel  string
		wantModel string
	}{
		{"no model requested reports the daemon default", "", defaultModel},
		{"a requested model is reported back", requestedModel, requestedModel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := c.request(acp.MethodSessionNew, acp.NewSessionRequest{Cwd: t.TempDir(), Model: tt.reqModel})
			if resp.Error != nil {
				t.Fatalf("session/new error: %+v", resp.Error)
			}

			var got struct {
				SessionID string `json:"sessionId"`
				Meta      struct {
					Model string `json:"gofer/model"`
				} `json:"_meta"`
			}
			if err := json.Unmarshal(resp.Result, &got); err != nil {
				t.Fatalf("unmarshal session/new result %s: %v", resp.Result, err)
			}
			if got.SessionID == "" {
				t.Errorf("session/new result %s: sessionId is empty — the ACP field must survive the _meta extension", resp.Result)
			}
			if got.Meta.Model != tt.wantModel {
				t.Errorf("session/new result %s: _meta[\"gofer/model\"] = %q, want %q",
					resp.Result, got.Meta.Model, tt.wantModel)
			}
		})
	}
}

// newSessionMetaResult is the session/new response as these tests decode it:
// the ACP field plus every gofer-namespaced `_meta` key, asserted as raw wire
// keys so a rename can't slip past.
type newSessionMetaResult struct {
	SessionID string `json:"sessionId"`
	Meta      struct {
		Model    string `json:"gofer/model"`
		ParentID string `json:"gofer/parent"`
		Agent    string `json:"gofer/agent"`
		Depth    int    `json:"gofer/depth"`
	} `json:"_meta"`
}

// TestSessionNewSubagentMeta drives the request half of the `_meta` extension:
// a client names a parent and an agent on session/new, and the daemon must both
// honor them (creating a real CHILD session) and report the link — including the
// Depth it derived itself, which the client cannot know — back on the response.
//
// The subtests share the parent created up front, so the depth-2 case exercises
// a genuine two-level chain rather than a synthesized one.
func TestSessionNewSubagentMeta(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	d := daemon.New(sup, daemon.Config{DefaultModel: "claude-sonnet-4-5"})
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	c := dial(t, context.Background(), "ws"+srv.URL[len("http"):], nil)
	cwd := t.TempDir()

	// newSession issues session/new with an optional gofer `_meta` block.
	newSession := func(t *testing.T, parent, agent string) newSessionMetaResult {
		t.Helper()
		params := map[string]any{"cwd": cwd}
		if parent != "" || agent != "" {
			params["_meta"] = map[string]any{"gofer/parent": parent, "gofer/agent": agent}
		}
		resp := c.request(acp.MethodSessionNew, params)
		if resp.Error != nil {
			t.Fatalf("session/new error: %+v", resp.Error)
		}
		var got newSessionMetaResult
		if err := json.Unmarshal(resp.Result, &got); err != nil {
			t.Fatalf("unmarshal session/new result %s: %v", resp.Result, err)
		}
		return got
	}

	root := newSession(t, "", "")
	if root.Meta.ParentID != "" || root.Meta.Agent != "" || root.Meta.Depth != 0 {
		t.Fatalf("a plain session/new reported a subagent link: %+v", root.Meta)
	}

	child := newSession(t, root.SessionID, "go-developer")
	grandchild := newSession(t, child.SessionID, "go-reviewer")

	tests := []struct {
		name       string
		got        newSessionMetaResult
		wantParent string
		wantAgent  string
		wantDepth  int
	}{
		{"child links the parent at depth 1", child, root.SessionID, "go-developer", 1},
		{"grandchild nests one deeper", grandchild, child.SessionID, "go-reviewer", 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got.SessionID == "" {
				t.Fatal("sessionId is empty — the ACP field must survive the _meta extension")
			}
			if tt.got.Meta.ParentID != tt.wantParent {
				t.Errorf("_meta[\"gofer/parent\"] = %q, want %q", tt.got.Meta.ParentID, tt.wantParent)
			}
			if tt.got.Meta.Agent != tt.wantAgent {
				t.Errorf("_meta[\"gofer/agent\"] = %q, want %q", tt.got.Meta.Agent, tt.wantAgent)
			}
			if tt.got.Meta.Depth != tt.wantDepth {
				t.Errorf("_meta[\"gofer/depth\"] = %d, want %d", tt.got.Meta.Depth, tt.wantDepth)
			}
		})
	}

	// An unknown parent is refused rather than quietly demoted to a root session,
	// and refused as INVALID PARAMS (-32602): the client sent a bad parameter, so
	// a client can tell "you named a session that does not exist" apart from a
	// daemon-side policy refusal like the depth cap (an application error).
	t.Run("unknown parent is invalid params", func(t *testing.T) {
		resp := c.request(acp.MethodSessionNew, map[string]any{
			"cwd":   cwd,
			"_meta": map[string]any{"gofer/parent": "no-such-session"},
		})
		if resp.Error == nil {
			t.Fatalf("session/new with an unknown parent succeeded: %s", resp.Result)
		}
		if resp.Error.Code != -32602 {
			t.Errorf("error code = %d, want -32602 (invalid params): %+v", resp.Error.Code, resp.Error)
		}
	})
}
