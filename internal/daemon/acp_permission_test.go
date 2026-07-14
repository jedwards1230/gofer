package daemon_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"
)

// acpPermissionRequest is the subset of a session/request_permission request
// params a test needs to assert on and answer.
type acpPermissionRequest struct {
	SessionID string `json:"sessionId"`
	ToolCall  struct {
		ToolCallID string `json:"toolCallId"`
		Title      string `json:"title"`
	} `json:"toolCall"`
	Options []struct {
		OptionID string `json:"optionId"`
		Kind     string `json:"kind"`
	} `json:"options"`
}

// selectedResponse builds a session/request_permission response selecting optionID.
func selectedResponse(optionID string) acp.RequestPermissionResponse {
	return acp.RequestPermissionResponse{Outcome: acp.PermissionOutcomeSelected{OptionID: optionID}}
}

// newACPSession creates a session over driver and returns its id.
func newACPSession(t *testing.T, driver *wsClient, cwd string) string {
	t.Helper()
	resp := driver.request("session/new", map[string]any{"cwd": cwd})
	if resp.Error != nil {
		t.Fatalf("session/new: %v", resp.Error)
	}
	var created struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(resp.Result, &created); err != nil {
		t.Fatalf("decode session/new: %v", err)
	}
	return created.SessionID
}

// awaitReq drains a peer's inbound requests until it sees a
// session/request_permission and decodes it.
func awaitPermissionRequest(t *testing.T, c *wsClient) (json.RawMessage, acpPermissionRequest) {
	t.Helper()
	deadline := time.After(defaultWait)
	for {
		select {
		case f, ok := <-c.inboundRequests:
			if !ok {
				t.Fatal("connection closed waiting for session/request_permission")
			}
			if f.Method != acp.MethodSessionRequestPermission {
				t.Fatalf("inbound request method = %q, want %q", f.Method, acp.MethodSessionRequestPermission)
			}
			var pr acpPermissionRequest
			if err := json.Unmarshal(f.Params, &pr); err != nil {
				t.Fatalf("decode request_permission params: %v", err)
			}
			return f.ID, pr
		case <-deadline:
			t.Fatal("timed out waiting for session/request_permission")
		}
	}
}

// waitOutstandingPermReqs polls until the daemon's outstanding permission-request
// count reaches want (cancellation on resolution is asynchronous).
func waitOutstandingPermReqs(t *testing.T, h *approvalHarness, want int) {
	t.Helper()
	deadline := time.Now().Add(defaultWait)
	var last int
	for time.Now().Before(deadline) {
		last = h.d.OutstandingPermissionRequestCount()
		if last == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("outstanding permission requests = %d, want %d", last, want)
}

// TestACPPermissionRoundTrip is the phone-approval acceptance test: a pure ACP
// peer receives the spec session/request_permission REQUEST for a turn another
// peer drives, answers it, and the answer resolves the driving turn's gate. The
// four option kinds map to the expected verdict + remember.
func TestACPPermissionRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name        string
		option      string
		wantVerdict event.Verdict
	}{
		{"allow once", string(acp.PermissionAllowOnce), event.VerdictAllow},
		{"allow always", string(acp.PermissionAllowAlways), event.VerdictAllow},
		{"reject once", string(acp.PermissionRejectOnce), event.VerdictDeny},
		{"reject always", string(acp.PermissionRejectAlways), event.VerdictDeny},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newApprovalHarness(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			cwd := t.TempDir()
			driver := dial(t, ctx, h.url, nil)
			phone := dial(t, ctx, h.url, nil)

			sid := newACPSession(t, driver, cwd)
			if lr := phone.request("session/load", map[string]any{"sessionId": sid, "cwd": cwd}); lr.Error != nil {
				t.Fatalf("session/load: %v", lr.Error)
			}

			promptDone := make(chan rpcFrame, 1)
			go func() {
				promptDone <- driver.request("session/prompt", map[string]any{"sessionId": sid, "text": "rm -rf /"})
			}()

			// The phone (a pure ACP client) receives the spec request and its
			// options, and answers it.
			reqID, pr := awaitPermissionRequest(t, phone)
			if pr.SessionID != sid || pr.ToolCall.ToolCallID == "" || pr.ToolCall.Title != "bash" {
				t.Fatalf("request params = %+v, want session %s / non-empty toolCallId / title bash", pr, sid)
			}
			if len(pr.Options) != 4 {
				t.Fatalf("request offered %d options, want 4", len(pr.Options))
			}
			phone.respond(reqID, selectedResponse(tc.option))

			// The gate delivered the verdict the phone's option maps to.
			select {
			case got := <-h.fake(sid).verdicts:
				if got != tc.wantVerdict {
					t.Fatalf("gate verdict = %q, want %q", got, tc.wantVerdict)
				}
			case <-time.After(defaultWait):
				t.Fatal("gate did not unblock after the phone answered")
			}

			select {
			case resp := <-promptDone:
				if resp.Error != nil {
					t.Fatalf("session/prompt: %v", resp.Error)
				}
			case <-time.After(defaultWait):
				t.Fatal("session/prompt did not return after the turn resolved")
			}

			// No daemon-side waiter dangles once the permission resolved.
			waitOutstandingPermReqs(t, h, 0)
		})
	}
}

