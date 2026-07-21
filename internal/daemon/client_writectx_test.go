package daemon_test

// client_writectx_test.go pins the write-context safety invariant the whole
// #142 refactor exists to make structural: a write to the shared client
// connection runs under a context the WRITE PATH owns, never one borrowed from a
// caller. coder/websocket's Write registers a context.AfterFunc(ctx, …c.close())
// — cancelling a write's context destroys the WHOLE connection — so a Call whose
// caller cancels its context mid-flight must NOT tear down the link every other
// session shares over it.
//
// [daemon.Client.Notify] takes no context at all, so the borrowed-context
// mistake is not even expressible there (it would not compile). This file covers
// the residual surface: [daemon.Client.Call], which keeps a context for the
// RESPONSE WAIT only, must still write its request frame under the owned bound —
// so a cancelled caller context abandons the wait yet leaves the connection
// healthy for the next write.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// newFrameCapturingWSServer accepts one WebSocket connection and pushes every
// inbound text frame onto the returned channel until the client hangs up. It
// never sends a response of its own, so a [daemon.Client.Call] against it blocks
// on its response wait — exactly the condition under which a caller's
// cancellation would, before this refactor, have been handed to the write.
func newFrameCapturingWSServer(t *testing.T) (url string, frames <-chan []byte) {
	t.Helper()
	ch := make(chan []byte, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
		for {
			typ, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			if typ != websocket.MessageText {
				continue
			}
			cp := append([]byte(nil), data...)
			select {
			case ch <- cp:
			case <-r.Context().Done():
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + srv.URL[len("http"):], ch
}

// TestCallWriteSurvivesCallerCancellation is the load-bearing regression test
// for #142. It calls [daemon.Client.Call] with an already-cancelled context and
// asserts two things that together encode the invariant:
//
//  1. The request frame STILL reaches the wire — proving the write ran under the
//     write path's own bound, not the cancelled caller context.
//  2. The connection is STILL usable afterwards — a subsequent [daemon.Client.Notify]
//     lands its frame — proving the cancelled Call did not trip
//     coder/websocket's close-on-cancel and tear the shared link down.
//
// Before the refactor, step 1's write would have run under the cancelled context
// and closed the connection, so step 2 would fail with a closed-connection error.
func TestCallWriteSurvivesCallerCancellation(t *testing.T) {
	url, frames := newFrameCapturingWSServer(t)

	c, err := daemon.Dial(context.Background(), url, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the caller has already given up before the write

	// The server never responds, so Call can only return via the response-wait
	// select observing the cancelled context — after the request write has
	// already run under its own owned bound.
	_, callErr := c.Call(ctx, "gofer/roster", nil)
	if !errors.Is(callErr, context.Canceled) {
		t.Fatalf("Call with cancelled ctx = %v, want context.Canceled", callErr)
	}

	// (1) The write ran despite the cancelled ctx: the frame reached the server.
	select {
	case <-frames:
	case <-time.After(2 * time.Second):
		t.Fatal("request frame never reached the wire — the write was bound to the cancelled caller ctx")
	}

	// (2) The connection survived: a following notification still writes. If the
	// cancelled Call had closed the connection, this would fail.
	if err := c.Notify("session/cancel", map[string]string{"sessionId": "s1"}); err != nil {
		t.Fatalf("Notify after a cancelled Call: %v — the connection was torn down", err)
	}
	select {
	case <-frames:
	case <-time.After(2 * time.Second):
		t.Fatal("notification frame never reached the wire — the connection did not survive the cancelled Call")
	}
}
