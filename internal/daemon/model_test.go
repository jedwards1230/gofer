package daemon_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/acp"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// TestSessionNewModelResolution covers session/new's model handling: a
// client-supplied model is honored end-to-end (proving handleSessionNew
// actually reads acp.NewSessionRequest.Model off the raw params, since
// event.SessionNew's ACP projection drops it — see acp.FromNewSession's doc),
// and an empty/absent model falls back to the daemon's configured
// DefaultModel. DefaultModel is deliberately set to a value distinct from the
// requested model in the honored case, so the assertion cannot pass merely
// because the daemon always resolves to its own default.
func TestSessionNewModelResolution(t *testing.T) {
	const defaultModel = "claude-sonnet-4-5"
	const requestedModel = "claude-haiku-4-5"

	sup := newTestSupervisor(t, fauxProvider)
	d := daemon.New(sup, daemon.Config{DefaultModel: defaultModel})
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	url := "ws" + srv.URL[len("http"):]
	c := dial(t, context.Background(), url, nil)

	tests := []struct {
		name      string
		reqModel  string
		wantModel string
	}{
		{
			name:      "client-supplied model is honored",
			reqModel:  requestedModel,
			wantModel: requestedModel,
		},
		{
			name:      "empty model falls back to the daemon default",
			reqModel:  "",
			wantModel: defaultModel,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := c.request(acp.MethodSessionNew, acp.NewSessionRequest{Cwd: t.TempDir(), Model: tt.reqModel})
			if resp.Error != nil {
				t.Fatalf("session/new error: %+v", resp.Error)
			}
			sid := decodeSessionID(t, resp)

			rosterResp := c.request("gofer/roster", nil)
			if rosterResp.Error != nil {
				t.Fatalf("gofer/roster error: %+v", rosterResp.Error)
			}
			var roster []sessionInfoWire
			if err := json.Unmarshal(rosterResp.Result, &roster); err != nil {
				t.Fatalf("unmarshal roster: %v", err)
			}
			var found bool
			for _, s := range roster {
				if s.ID != sid {
					continue
				}
				found = true
				if s.Model != tt.wantModel {
					t.Errorf("roster model = %q, want %q", s.Model, tt.wantModel)
				}
			}
			if !found {
				t.Fatalf("session %s missing from gofer/roster: %+v", sid, roster)
			}
		})
	}
}
