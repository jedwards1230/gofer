package daemonbridge

// reconstruct_internal_test.go covers handleNotification's
// gofer/permission_requested and gofer/permission_resolved reconstruction
// (see reconstruct.go) directly, without a real *daemon.Client or a live
// daemon: neither branch touches s.client (registerFresh pre-populates the
// session's reconstruction state so handleNotification's s.session lookup
// never falls to the loadHistory path that would need one), so a bare
// Supervisor value wired up the same way [New] does — minus the client and
// the demux goroutine — is enough to drive them synchronously.
// permission_test.go's (package daemonbridge_test)
// TestReplySendsPermissionReplyNotification is this package's other half of
// the M3 approvals-relay contract: the outbound "permission.reply"
// notification, over a real (if minimal) WebSocket connection.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// newReconstructTestSupervisor returns a Supervisor with just enough state
// for handleNotification's permission branches: a session map and a closed
// channel selectable in [Supervisor.session]'s guard, no *daemon.Client.
func newReconstructTestSupervisor() *Supervisor {
	return &Supervisor{
		sessions: make(map[string]*sessionState),
		closed:   make(chan struct{}),
	}
}

// TestHandleNotificationReconstructsPermissionRequested asserts a
// "permission.requested" notification, in event.PermissionRequested's own
// MarshalJSON shape, reconstructs into an event.PermissionRequested on the
// named session's broker.
func TestHandleNotificationReconstructsPermissionRequested(t *testing.T) {
	s := newReconstructTestSupervisor()
	const sid = "sess-1"
	s.registerFresh(sid)

	sub, err := s.Subscribe(context.Background(), sid)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	params := json.RawMessage(`{"sessionId":"sess-1","id":"perm-1","tool":"bash","spec":{"cmd":"rm -rf /tmp/x"},"trace":["no rule"]}`)
	s.handleNotification(daemon.Notification{Method: methodGoferPermissionRequested, Params: params})

	select {
	case ev := <-sub.C:
		pr, ok := ev.(event.PermissionRequested)
		if !ok {
			t.Fatalf("got %T, want event.PermissionRequested", ev)
		}
		if pr.ID != "perm-1" || pr.Tool != "bash" {
			t.Errorf("PermissionRequested = %+v, want ID=perm-1 Tool=bash", pr)
		}
		if pr.Spec["cmd"] != "rm -rf /tmp/x" {
			t.Errorf("PermissionRequested.Spec = %+v, want cmd=rm -rf /tmp/x", pr.Spec)
		}
		if len(pr.Trace) != 1 || pr.Trace[0] != "no rule" {
			t.Errorf("PermissionRequested.Trace = %+v, want [no rule]", pr.Trace)
		}
		if pr.SessionID() != sid {
			t.Errorf("SessionID() = %q, want %q", pr.SessionID(), sid)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the reconstructed PermissionRequested")
	}
}

// TestHandleNotificationReconstructsPermissionResolved asserts a
// "permission.resolved" notification reconstructs into an
// event.PermissionResolved.
func TestHandleNotificationReconstructsPermissionResolved(t *testing.T) {
	s := newReconstructTestSupervisor()
	const sid = "sess-1"
	s.registerFresh(sid)

	sub, err := s.Subscribe(context.Background(), sid)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	params := json.RawMessage(`{"sessionId":"sess-1","id":"perm-1","verdict":"deny","rule":"deny bash rm"}`)
	s.handleNotification(daemon.Notification{Method: methodGoferPermissionResolved, Params: params})

	select {
	case ev := <-sub.C:
		pr, ok := ev.(event.PermissionResolved)
		if !ok {
			t.Fatalf("got %T, want event.PermissionResolved", ev)
		}
		if pr.ID != "perm-1" || pr.Verdict != event.VerdictDeny || pr.Rule != "deny bash rm" {
			t.Errorf("PermissionResolved = %+v, want ID=perm-1 Verdict=deny Rule=%q", pr, "deny bash rm")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the reconstructed PermissionResolved")
	}
}

// TestHandleNotificationDropsMalformedPermissionRequested asserts a
// permission.requested notification with no session_id (or invalid JSON) is
// dropped rather than panicking or creating a stray session entry — the same
// tolerance handleNotification's ACP session/update path already has for a
// protocol drift.
func TestHandleNotificationDropsMalformedPermissionRequested(t *testing.T) {
	s := newReconstructTestSupervisor()
	s.handleNotification(daemon.Notification{Method: methodGoferPermissionRequested, Params: json.RawMessage(`{}`)})
	s.handleNotification(daemon.Notification{Method: methodGoferPermissionRequested, Params: json.RawMessage(`not json`)})

	s.mu.Lock()
	n := len(s.sessions)
	s.mu.Unlock()
	if n != 0 {
		t.Errorf("sessions = %d, want 0 (malformed notification should not register a session)", n)
	}
}
