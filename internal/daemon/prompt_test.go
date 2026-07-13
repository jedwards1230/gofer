package daemon_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/provider"
)

// sessionUpdateParams is the wire shape of a session/update notification's
// params, decoded loosely enough to assert on the discriminator and text
// without importing acp's server-side-only decode helpers (there are none for
// the client direction — acp is written for gofer to play the agent role).
type sessionUpdateParams struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		SessionUpdate string `json:"sessionUpdate"`
		Content       struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"update"`
}

// newSession dials c and issues session/new, returning the resulting session
// id. It fails the test on any error.
func newSession(t *testing.T, c *wsClient, cwd string) string {
	t.Helper()
	resp := c.request(acp.MethodSessionNew, acp.NewSessionRequest{Cwd: cwd})
	if resp.Error != nil {
		t.Fatalf("session/new error: %+v", resp.Error)
	}
	var sess acp.NewSessionResponse
	if err := json.Unmarshal(resp.Result, &sess); err != nil {
		t.Fatalf("unmarshal NewSessionResponse: %v", err)
	}
	if sess.SessionID == "" {
		t.Fatal("session/new: empty sessionId")
	}
	return sess.SessionID
}

// TestSessionNewPromptStream drives the full happy path: session/new, then
// session/prompt, asserting every scripted delta arrives as a session/update
// notification (in order) before the terminal PromptResponse. The prompt's
// own text arrives first, as a settled user_message_chunk (a user turn has
// no deltas — see event.MessageUser's doc) — checked separately below, ahead
// of the scripted agent deltas.
func TestSessionNewPromptStream(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	sid := newSession(t, c, t.TempDir())

	respCh := make(chan rpcFrame, 1)
	go func() {
		respCh <- c.request(acp.MethodSessionPrompt, acp.PromptRequest{
			SessionID: sid,
			Prompt:    []acp.ContentBlock{acp.TextBlock("hi")},
		})
	}()

	userNotif := c.waitNotification()
	if userNotif.Method != acp.MethodSessionUpdate {
		t.Fatalf("user echo: method = %q, want %q", userNotif.Method, acp.MethodSessionUpdate)
	}
	var userUp sessionUpdateParams
	if err := json.Unmarshal(userNotif.Params, &userUp); err != nil {
		t.Fatalf("user echo: unmarshal params: %v", err)
	}
	if userUp.Update.SessionUpdate != "user_message_chunk" || userUp.Update.Content.Text != "hi" {
		t.Fatalf("user echo = %+v, want user_message_chunk(text=hi)", userUp.Update)
	}

	// faux.Default() scripts 2 reasoning chunks + 3 text chunks = 5 deltas,
	// each projecting to exactly one session/update notification, in order.
	wantChunks := []string{
		"The user said hello. ", "I'll greet them back.",
		"Hello", "! How can ", "I help you today?",
	}
	wantKinds := []string{
		"agent_thought_chunk", "agent_thought_chunk",
		"agent_message_chunk", "agent_message_chunk", "agent_message_chunk",
	}

	for i, wantText := range wantChunks {
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
		if up.Update.SessionUpdate != wantKinds[i] {
			t.Errorf("notification %d: sessionUpdate = %q, want %q", i, up.Update.SessionUpdate, wantKinds[i])
		}
		if up.Update.Content.Text != wantText {
			t.Errorf("notification %d: text = %q, want %q", i, up.Update.Content.Text, wantText)
		}
	}

	final := <-respCh
	if final.Error != nil {
		t.Fatalf("session/prompt error: %+v", final.Error)
	}
	var pr acp.PromptResponse
	if err := json.Unmarshal(final.Result, &pr); err != nil {
		t.Fatalf("unmarshal PromptResponse: %v", err)
	}
	if pr.StopReason != acp.StopReasonEndTurn {
		t.Errorf("StopReason = %q, want %q", pr.StopReason, acp.StopReasonEndTurn)
	}
}

// TestSessionCancelInterruptsPrompt asserts session/cancel interrupts an
// in-flight turn: the outstanding session/prompt request resolves with
// PromptResponse{StopReasonCancelled} instead of hanging. It uses
// [blockingProvider] rather than the faux provider because faux's Stream
// never blocks — there would be no reliable window to interrupt.
func TestSessionCancelInterruptsPrompt(t *testing.T) {
	bp := newBlockingProvider()
	sup := newTestSupervisor(t, func() provider.Provider { return bp })
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	sid := newSession(t, c, t.TempDir())

	respCh := make(chan rpcFrame, 1)
	go func() {
		respCh <- c.request(acp.MethodSessionPrompt, acp.PromptRequest{
			SessionID: sid,
			Prompt:    []acp.ContentBlock{acp.TextBlock("hi")},
		})
	}()

	<-bp.started // the turn's first model call is genuinely blocked in flight

	c.notify(acp.MethodSessionCancel, acp.CancelNotification{SessionID: sid})

	final := <-respCh
	if final.Error != nil {
		t.Fatalf("session/prompt error after cancel: %+v", final.Error)
	}
	var pr acp.PromptResponse
	if err := json.Unmarshal(final.Result, &pr); err != nil {
		t.Fatalf("unmarshal PromptResponse: %v", err)
	}
	if pr.StopReason != acp.StopReasonCancelled {
		t.Errorf("StopReason = %q, want %q", pr.StopReason, acp.StopReasonCancelled)
	}
}
