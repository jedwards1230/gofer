package daemon_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/coder/websocket"
)

// wantMaxConns mirrors internal daemon.maxConns (unexported, so this test
// can't reference it directly). If that constant ever changes this test's
// loop bound must change with it.
const wantMaxConns = 128

// TestServeWS_MaxConnsRejects503 asserts the daemon accepts at most
// wantMaxConns concurrent WebSocket connections and refuses the next upgrade
// attempt with 503, rather than blocking it or accepting it unbounded. Every
// dial call is synchronous (Dial doesn't return until the server has either
// upgraded the connection or answered with a non-101 status), so opening the
// connections sequentially is already deterministic — no sleeps, no
// goroutine barrier needed.
func TestServeWS_MaxConnsRejects503(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")

	ctx := context.Background()
	for i := 0; i < wantMaxConns; i++ {
		conn, _, err := websocket.Dial(ctx, url, nil)
		if err != nil {
			t.Fatalf("dial %d/%d: %v", i+1, wantMaxConns, err)
		}
		t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	}

	_, resp, err := websocket.Dial(ctx, url, nil)
	if err == nil {
		t.Fatal("dial past the cap: want an error (upgrade refused), got none")
	}
	if resp == nil {
		t.Fatalf("dial past the cap: want a non-nil response, err = %v", err)
		return
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("dial past the cap: status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
}
