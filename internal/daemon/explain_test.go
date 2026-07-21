package daemon_test

// explain_test.go covers the ACP session/explain_permission handler: the
// request shapes it accepts and rejects (modeled on setconfig_test.go's
// subtests), and — the load-bearing one — that answering it changes nothing.
// An explain that quietly consumed the pending request would look correct in
// every render and lose the human's decision.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"
)

// pendingExplainFixture drives one turn to a real pending permission request
// and returns the session id and the call id the gate is holding — the shared
// setup for the explain cases below. The turn stays blocked on that gate for
// the rest of the test.
//
// It registers a cleanup that answers the request and waits for the driving
// session/prompt to return, so the blocked turn is always released before the
// harness tears the server down. Without it the in-flight prompt would observe
// the connection closing and fail the test from its own goroutine — for
// reasons having nothing to do with what the test asserted. A duplicate reply
// (the read-only test answers the request itself) is a no-op the daemon
// rejects, and a prompt that already returned makes the wait fall straight
// through.
func pendingExplainFixture(t *testing.T, h *approvalHarness, c *wsClient) (sessionID, callID string) {
	t.Helper()

	newResp := c.request(acp.MethodSessionNew, map[string]any{"cwd": t.TempDir()})
	if newResp.Error != nil {
		t.Fatalf("session/new: %v", newResp.Error)
	}
	var created struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(newResp.Result, &created); err != nil {
		t.Fatalf("decode session/new: %v", err)
	}

	promptDone := make(chan rpcFrame, 1)
	go func() {
		promptDone <- c.request(acp.MethodSessionPrompt, map[string]any{"sessionId": created.SessionID, "text": "rm -rf /"})
	}()

	req := waitForNotificationMethod(t, c, "gofer/permission_requested")
	var pr struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(req.Params, &pr); err != nil {
		t.Fatalf("decode permission_requested: %v", err)
	}
	if pr.ID == "" {
		t.Fatal("permission_requested carried no call id")
	}

	t.Cleanup(func() {
		c.notify("permission.reply", map[string]any{"id": pr.ID, "verdict": "allow"})
		select {
		case <-promptDone:
		case <-time.After(defaultWait):
			t.Error("the driving session/prompt never returned after the pending request was released")
		}
	})
	return created.SessionID, pr.ID
}

// explain sends session/explain_permission and returns the raw frame, so a
// caller can assert on either the result or the error.
func explain(t *testing.T, c *wsClient, sessionID, callID string) rpcFrame {
	t.Helper()
	return c.request(acp.MethodSessionExplainPermission, acp.ExplainPermissionRequest{
		SessionID:  sessionID,
		ToolCallID: callID,
	})
}

// TestExplainPermissionAnswersFromTheHeldRequest is the happy path: with a
// request outstanding, the daemon answers why it was gated from the params it
// retained when it broadcast it — reason, policy label, and the guard's raw
// trace verbatim.
func TestExplainPermissionAnswersFromTheHeldRequest(t *testing.T) {
	h := newApprovalHarness(t)
	// context.Background(), not a cancel-on-return context: this client
	// outlives the test body — the fixture's cleanup answers the pending
	// request through it after every deferred cancel would already have fired,
	// and a cancelled connection there would fail the write instead.
	c := dial(t, context.Background(), h.url, nil)

	sid, callID := pendingExplainFixture(t, h, c)

	resp := explain(t, c, sid, callID)
	if resp.Error != nil {
		t.Fatalf("session/explain_permission: %+v", resp.Error)
	}
	var got acp.ExplainPermissionResponse
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("decode ExplainPermissionResponse: %v", err)
	}
	if got.Rationale.Reason == "" {
		t.Error("rationale carried no reason — an explain must always say something")
	}
	// approvalSession's guard reports `rule: ask` (see approvals_test.go).
	if got.Rationale.Policy != "ask" {
		t.Errorf("rationale policy = %q, want the matched rule label %q", got.Rationale.Policy, "ask")
	}
	if want := []string{"rule: ask"}; len(got.Rationale.Trace) != 1 || got.Rationale.Trace[0] != want[0] {
		t.Errorf("rationale trace = %v, want the guard's own entries %v verbatim", got.Rationale.Trace, want)
	}
}

// TestExplainPermissionRequestShapes covers the rejections: a call id nothing
// is pending for, a call id belonging to a different session (which must not
// leak that session's rationale), and the two required fields. Each must be an
// ERROR — a zero rationale would tell a client "gated for no stated reason"
// when the truth is "not pending" or "not yours".
func TestExplainPermissionRequestShapes(t *testing.T) {
	h := newApprovalHarness(t)
	c := dial(t, context.Background(), h.url, nil) // outlives the body — see the first case

	sid, callID := pendingExplainFixture(t, h, c)

	tests := []struct {
		name      string
		sessionID string
		callID    string
		wantCode  int
	}{
		{"unknown call id", sid, "no-such-call", -32000},
		{"call id from another session", "some-other-session", callID, -32000},
		{"missing session id", "", callID, -32602},
		{"missing tool call id", sid, "", -32602},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := explain(t, c, tc.sessionID, tc.callID)
			if resp.Error == nil {
				t.Fatalf("session/explain_permission(%q, %q): want an error, got result %s", tc.sessionID, tc.callID, resp.Result)
			}
			if resp.Error.Code != tc.wantCode {
				t.Errorf("error code = %d (%s), want %d", resp.Error.Code, resp.Error.Message, tc.wantCode)
			}
		})
	}

	t.Run("malformed params", func(t *testing.T) {
		resp := c.request(acp.MethodSessionExplainPermission, "not an object")
		if resp.Error == nil {
			t.Fatal("session/explain_permission with non-object params: want an error, got none")
		}
		if resp.Error.Code != -32602 {
			t.Errorf("error code = %d, want -32602 (invalid params)", resp.Error.Code)
		}
	})
}

// TestExplainPermissionIsReadOnly is the invariant the whole feature rests on:
// explaining a request — twice, to catch a handler that consumes the retained
// params on first read — leaves it pending, and the human's later
// permission.reply still resolves the gate and finishes the turn.
//
// It is deliberately end-to-end (the approvals round-trip harness, a real
// supervisor and gate) rather than a unit assertion on the daemon's maps: what
// must not break is a user's ability to ANSWER after reading the explanation,
// and that only the whole pipeline can prove.
func TestExplainPermissionIsReadOnly(t *testing.T) {
	h := newApprovalHarness(t)
	c := dial(t, context.Background(), h.url, nil) // outlives the body — see the first case

	sid, callID := pendingExplainFixture(t, h, c)

	for i := range 2 {
		resp := explain(t, c, sid, callID)
		if resp.Error != nil {
			t.Fatalf("explain #%d: %+v", i+1, resp.Error)
		}
	}

	// The reply still lands: the gate delivers the verdict and the turn ends.
	c.notify("permission.reply", map[string]any{"id": callID, "verdict": "allow"})

	select {
	case got := <-h.fake(sid).verdicts:
		if got != event.VerdictAllow {
			t.Fatalf("gate delivered verdict %q, want allow", got)
		}
	case <-time.After(defaultWait):
		t.Fatal("the gate never unblocked after a reply that followed two explains — an explain resolved or dropped the request")
	}

	waitForNotificationMethod(t, c, "gofer/permission_resolved")

	// ...and once it HAS resolved, explaining it is an error rather than a
	// stale rationale: the retained request is released on resolution exactly
	// as before, so nothing leaked either.
	if resp := explain(t, c, sid, callID); resp.Error == nil {
		t.Error("explaining a resolved request succeeded — the retained request outlived its gate")
	}
}
