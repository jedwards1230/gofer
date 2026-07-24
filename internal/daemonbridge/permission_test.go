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
	"github.com/jedwards1230/gofer/internal/tui"
)

// newRawCapturingWSServer starts an httptest.Server that accepts exactly one
// WebSocket connection, reads exactly one text frame off it, and delivers
// that frame's BYTES on the returned channel. It never writes a response —
// permission.reply is a fire-and-forget notification, so
// [daemon.Client.Notify] neither expects nor waits for one.
//
// The raw bytes are what the wire-fidelity tests below assert on: "the params
// object is exactly these bytes" is a claim a decoded map cannot make.
func newRawCapturingWSServer(t *testing.T) (url string, frames <-chan json.RawMessage) {
	t.Helper()
	ch := make(chan json.RawMessage, 1)
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
		ch <- json.RawMessage(data)
		<-r.Context().Done() // keep the connection open until the client hangs up
	}))
	t.Cleanup(srv.Close)
	return "ws" + srv.URL[len("http"):], ch
}

// newCapturingWSServer is [newRawCapturingWSServer] with the frame decoded
// into a generic map, for the assertions that only care about member values.
func newCapturingWSServer(t *testing.T) (url string, frames <-chan map[string]any) {
	t.Helper()
	url, raw := newRawCapturingWSServer(t)
	ch := make(chan map[string]any, 1)
	go func() {
		data, ok := <-raw
		if !ok {
			return
		}
		var frame map[string]any
		if err := json.Unmarshal(data, &frame); err == nil {
			ch <- frame
		}
	}()
	return url, ch
}

// awaitReplyParams reads the captured permission.reply frame and returns its
// params member's raw bytes.
func awaitReplyParams(t *testing.T, frames <-chan json.RawMessage) json.RawMessage {
	t.Helper()
	select {
	case data := <-frames:
		var frame struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(data, &frame); err != nil {
			t.Fatalf("decode frame %s: %v", data, err)
		}
		if frame.Method != "permission.reply" {
			t.Fatalf("method = %q, want %q", frame.Method, "permission.reply")
		}
		return frame.Params
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the permission.reply frame")
		return nil
	}
}

// newReplyBridge dials url and returns a bridge over it, closed on cleanup.
func newReplyBridge(t *testing.T, url string) *daemonbridge.Supervisor {
	t.Helper()
	c, err := daemon.Dial(context.Background(), url, "")
	if err != nil {
		t.Fatalf("daemon.Dial: %v", err)
	}
	b := daemonbridge.New(c)
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// TestReplyPlainAllowOmitsInput is the backward-compatibility pin for the
// amend field: a plain allow must put the EXACT same bytes on the wire as it
// did before amend existed, so a daemon too old to know about "input" is
// unaffected. `omitempty` is what makes that true, and a byte comparison is
// the only assertion that proves it — a decoded map would happily report a
// present-but-null member as absent.
func TestReplyPlainAllowOmitsInput(t *testing.T) {
	url, frames := newRawCapturingWSServer(t)
	b := newReplyBridge(t, url)

	if err := b.Reply(context.Background(), "sess-1", "perm-1", tui.PermissionDecision{Allow: true}); err != nil {
		t.Fatalf("Reply: %v", err)
	}

	got := string(awaitReplyParams(t, frames))
	if want := `{"id":"perm-1","verdict":"allow"}`; got != want {
		t.Errorf("plain allow params = %s, want %s", got, want)
	}
}

// TestReplyAmendedAllowCarriesInput is the other half: an amended allow adds
// the replacement tool input verbatim, so the daemon can hand it to the gate
// and the SDK can substitute it into the call.
func TestReplyAmendedAllowCarriesInput(t *testing.T) {
	url, frames := newRawCapturingWSServer(t)
	b := newReplyBridge(t, url)

	input := json.RawMessage(`{"cmd":"rm -rf /tmp/x --dry-run","timeout":120}`)
	d := tui.PermissionDecision{Allow: true, Remember: true, Input: input}
	if err := b.Reply(context.Background(), "sess-1", "perm-1", d); err != nil {
		t.Fatalf("Reply: %v", err)
	}

	got := string(awaitReplyParams(t, frames))
	want := `{"id":"perm-1","verdict":"allow","remember":true,"input":{"cmd":"rm -rf /tmp/x --dry-run","timeout":120}}`
	if got != want {
		t.Errorf("amended allow params = %s, want %s", got, want)
	}
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

	if err := b.Reply(context.Background(), "sess-1", "perm-1", tui.PermissionDecision{Allow: true, Remember: true}); err != nil {
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

	if err := b.Reply(context.Background(), "sess-1", "perm-1", tui.PermissionDecision{}); err != nil {
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
