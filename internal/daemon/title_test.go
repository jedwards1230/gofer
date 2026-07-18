package daemon_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
)

// sessionInfoUpdateParams is the wire shape of a session_info_update
// session/update notification's params — the ACP projection of the SDK's
// session.info event (see acp.SessionInfoUpdate). It decodes the discriminator
// and the derived title, enough to prove the title reaches a peer.
type sessionInfoUpdateParams struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		SessionUpdate string `json:"sessionUpdate"`
		Title         string `json:"title"`
	} `json:"update"`
}

// waitSessionInfoUpdate blocks for c's next session_info_update session/update
// notification, silently skipping every other frame (content chunks, gofer/
// event, ...), and returns its decoded params. It reads c.notifications
// directly rather than through waitNotification, which deliberately SKIPS the
// title update so content-focused tests aren't disturbed by it.
func waitSessionInfoUpdate(t *testing.T, c *wsClient) sessionInfoUpdateParams {
	t.Helper()
	deadline := time.After(defaultWait)
	for {
		select {
		case f, ok := <-c.notifications:
			if !ok {
				t.Fatalf("connection closed waiting for a session_info_update")
			}
			if !isSessionInfoUpdate(f) {
				continue
			}
			var up sessionInfoUpdateParams
			if err := json.Unmarshal(f.Params, &up); err != nil {
				t.Fatalf("unmarshal session_info_update params: %v", err)
			}
			return up
		case <-deadline:
			t.Fatalf("timed out waiting for a session_info_update")
		}
	}
}

// assertNoSessionInfoUpdate asserts NO session_info_update arrives on c within a
// short grace window — the set-once check that a re-prompt does not re-emit the
// title. Other frames (content chunks, gofer/event) are expected and skipped.
func assertNoSessionInfoUpdate(t *testing.T, c *wsClient) {
	t.Helper()
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case f, ok := <-c.notifications:
			if !ok {
				return
			}
			if isSessionInfoUpdate(f) {
				var up sessionInfoUpdateParams
				_ = json.Unmarshal(f.Params, &up)
				t.Errorf("unexpected re-emitted session_info_update: title=%q", up.Update.Title)
				return
			}
		case <-deadline:
			return
		}
	}
}

// TestSessionFirstPromptSurfacesTitle is the title pass-through proof: a
// session's FIRST prompt makes the daemon fan a session_info_update session/
// update carrying the title gofer derives from that prompt (first non-empty
// line, whitespace-collapsed, bounded) — for free, via acp.ToSessionUpdate.
// gofer generates the title (supervisor/managed.go's enqueue) and emits the
// SDK's session.info event; it does not build the ACP projection. A SECOND
// prompt must NOT re-emit the title: it is set once from the first prompt.
func TestSessionFirstPromptSurfacesTitle(t *testing.T) {
	sup := newTestSupervisor(t, twoTurnProvider())
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	sid := newSession(t, c, t.TempDir())

	// First prompt: a multi-line, over-spaced prompt whose title is its first
	// non-empty line with internal whitespace collapsed.
	respCh := make(chan rpcFrame, 1)
	go func() {
		respCh <- c.request(acp.MethodSessionPrompt, acp.PromptRequest{
			SessionID: sid,
			Prompt:    []acp.ContentBlock{acp.TextBlock("investigate   the flaky build\nmore detail on the next line")},
		})
	}()

	info := waitSessionInfoUpdate(t, c)
	if info.SessionID != sid {
		t.Errorf("session_info_update sessionId = %q, want %q", info.SessionID, sid)
	}
	if info.Update.SessionUpdate != "session_info_update" {
		t.Errorf("sessionUpdate = %q, want session_info_update", info.Update.SessionUpdate)
	}
	if info.Update.Title != "investigate the flaky build" {
		t.Errorf("title = %q, want %q", info.Update.Title, "investigate the flaky build")
	}

	// Drain the first turn's content and terminal so the second prompt streams
	// cleanly (twoTurnProvider scripts one text chunk per turn).
	if got := promptStopReason(t, drainPrompt(t, c, respCh, sid, 1)); got != acp.StopReasonEndTurn {
		t.Errorf("prompt #1 StopReason = %q, want %q", got, acp.StopReasonEndTurn)
	}

	// Second prompt on the same session: the title is already set, so no new
	// session_info_update is emitted (set-once; SetTitle is emit-on-change and
	// the supervisor never regenerates the title on a later turn).
	resp2, _ := drivePrompt(t, c, sid, "second prompt with an entirely different first line", 1)
	if got := promptStopReason(t, resp2); got != acp.StopReasonEndTurn {
		t.Errorf("prompt #2 StopReason = %q, want %q", got, acp.StopReasonEndTurn)
	}
	assertNoSessionInfoUpdate(t, c)
}

// drainPrompt drains wantNotifs content session/update notifications for an
// already-issued prompt whose response arrives on respCh, then returns that
// response — the split-out variant of drivePrompt used when the caller issued
// the session/prompt itself (here, to observe the title update before the
// content stream).
func drainPrompt(t *testing.T, c *wsClient, respCh <-chan rpcFrame, sid string, wantNotifs int) rpcFrame {
	t.Helper()
	for i := 0; i < wantNotifs; i++ {
		n := c.waitNotification()
		if n.Method != acp.MethodSessionUpdate {
			t.Fatalf("notification %d: method = %q, want %q", i, n.Method, acp.MethodSessionUpdate)
		}
		var up sessionUpdateParams
		if err := json.Unmarshal(n.Params, &up); err != nil {
			t.Fatalf("notification %d: unmarshal params: %v", i, err)
		}
		if up.SessionID != sid {
			t.Errorf("notification %d: sessionId = %q, want %q", i, up.SessionID, sid)
		}
	}
	return <-respCh
}
