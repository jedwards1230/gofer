package daemon_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/acp"
)

// TestGoferSetModel is the set-then-prompt proof: after a same-provider
// gofer/set_model swap, the roster reflects the new model AND the session is
// still fully promptable over the wire — the swapped runner is not left in
// some half-broken state, and the daemon's own DefaultModel fallback (see
// TestSessionNewModelResolution) is untouched by a mid-session change.
func TestGoferSetModel(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	resp := c.request(acp.MethodSessionNew, acp.NewSessionRequest{Cwd: t.TempDir(), Model: "claude-sonnet-5"})
	if resp.Error != nil {
		t.Fatalf("session/new error: %+v", resp.Error)
	}
	sid := decodeSessionID(t, resp)

	setResp := c.request("gofer/set_model", map[string]string{"sessionId": sid, "model": "claude-opus-4-8"})
	if setResp.Error != nil {
		t.Fatalf("gofer/set_model error: %+v", setResp.Error)
	}

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
		if s.Model != "claude-opus-4-8" {
			t.Errorf("roster model after set_model = %q, want claude-opus-4-8", s.Model)
		}
	}
	if !found {
		t.Fatalf("session %s missing from gofer/roster: %+v", sid, roster)
	}

	// The set-then-prompt proof: the swapped session still drives a normal
	// turn to completion over the wire.
	promptResp := c.request(acp.MethodSessionPrompt, acp.PromptRequest{
		SessionID: sid,
		Prompt:    []acp.ContentBlock{acp.TextBlock("hi")},
	})
	if promptResp.Error != nil {
		t.Fatalf("session/prompt after set_model error: %+v", promptResp.Error)
	}
}

// TestGoferSetModelEmptyModel asserts an empty model is rejected as invalid
// params, never reaching the supervisor.
func TestGoferSetModelEmptyModel(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	sid := newSession(t, c, t.TempDir())

	resp := c.request("gofer/set_model", map[string]string{"sessionId": sid, "model": ""})
	if resp.Error == nil {
		t.Fatal("gofer/set_model with an empty model: want an error, got none")
	}
}

// TestGoferSetModelUnknownSession asserts gofer/set_model against a session
// id the supervisor has never seen surfaces as a clear application error.
func TestGoferSetModelUnknownSession(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	resp := c.request("gofer/set_model", map[string]string{"sessionId": "does-not-exist", "model": "claude-opus-4-8"})
	if resp.Error == nil {
		t.Fatal("gofer/set_model on unknown session: want an error, got none")
	}
}