// TestACPPermissionRememberMapping proves the remember distinction survives the
// ACP round trip: allow_always routes an allow WITH remember, allow_once WITHOUT.
func TestACPPermissionRememberMapping(t *testing.T) {
	for _, tc := range []struct {
		option       string
		wantRemember bool
	}{
		{string(acp.PermissionAllowAlways), true},
		{string(acp.PermissionAllowOnce), false},
	} {
		t.Run(tc.option, func(t *testing.T) {
			h := newApprovalHarness(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			cwd := t.TempDir()
			driver := dial(t, ctx, h.url, nil)
			phone := dial(t, ctx, h.url, nil)
			sid := newACPSession(t, driver, cwd)
			if lr := phone.request("session/load", map[string]any{"sessionId": sid, "cwd": cwd}); lr.Error != nil {
				t.Fatalf("session/load: %v", lr.Error)
			}
			promptDone := make(chan rpcFrame, 1)
			go func() {
				promptDone <- driver.request("session/prompt", map[string]any{"sessionId": sid, "text": "ls"})
			}()

			reqID, _ := awaitPermissionRequest(t, phone)
			phone.respond(reqID, selectedResponse(tc.option))

			select {
			case got := <-h.fake(sid).replies:
				if got.Remember != tc.wantRemember {
					t.Fatalf("gate reply remember = %v, want %v", got.Remember, tc.wantRemember)
				}
			case <-time.After(defaultWait):
				t.Fatal("gate did not receive a reply")
			}
			if resp := <-promptDone; resp.Error != nil {
				t.Fatalf("session/prompt: %v", resp.Error)
			}
		})
	}
}

// TestGoferNativeClassificationOrdering pins the classification default: a peer
// that has NOT yet invoked any gofer/* method or permission.reply when the first
// permission fires is treated as ACP (the safe default), so it receives the
// session/request_permission request — AND can still resolve the turn via a
// permission.reply, which is exactly how a gofer client whose roster poll hasn't
// landed yet behaves. Proves the default never strands such a peer.
func TestGoferNativeClassificationOrdering(t *testing.T) {
	h := newApprovalHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cwd := t.TempDir()
	// A single peer that drives the turn WITHOUT any prior gofer/* call, so at
	// fan-out time it is still classified ACP.
	c := dial(t, ctx, h.url, nil)
	sid := newACPSession(t, c, cwd)

	promptDone := make(chan rpcFrame, 1)
	go func() {
		promptDone <- c.request("session/prompt", map[string]any{"sessionId": sid, "text": "ls"})
	}()

	// Being ACP-classified, it receives the spec request...
	reqID, pr := awaitPermissionRequest(t, c)
	_ = reqID
	// ...yet it answers via the gofer-native permission.reply, and the gate resolves.
	c.notify("permission.reply", map[string]any{"id": pr.ToolCall.ToolCallID, "verdict": "allow"})

	select {
	case got := <-h.fake(sid).verdicts:
		if got != event.VerdictAllow {
			t.Fatalf("gate verdict = %q, want allow", got)
		}
	case <-time.After(defaultWait):
		t.Fatal("gate did not unblock after permission.reply")
	}
	if resp := <-promptDone; resp.Error != nil {
		t.Fatalf("session/prompt: %v", resp.Error)
	}
	waitOutstandingPermReqs(t, h, 0)
}

// TestACPPermissionClientRejectsRequest: a client that cannot answer
// session/request_permission (it replies with a JSON-RPC error) must not wedge
// the daemon — the error is a no-op, and a gofer-native answer still resolves
// the turn.
func TestACPPermissionClientRejectsRequest(t *testing.T) {
	h := newApprovalHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cwd := t.TempDir()
	tui := dial(t, ctx, h.url, nil)
	phone := dial(t, ctx, h.url, nil)

	if r := tui.request("gofer/roster", nil); r.Error != nil {
		t.Fatalf("gofer/roster: %v", r.Error)
	}
	sid := newACPSession(t, tui, cwd)
	if lr := phone.request("session/load", map[string]any{"sessionId": sid, "cwd": cwd}); lr.Error != nil {
		t.Fatalf("session/load: %v", lr.Error)
	}

	promptDone := make(chan rpcFrame, 1)
	go func() {
		promptDone <- tui.request("session/prompt", map[string]any{"sessionId": sid, "text": "ls"})
	}()

	// The phone rejects the request (as a client that doesn't implement it would).
	reqID, pr := awaitPermissionRequest(t, phone)
	phone.respondError(reqID, -32601, "method not found")

	// The gofer-native answer still resolves the gate; the rejection was a no-op.
	tui.notify("permission.reply", map[string]any{"id": pr.ToolCall.ToolCallID, "verdict": "allow"})
	select {
	case got := <-h.fake(sid).verdicts:
		if got != event.VerdictAllow {
			t.Fatalf("gate verdict = %q, want allow", got)
		}
	case <-time.After(defaultWait):
		t.Fatal("gate did not unblock after the gofer-native answer")
	}
	if resp := <-promptDone; resp.Error != nil {
		t.Fatalf("session/prompt: %v", resp.Error)
	}
	waitOutstandingPermReqs(t, h, 0)
}

// TestACPPermissionRaceGoferFirst: a gofer-native peer (the TUI) answers via
// permission.reply before the ACP peer (phone) does. The gate takes the TUI's
// verdict, the phone's outstanding session/request_permission is retracted (no
// dangling daemon waiter), and a late ACP answer is a harmless no-op.
func TestACPPermissionRaceGoferFirst(t *testing.T) {
	h := newApprovalHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cwd := t.TempDir()
	tui := dial(t, ctx, h.url, nil)
	phone := dial(t, ctx, h.url, nil)

	// The TUI identifies as gofer-native by polling the roster before it drives
	// anything, so the daemon never sends it a session/request_permission.
	if r := tui.request("gofer/roster", nil); r.Error != nil {
		t.Fatalf("gofer/roster: %v", r.Error)
	}
	sid := newACPSession(t, tui, cwd)
	if lr := phone.request("session/load", map[string]any{"sessionId": sid, "cwd": cwd}); lr.Error != nil {
		t.Fatalf("session/load: %v", lr.Error)
	}

	promptDone := make(chan rpcFrame, 1)
	go func() {
		promptDone <- tui.request("session/prompt", map[string]any{"sessionId": sid, "text": "ls"})
	}()

	// The phone gets the spec request; the TUI answers first via permission.reply.
	reqID, pr := awaitPermissionRequest(t, phone)
	tui.notify("permission.reply", map[string]any{"id": pr.ToolCall.ToolCallID, "verdict": "allow"})

	select {
	case got := <-h.fake(sid).verdicts:
		if got != event.VerdictAllow {
			t.Fatalf("gate verdict = %q, want allow", got)
		}
	case <-time.After(defaultWait):
		t.Fatal("gate did not unblock after the TUI answered")
	}
	select {
	case resp := <-promptDone:
		if resp.Error != nil {
			t.Fatalf("session/prompt: %v", resp.Error)
		}
	case <-time.After(defaultWait):
		t.Fatal("session/prompt did not return")
	}

	// The phone's request was retracted — no daemon-side waiter dangles.
	waitOutstandingPermReqs(t, h, 0)

	// A late ACP answer is a no-op: it must not panic, error the connection, or
	// deliver a second verdict.
	phone.respond(reqID, selectedResponse(string(acp.PermissionRejectOnce)))
	select {
	case got := <-h.fake(sid).verdicts:
		t.Fatalf("late ACP answer delivered a second verdict %q — must be a no-op", got)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestACPPermissionRacePhoneFirst: the ACP peer (phone) answers before the
// gofer-native peer. The gate takes the phone's verdict, and the gofer-native
// peer receives gofer/permission_resolved so its own dialog resolves.
func TestACPPermissionRacePhoneFirst(t *testing.T) {
	h := newApprovalHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cwd := t.TempDir()
	tui := dial(t, ctx, h.url, nil)
	phone := dial(t, ctx, h.url, nil)

	if r := tui.request("gofer/roster", nil); r.Error != nil {
		t.Fatalf("gofer/roster: %v", r.Error)
	}
	sid := newACPSession(t, tui, cwd)
	if lr := phone.request("session/load", map[string]any{"sessionId": sid, "cwd": cwd}); lr.Error != nil {
		t.Fatalf("session/load: %v", lr.Error)
	}

	promptDone := make(chan rpcFrame, 1)
	go func() {
		promptDone <- tui.request("session/prompt", map[string]any{"sessionId": sid, "text": "ls"})
	}()

	reqID, _ := awaitPermissionRequest(t, phone)
	phone.respond(reqID, selectedResponse(string(acp.PermissionRejectOnce)))

	select {
	case got := <-h.fake(sid).verdicts:
		if got != event.VerdictDeny {
			t.Fatalf("gate verdict = %q, want deny", got)
		}
	case <-time.After(defaultWait):
		t.Fatal("gate did not unblock after the phone answered")
	}

	// The gofer-native peer's dialog resolves via gofer/permission_resolved.
	waitForNotificationMethod(t, tui, "gofer/permission_resolved")

	select {
	case resp := <-promptDone:
		if resp.Error != nil {
			t.Fatalf("session/prompt: %v", resp.Error)
		}
	case <-time.After(defaultWait):
		t.Fatal("session/prompt did not return")
	}
}

// TestACPPermissionInterruptWhilePending: a permission is pending at the phone
// when the turn is interrupted (session/cancel). The interrupt resolves the gate
// (deny/cancelled), and the phone's outstanding session/request_permission is
// retracted rather than left dangling.
func TestACPPermissionInterruptWhilePending(t *testing.T) {
	h := newApprovalHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cwd := t.TempDir()
	driver := dial(t, ctx, h.url, nil)
	phone := dial(t, ctx, h.url, nil)
	sid := newACPSession(t, driver, cwd)
	if lr := phone.request("session/load", map[string]any{"sessionId": sid, "cwd": cwd}); lr.Error != nil {
		t.Fatalf("session/load: %v", lr.Error)
	}

	promptDone := make(chan rpcFrame, 1)
	go func() {
		promptDone <- driver.request("session/prompt", map[string]any{"sessionId": sid, "text": "ls"})
	}()

	// The phone has the pending request; there is exactly one outstanding fan-out.
	awaitPermissionRequest(t, phone)
	waitOutstandingPermReqs(t, h, 1)

	// Interrupt the turn.
	driver.notify("session/cancel", map[string]any{"sessionId": sid})

	// The interrupt retracts the phone's outstanding request.
	waitOutstandingPermReqs(t, h, 0)

	// The driving prompt returns (cancelled) rather than hanging.
	select {
	case <-promptDone:
	case <-time.After(defaultWait):
		t.Fatal("session/prompt did not return after interrupt")
	}
}
