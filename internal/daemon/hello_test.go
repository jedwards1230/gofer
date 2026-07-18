package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/coder/websocket"

	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// TestGoferHelloHandler round-trips gofer/hello through a real daemon and the
// real [daemon.Client]: the daemon reports its configured build version as
// binaryVersion, [daemon.WireVersion] as wireVersion, and ACP's protocol
// version (1) as acpProtocolVersion. This is the authoritative, connection-
// scoped version exchange a router calls first on every worker (design §6).
func TestGoferHelloHandler(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultWait)
	defer cancel()

	// gofer/hello never creates a session, so this provider is never invoked;
	// it just satisfies the harness's newProvider signature.
	sup := newTestSupervisor(t, func() provider.Provider { return newBlockingProvider() })
	_, url := newTestDaemonWithConfig(t, sup, daemon.Config{Version: "v9.9.9", DefaultModel: "faux"})

	c, err := daemon.Dial(ctx, url, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	got, err := c.Hello(ctx)
	if err != nil {
		t.Fatalf("Hello: %v", err)
	}

	// WireVersion is exported so this external test can name it; it is also
	// asserted == 1 (its current value, design §6) so a bump is a deliberate,
	// test-visible change.
	if daemon.WireVersion != 1 {
		t.Errorf("daemon.WireVersion = %d, want 1", daemon.WireVersion)
	}
	want := daemon.HelloResult{
		BinaryVersion:      "v9.9.9",
		WireVersion:        daemon.WireVersion,
		ACPProtocolVersion: 1,
	}
	if got != want {
		t.Errorf("Hello() = %+v, want %+v", got, want)
	}
}

// TestClientHelloUnsupported proves [daemon.Client.Hello] maps a
// method-not-found (-32601) reply to [daemon.ErrHelloUnsupported] via
// errors.Is — a pre-hello daemon predating the handshake is a known,
// non-fatal case, not a hard failure (design §6). The real daemon now HAS
// gofer/hello, so this stands up a bare WebSocket handler that speaks
// JSON-RPC and answers every request method-not-found, simulating that
// older daemon on the actual client wire.
func TestClientHelloUnsupported(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultWait)
	defer cancel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// nil AcceptOptions matches the real daemon: same-origin only, which a
		// non-browser coder/websocket client (daemon.Dial sends no Origin)
		// satisfies.
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow() //nolint:errcheck // best-effort; the connection is closing anyway
		for {
			// A background context, not r.Context(): the request context is
			// unreliable once the connection is hijacked. The read errors when
			// the client disconnects, ending the loop.
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			var req struct {
				ID json.RawMessage `json:"id"`
			}
			if err := json.Unmarshal(data, &req); err != nil {
				continue
			}
			// A JSON-RPC notification has no id and takes no response; only
			// requests (id present) get the method-not-found reply.
			if len(req.ID) == 0 {
				continue
			}
			resp := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not found"}}`, req.ID)
			if err := conn.Write(context.Background(), websocket.MessageText, []byte(resp)); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	url := "ws" + srv.URL[len("http"):]
	c, err := daemon.Dial(ctx, url, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	_, err = c.Hello(ctx)
	if !errors.Is(err, daemon.ErrHelloUnsupported) {
		t.Fatalf("Hello() err = %v, want errors.Is(err, daemon.ErrHelloUnsupported)", err)
	}
}
