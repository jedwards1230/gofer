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
