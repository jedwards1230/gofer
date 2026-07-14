package daemon_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// loadSession issues session/load for sid over c (attaching c to the session's
// fan-out set) and fails the test on any error. A brand-new session has no
// history, so the load replays no notifications.
func loadSession(t *testing.T, c *wsClient, sid, cwd string) {
	t.Helper()
	resp := c.request(acp.MethodSessionLoad, acp.LoadSessionRequest{SessionID: sid, Cwd: cwd})
	if resp.Error != nil {
		t.Fatalf("session/load error: %+v", resp.Error)
	}
}

// waitUpdate blocks for c's next session/update notification and decodes it.
func waitUpdate(t *testing.T, c *wsClient) sessionUpdateParams {
	t.Helper()
	n := c.waitNotification()
	if n.Method != acp.MethodSessionUpdate {
		t.Fatalf("notification method = %q, want %q", n.Method, acp.MethodSessionUpdate)
	}
	var up sessionUpdateParams
	if err := json.Unmarshal(n.Params, &up); err != nil {
		t.Fatalf("unmarshal session/update params: %v", err)
	}
	return up
}

// waitForPeerCount polls the daemon's fan-out registry until sessionID's
// attached-peer count reaches want, or fails after defaultWait. Detach on
// disconnect is asynchronous (it runs when the closed peer's read loop exits),
// so a test that closes a connection must wait for the registry to settle
// rather than assume it already has.
func waitForPeerCount(t *testing.T, d *daemon.Daemon, sessionID string, want int) {
	t.Helper()
	deadline := time.Now().Add(defaultWait)
	for {
		got := d.PeersForSessionCount(sessionID)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("peer count for %s = %d, want %d (timed out)", sessionID, got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// assertNoMoreUpdates asserts no further notification arrives on c within a
// short grace window — the no-duplicate-delivery check.
func assertNoMoreUpdates(t *testing.T, c *wsClient) {
	t.Helper()
	select {
	case n, ok := <-c.notifications:
		if ok {
			t.Errorf("unexpected extra notification: method=%q params=%s", n.Method, n.Params)
		}
	case <-time.After(100 * time.Millisecond):
	}
}

// TestPromptFanOutToAttachedPeers is the M3 fan-out keystone: a turn ONE peer
// drives is streamed to EVERY peer attached to the session. c1 creates a
// session; both c1 and c2 attach via session/load; c1 drives one prompt.
// Both peers receive the full assistant-delta stream, in order — but the
// user-message echo is suppressed to c1 (the originator, which already knows
// what it typed) while c2 (a merely-attached observer) does receive it.
func TestPromptFanOutToAttachedPeers(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")
	c1 := dial(t, context.Background(), url, nil)
	c2 := dial(t, context.Background(), url, nil)

	cwd := t.TempDir()
	sid := newSession(t, c1, cwd)

	// Both peers attach. session/load returns only after the daemon has
	// registered the caller (attachPeer runs before handleSessionLoad returns),
	// so once both calls below have returned, both peers are guaranteed in the
	// fan-out set before the prompt starts — no arbitrary sleep needed.
	loadSession(t, c1, sid, cwd)
	loadSession(t, c2, sid, cwd)

	respCh := make(chan rpcFrame, 1)
	go func() {
		respCh <- c1.request(acp.MethodSessionPrompt, acp.PromptRequest{
			SessionID: sid,
			Prompt:    []acp.ContentBlock{acp.TextBlock("hi")},
		})
	}()

	// faux.Default(): 2 reasoning chunks + 3 text chunks, each one delta.
	wantDeltas := []struct{ kind, text string }{
		{"agent_thought_chunk", "The user said hello. "},
		{"agent_thought_chunk", "I'll greet them back."},
		{"agent_message_chunk", "Hello"},
		{"agent_message_chunk", "! How can "},
		{"agent_message_chunk", "I help you today?"},
	}

	// c2 (attached, NOT the originator) sees the user echo first, then every
	// assistant delta in order.
	c2User := waitUpdate(t, c2)
	if c2User.Update.SessionUpdate != "user_message_chunk" || c2User.Update.Content.Text != "hi" {
		t.Fatalf("c2 first update = %+v, want user_message_chunk(text=hi)", c2User.Update)
	}
	for i, want := range wantDeltas {
		up := waitUpdate(t, c2)
		if up.SessionID != sid {
			t.Errorf("c2 delta %d: sessionId = %q, want %q", i, up.SessionID, sid)
		}
		if up.Update.SessionUpdate != want.kind || up.Update.Content.Text != want.text {
			t.Errorf("c2 delta %d = %+v, want %s(%q)", i, up.Update, want.kind, want.text)
		}
	}

	// c1 (the originator) sees the SAME deltas but NOT its own user echo: its
	// very first update is the turn's first assistant chunk.
	for i, want := range wantDeltas {
		up := waitUpdate(t, c1)
		if up.Update.SessionUpdate == "user_message_chunk" {
			t.Fatalf("c1 (originator) received its own suppressed user echo: %+v", up.Update)
		}
		if up.Update.SessionUpdate != want.kind || up.Update.Content.Text != want.text {
			t.Errorf("c1 delta %d = %+v, want %s(%q)", i, up.Update, want.kind, want.text)
		}
	}

	final := <-respCh
	if got := promptStopReason(t, final); got != acp.StopReasonEndTurn {
		t.Errorf("originator StopReason = %q, want %q", got, acp.StopReasonEndTurn)
	}

	// Neither peer gets a duplicate or stray update after the turn settles.
	assertNoMoreUpdates(t, c1)
	assertNoMoreUpdates(t, c2)
}

// TestPromptFanOutPeerDisconnectMidTurn is the concurrency guard (run with
// -race): a peer disconnects mid-turn while another peer's turn is streaming.
// The surviving peer must still receive the remaining events and a clean
// terminal, the disconnected peer must be deregistered cleanly (no
// send-on-closed-connection panic, no goroutine leak), and nothing is
// double-delivered. It uses [blockingProvider] to hold the turn open across
// the disconnect deterministically.
func TestPromptFanOutPeerDisconnectMidTurn(t *testing.T) {
	bp := newBlockingProvider()
	sup := newTestSupervisor(t, func() provider.Provider { return bp })
	d, url := newTestDaemon(t, sup, "")

	c1 := dial(t, context.Background(), url, nil) // the driver / survivor
	c2 := dial(t, context.Background(), url, nil) // the observer that disconnects

	cwd := t.TempDir()
	sid := newSession(t, c1, cwd)
	loadSession(t, c1, sid, cwd)
	loadSession(t, c2, sid, cwd)
	waitForPeerCount(t, d, sid, 2) // both attached before the turn starts

	respCh := make(chan rpcFrame, 1)
	go func() {
		respCh <- c1.request(acp.MethodSessionPrompt, acp.PromptRequest{
			SessionID: sid,
			Prompt:    []acp.ContentBlock{acp.TextBlock("hi")},
		})
	}()

	<-bp.started // the turn's first model call is genuinely blocked in flight

	// Disconnect the observer mid-turn. Its server-side peer loop observes the
	// read error and runs detachPeer; the in-flight broadcast to it (if any)
	// just errors and is logged, never panicking.
	c2.close()
	waitForPeerCount(t, d, sid, 1) // only the driver remains attached

	// Release the held turn; the survivor must receive its content and a clean
	// terminal even though a co-subscriber vanished mid-stream.
	close(bp.release)

	up := waitUpdate(t, c1)
	if up.Update.SessionUpdate != "agent_message_chunk" || up.Update.Content.Text != "hello" {
		t.Fatalf("survivor update = %+v, want agent_message_chunk(text=hello)", up.Update)
	}

	final := <-respCh
	if got := promptStopReason(t, final); got != acp.StopReasonEndTurn {
		t.Errorf("survivor StopReason = %q, want %q", got, acp.StopReasonEndTurn)
	}

	// No duplicate delivery to the survivor after the terminal response.
	assertNoMoreUpdates(t, c1)
}
