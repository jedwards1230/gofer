package daemon_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// TestServeCtxCancelShutdownWithInflightTurn guards the daemon's real shutdown
// orchestration under load. A live client is attached and a turn is genuinely
// in flight (blockingProvider) when the serve context is cancelled — exactly
// what SIGINT does via signal.NotifyContext in cmd/gofer's runDaemon. It
// asserts BOTH halves of that orchestration return promptly:
//
//  1. Serve(ctx) itself — which runs a REAL listener + graceful Shutdown,
//     unlike the httptest-mounted Handler every other daemon test uses, so this
//     is the only coverage of the Serve→ctx.Done→Shutdown path with traffic on
//     the wire; and
//  2. the sup.Close() that runDaemon calls once Serve returns.
//
// A regression that let an attached peer or an in-flight turn wedge either step
// (e.g. Shutdown blocking on a hijacked connection, or runner.Close joining a
// turn that never observes cancellation) would hang here rather than pass.
// TestShutdownUnblocksInflightPrompt covers only the per-connection handler
// unwind via httptest; it never exercises Serve(ctx) or sup.Close().
func TestServeCtxCancelShutdownWithInflightTurn(t *testing.T) {
	bp := newBlockingProvider()
	sup := newTestSupervisor(t, func() provider.Provider { return bp })

	// A real listener on an ephemeral port, closed immediately so Serve can
	// rebind it — this drives Serve(ctx)+Shutdown, not a mounted Handler.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	d := daemon.New(sup, daemon.Config{ListenAddr: addr, DefaultModel: "faux"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- d.Serve(ctx) }()
	waitForListener(t, addr)

	c := dial(t, context.Background(), "ws://"+addr, nil)
	sid := newSession(t, c, t.TempDir())

	// Drive a turn at the wire level (not c.request, which fails the test if the
	// connection closes — a close is a valid unwind here) and wait until the
	// provider's first call is genuinely blocked in flight.
	id := atomic.AddInt64(&c.idc, 1)
	c.write(struct {
		JSONRPC string            `json:"jsonrpc"`
		ID      int64             `json:"id"`
		Method  string            `json:"method"`
		Params  acp.PromptRequest `json:"params"`
	}{
		jsonrpcVersion, id, acp.MethodSessionPrompt,
		acp.PromptRequest{SessionID: sid, Prompt: []acp.ContentBlock{acp.TextBlock("hi")}},
	})
	<-bp.started // the turn's first model call is genuinely blocked in flight

	// SIGINT-equivalent: cancel the serve context with the turn in flight and
	// the peer still attached and streaming.
	cancel()

	select {
	case serr := <-serveErr:
		if serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			t.Fatalf("Serve returned error after ctx cancel: %v", serr)
		}
	case <-time.After(defaultWait):
		t.Fatal("Serve did not return within timeout after ctx cancel with an in-flight turn — shutdown hang")
	}

	// runDaemon calls sup.Close() once Serve returns; it must not hang on the
	// in-flight session's runner either. (Also registered as a t.Cleanup by
	// newTestSupervisor; Close is idempotent, so this explicit call is safe.)
	closed := make(chan error, 1)
	go func() { closed <- sup.Close() }()
	select {
	case cerr := <-closed:
		if cerr != nil {
			t.Fatalf("sup.Close after Serve returned: %v", cerr)
		}
	case <-time.After(defaultWait):
		t.Fatal("sup.Close did not return within timeout after Serve — supervisor shutdown hang")
	}
}

// waitForListener blocks until addr accepts a TCP connection or the timeout
// elapses, so the test drives the daemon only once Serve has bound the port.
func waitForListener(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(defaultWait)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("listener %s never came up", addr)
}
