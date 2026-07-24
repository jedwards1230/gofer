package daemon_test

// seteffort_test.go is setmodel_test.go's effort-axis twin over the wire:
// gofer/set_effort's happy path (set, then still promptable), its params
// contract — which deliberately differs from gofer/set_model's on the empty
// value — and its application errors.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/acp"
)

// TestGoferSetEffort is the set-then-prompt proof: after a gofer/set_effort
// change the roster reflects the new level AND the session is still fully
// promptable over the wire — the reconfigured runner is not left in a
// half-broken state.
func TestGoferSetEffort(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	resp := c.request(acp.MethodSessionNew, acp.NewSessionRequest{Cwd: t.TempDir(), Model: "claude-sonnet-5"})
	if resp.Error != nil {
		t.Fatalf("session/new error: %+v", resp.Error)
	}
	sid := decodeSessionID(t, resp)

	setResp := c.request("gofer/set_effort", map[string]string{"sessionId": sid, "effort": "high"})
	if setResp.Error != nil {
		t.Fatalf("gofer/set_effort error: %+v", setResp.Error)
	}

	if got := rosterEffort(t, c, sid); got != "high" {
		t.Errorf("roster effort after set_effort = %q, want high", got)
	}

	promptResp := c.request(acp.MethodSessionPrompt, acp.PromptRequest{
		SessionID: sid,
		Prompt:    []acp.ContentBlock{acp.TextBlock("hi")},
	})
	if promptResp.Error != nil {
		t.Fatalf("session/prompt after set_effort error: %+v", promptResp.Error)
	}
}

// TestGoferSetEffortEmptyClears is the contract that separates this method
// from gofer/set_model, where an empty value is invalid params. "" is the SDK's
// documented "clear the level back to the provider's default", so it must be
// ACCEPTED and must actually move the roster back — rejecting it would make the
// clear operation unreachable over the wire (see decodeSetEffortParams).
func TestGoferSetEffortEmptyClears(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	sid := newSession(t, c, t.TempDir())

	if resp := c.request("gofer/set_effort", map[string]string{"sessionId": sid, "effort": "medium"}); resp.Error != nil {
		t.Fatalf("gofer/set_effort medium error: %+v", resp.Error)
	}
	if got := rosterEffort(t, c, sid); got != "medium" {
		t.Fatalf("test premise broken: roster effort = %q, want medium before the clear", got)
	}

	resp := c.request("gofer/set_effort", map[string]string{"sessionId": sid, "effort": ""})
	if resp.Error != nil {
		t.Fatalf("gofer/set_effort with an empty effort: want it accepted as a clear, got error %+v", resp.Error)
	}
	if got := rosterEffort(t, c, sid); got != "" {
		t.Errorf("roster effort after the clear = %q, want \"\"", got)
	}
}

// TestGoferSetEffortMissingSessionID asserts the one thing that IS invalid
// params here.
func TestGoferSetEffortMissingSessionID(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	resp := c.request("gofer/set_effort", map[string]string{"effort": "high"})
	if resp.Error == nil {
		t.Fatal("gofer/set_effort with no sessionId: want an error, got none")
	}
}

// TestGoferSetEffortUnknownLevel asserts a level outside the unified
// vocabulary surfaces as a clear application error naming the offending value,
// leaving the session's level untouched.
func TestGoferSetEffortUnknownLevel(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	sid := newSession(t, c, t.TempDir())

	resp := c.request("gofer/set_effort", map[string]string{"sessionId": sid, "effort": "ultra"})
	if resp.Error == nil {
		t.Fatal("gofer/set_effort with an unknown level: want an error, got none")
	}
	if !strings.Contains(resp.Error.Message, "ultra") {
		t.Errorf("error message = %q, want it to name the offending level", resp.Error.Message)
	}
	if got := rosterEffort(t, c, sid); got != "" {
		t.Errorf("roster effort after a rejected set_effort = %q, want the unchanged \"\"", got)
	}
}

// TestGoferSetEffortUnknownSession asserts gofer/set_effort against a session
// id the supervisor has never seen surfaces as a clear application error.
func TestGoferSetEffortUnknownSession(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	resp := c.request("gofer/set_effort", map[string]string{"sessionId": "does-not-exist", "effort": "low"})
	if resp.Error == nil {
		t.Fatal("gofer/set_effort on unknown session: want an error, got none")
	}
}

// rosterEffort reads sid's effort back off gofer/roster — the only way a client
// can observe the change, and therefore the assertion that proves the wire
// field is actually populated rather than dropped in the DTO mapping.
func rosterEffort(t *testing.T, c *wsClient, sid string) string {
	t.Helper()
	resp := c.request("gofer/roster", nil)
	if resp.Error != nil {
		t.Fatalf("gofer/roster error: %+v", resp.Error)
	}
	var roster []sessionInfoWire
	if err := json.Unmarshal(resp.Result, &roster); err != nil {
		t.Fatalf("unmarshal roster: %v", err)
	}
	for _, s := range roster {
		if s.ID == sid {
			return s.Effort
		}
	}
	t.Fatalf("session %s missing from gofer/roster: %+v", sid, roster)
	return ""
}
