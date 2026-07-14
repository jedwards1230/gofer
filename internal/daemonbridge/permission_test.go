package daemonbridge_test

// permission_test.go covers Supervisor.Reply's wire format directly against a
// raw, capturing WebSocket server — not the daemon test harness bridge_test.go
// otherwise uses — because "permission.reply"'s daemon-side handler
// (internal/daemon, contract #1 of the M3 implementation brief) is out of
// this package's edit scope and may not exist yet in this worktree. This
// test pins ONLY what daemonbridge itself is responsible for: sending a bare
// JSON-RPC notification (no id/response expected) for method
// "permission.reply" with params {"id","verdict","remember"}.
//
// internal/daemonbridge's own reconstruct_internal_test.go (package
// daemonbridge) covers the receiving half — reconstructing
// event.PermissionRequested/PermissionResolved from the daemon's matching
// notifications.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/daemonbridge"
)

// newCapturingWSServer starts an httptest.Server that accepts exactly one
// WebSocket connection, reads exactly one text frame off it, and delivers
// the decoded JSON-RPC frame on the returned channel. It never writes a
// response — permission.reply is a fire-and-forget notification, so
// [daemon.Client.Notify] neither expects nor waits for one.
func newCapturingWSServer(t *testing.T) (url string, frames <-chan map[string]any) {
	t.Helper()
	ch := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
		_, data, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		var frame map[string]any
		if err := json.Unmarshal(data, &frame); err == nil {
			ch <- frame
		}
		<-r.Context().Done() // keep the connection open until the client hangs up
	}))
	t.Cleanup(srv.Close)
	return "ws" + srv.URL[len("http"):], ch
}

// TestReplySendsPermissionReplyNotification asserts Supervisor.Reply sends a
// bare notification (no "id" key) for method "permission.reply" with params
// {"id":<call id>,"verdict":"allow"|"deny","remember":<bool>} — contract #1
// of the M3 implementation brief, verbatim.
func TestReplySendsPermissionReplyNotification(t *testing.T) {
	url, frames := newCapturingWSServer(t)

	c, err := daemon.Dial(context.Background(), url, "")
	if err != nil {
		t.Fatalf("daemon.Dial: %v", err)
	}
	b := daemonbridge.New(c)
	defer func() { _ = b.Close() }()

	if err := b.Reply(context.Background(), "sess-1", "perm-1", true, true); err != nil {
		t.Fatalf("Reply: %v", err)
	}

	select {
	case frame := <-frames:
		if _, hasID := frame["id"]; hasID {
			t.Errorf("frame has an \"id\" key (%v): want a bare notification, not a Call", frame["id"])
		}
		if got := frame["method"]; got != "permission.reply" {
			t.Errorf("method = %v, want %q", got, "permission.reply")
		}
		params, ok := frame["params"].(map[string]any)
		if !ok {
			t.Fatalf("params = %v (%T), want an object", frame["params"], frame["params"])
		}
		if params["id"] != "perm-1" {
			t.Errorf("params.id = %v, want %q", params["id"], "perm-1")
		}
		if params["verdict"] != "allow" {
			t.Errorf("params.verdict = %v, want %q", params["verdict"], "allow")
		}
		if params["remember"] != true {
			t.Errorf("params.remember = %v, want true", params["remember"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the permission.reply frame")
	}
}

// TestReplyDenyOmitsRemember asserts a deny reply with remember=false sends
// verdict="deny" and, per event.PermissionReply's own
// `remember,omitempty"`-mirroring wire shape, either omits "remember" or
// carries it false — never true.
func TestReplyDenyOmitsRemember(t *testing.T) {
	url, frames := newCapturingWSServer(t)

	c, err := daemon.Dial(context.Background(), url, "")
	if err != nil {
		t.Fatalf("daemon.Dial: %v", err)
	}
	b := daemonbridge.New(c)
	defer func() { _ = b.Close() }()

	if err := b.Reply(context.Background(), "sess-1", "perm-1", false, false); err != nil {
		t.Fatalf("Reply: %v", err)
	}

	select {
	case frame := <-frames:
		params, ok := frame["params"].(map[string]any)
		if !ok {
			t.Fatalf("params = %v (%T), want an object", frame["params"], frame["params"])
		}
		if params["verdict"] != "deny" {
			t.Errorf("params.verdict = %v, want %q", params["verdict"], "deny")
		}
		if v, present := params["remember"]; present && v != false {
			t.Errorf("params.remember = %v, want absent or false", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the permission.reply frame")
	}
}
