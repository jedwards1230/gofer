package daemon_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/provider/faux"
)

// twoTurnProvider returns a provider.Provider constructor scripting two
// distinct one-chunk turns, so a test can drive two full session/prompt
// round trips on the same session and tell their content apart on the wire.
// faux.Default (one turn) exhausts after a single Prompt call, which is why
// the sequential-prompt regression tests below need their own script.
func twoTurnProvider() func() provider.Provider {
	script := faux.Script{Turns: []faux.Turn{
		{Text: []string{"turn-one-reply"}, StopReason: provider.StopEndTurn},
		{Text: []string{"turn-two-reply"}, StopReason: provider.StopEndTurn},
	}}
	return func() provider.Provider { return faux.New(script) }
}

// drivePrompt sends one session/prompt for sid over c and collects exactly
// wantNotifs session/update notifications' text content before reading the
// terminal response — mirroring TestSessionNewPromptStream's pattern, where
// the exact notification count is known up front from the scripted turn.
// wantNotifs and the returned texts cover assistant content only: c is the
// ORIGINATING peer, and the daemon suppresses the user-message echo back to
// the peer that drove the prompt (see broadcastUpdate), so — unlike a second,
// merely-attached peer — c never receives its own user_message_chunk. The
// first notification here is therefore the turn's first assistant chunk.
func drivePrompt(t *testing.T, c *wsClient, sid, text string, wantNotifs int) (rpcFrame, []string) {
	t.Helper()

	respCh := make(chan rpcFrame, 1)
	go func() {
		respCh <- c.request(acp.MethodSessionPrompt, acp.PromptRequest{
			SessionID: sid,
			Prompt:    []acp.ContentBlock{acp.TextBlock(text)},
		})
	}()

	texts := make([]string, 0, wantNotifs)
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
		texts = append(texts, up.Update.Content.Text)
	}

	return <-respCh, texts
}

// promptStopReason unmarshals resp's result as a PromptResponse and returns
// its StopReason, failing the test on a response-shaped error or a decode
// failure.
func promptStopReason(t *testing.T, resp rpcFrame) acp.StopReason {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("session/prompt error: %+v", resp.Error)
	}
	var pr acp.PromptResponse
	if err := json.Unmarshal(resp.Result, &pr); err != nil {
		t.Fatalf("unmarshal PromptResponse: %v", err)
	}
	return pr.StopReason
}

// TestTwoSequentialPromptsStreamIndependently is the regression test for the
// bug fixed by [supervisor.Supervisor.SubscribeLive]: the broker replays its
// retained must-deliver backlog into every NEW subscription, a feature meant
// for mid-session attach. handleSessionPrompt used to subscribe fresh (via
// plain Subscribe) per prompt, so the SECOND prompt's fresh subscription
// would be pre-loaded with the FIRST turn's retained turn.finished — the
// wait loop would consume it immediately and return turn #1's stop reason as
// prompt #2's response in ~0ms, with zero session/update notifications for
// prompt #2's actual (never-observed) turn.
//
// Verified against a plain Subscribe (temporarily swapped in and reverted):
// this test fails on the SECOND drivePrompt call, timing out in
// waitNotification — the retained backlog's MessageFinished/TurnFinished
// don't project to a session/update (only MessageDelta/ToolCall* do, see
// [acp.ToSessionUpdate]), so zero notifications ever arrive for prompt #2
// even though its respCh already resolved instantly with turn #1's stale
// stop reason. Against SubscribeLive it passes: each prompt observes only
// its own turn.
func TestTwoSequentialPromptsStreamIndependently(t *testing.T) {
	sup := newTestSupervisor(t, twoTurnProvider())
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	sid := newSession(t, c, t.TempDir())

	resp1, texts1 := drivePrompt(t, c, sid, "hi", 1)
	if got := promptStopReason(t, resp1); got != acp.StopReasonEndTurn {
		t.Errorf("prompt #1 StopReason = %q, want %q", got, acp.StopReasonEndTurn)
	}
	if len(texts1) != 1 || texts1[0] != "turn-one-reply" {
		t.Fatalf("prompt #1 texts = %+v, want [turn-one-reply]", texts1)
	}

	resp2, texts2 := drivePrompt(t, c, sid, "hi again", 1)
	if got := promptStopReason(t, resp2); got != acp.StopReasonEndTurn {
		t.Errorf("prompt #2 StopReason = %q, want %q", got, acp.StopReasonEndTurn)
	}
	if len(texts2) != 1 || texts2[0] != "turn-two-reply" {
		t.Fatalf("prompt #2 texts = %+v, want [turn-two-reply]", texts2)
	}

	// No-duplicate-updates assertion: prompt #2 must not re-emit any of
	// prompt #1's content.
	for _, txt := range texts2 {
		if txt == texts1[0] {
			t.Errorf("prompt #2 re-emitted prompt #1's content: %q", txt)
		}
	}
}

// TestPromptAfterLoadStreams asserts the loaded-session path also composes
// correctly with the SubscribeLive fix: session/load replays folded journal
// history via notifications (a wholly separate mechanism from the broker's
// retained backlog, see handleSessionLoad), and a subsequent session/prompt
// on the now-loaded session streams only its own new turn — not the loaded
// history and not the pre-load turn's retained terminal event.
func TestPromptAfterLoadStreams(t *testing.T) {
	sup := newTestSupervisor(t, twoTurnProvider())
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	cwd := t.TempDir()
	sid := newSession(t, c, cwd)

	resp1, texts1 := drivePrompt(t, c, sid, "hi", 1)
	if got := promptStopReason(t, resp1); got != acp.StopReasonEndTurn {
		t.Errorf("prompt #1 StopReason = %q, want %q", got, acp.StopReasonEndTurn)
	}
	if len(texts1) != 1 || texts1[0] != "turn-one-reply" {
		t.Fatalf("prompt #1 texts = %+v, want [turn-one-reply]", texts1)
	}

	// session/load replays the settled fold as session/update notifications
	// before its response; drain exactly as many as ReplayNotifications
	// would build from the current fold (the same oracle
	// TestSessionLoadReplaysHistoryBeforeResponse uses), so the load
	// response can be read cleanly off respCh below.
	msgs, err := sup.History(context.Background(), sid)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	wantReplay := acp.ReplayNotifications(sid, msgs)
	if len(wantReplay) == 0 {
		t.Fatal("expected at least one replay notification from the settled turn")
	}

	loadRespCh := make(chan rpcFrame, 1)
	go func() {
		loadRespCh <- c.request(acp.MethodSessionLoad, acp.LoadSessionRequest{SessionID: sid, Cwd: cwd})
	}()
	for i := 0; i < len(wantReplay); i++ {
		c.waitNotification()
	}
	loadResp := <-loadRespCh
	if loadResp.Error != nil {
		t.Fatalf("session/load error: %+v", loadResp.Error)
	}

	resp2, texts2 := drivePrompt(t, c, sid, "hi again", 1)
	if got := promptStopReason(t, resp2); got != acp.StopReasonEndTurn {
		t.Errorf("prompt #2 (post-load) StopReason = %q, want %q", got, acp.StopReasonEndTurn)
	}
	if len(texts2) != 1 || texts2[0] != "turn-two-reply" {
		t.Fatalf("prompt #2 (post-load) texts = %+v, want [turn-two-reply]", texts2)
	}
	for _, txt := range texts2 {
		if txt == texts1[0] {
			t.Errorf("prompt #2 (post-load) re-emitted prompt #1's content: %q", txt)
		}
	}
}
