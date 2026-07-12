package daemon_test

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/provider"
)

// TestShutdownUnblocksInflightPrompt asserts that cancelling a connection's
// context unwinds an in-flight session/prompt handler rather than leaking it.
// It drives a turn into a genuinely-blocked state (blockingProvider), then
// shuts the daemon down — which cancels every connection context — and requires
// the outstanding request to unwind promptly (an error reply, or the connection
// closing) instead of hanging. This exercises the handler's ctx.Done() path,
// the same unwind a raw client disconnect reaches once the read loop cancels
// the connection context (see peer.run).
func TestShutdownUnblocksInflightPrompt(t *testing.T) {
	bp := newBlockingProvider()
	sup := newTestSupervisor(t, func() provider.Provider { return bp })
	d, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	sid := newSession(t, c, t.TempDir())

	// Send session/prompt at the wire level rather than via c.request, which
	// fails the test if the connection closes — here a close is a valid way for
	// the handler to have unwound, so we tolerate it.
	id := atomic.AddInt64(&c.idc, 1)
	idJSON, _ := json.Marshal(id)
	ch := c.register(string(idJSON))
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

	if err := d.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	select {
	case <-ch: // an error reply delivered, or the channel closed on teardown — either means the handler unwound
	case <-time.After(defaultWait):
		t.Fatal("in-flight session/prompt did not unwind after Shutdown — handler leaked")
	}
}
