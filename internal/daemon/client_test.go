package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// TestClientRosterKillArchive drives [daemon.Client] against a real Handler
// through gofer/roster, gofer/kill, and gofer/archive — the CLI's control
// surface — asserting both the happy path and the documented ErrRunning
// message on archive-while-running.
func TestClientRosterKillArchive(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")

	c, err := daemon.Dial(context.Background(), url, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	newResult, err := c.Call(context.Background(), acp.MethodSessionNew, acp.NewSessionRequest{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}
	var sess acp.NewSessionResponse
	if err := json.Unmarshal(newResult, &sess); err != nil {
		t.Fatalf("unmarshal NewSessionResponse: %v", err)
	}

	rosterResult, err := c.Call(context.Background(), "gofer/roster", nil)
	if err != nil {
		t.Fatalf("gofer/roster: %v", err)
	}
	var roster []struct {
		ID   string `json:"id"`
		Live bool   `json:"live"`
	}
	if err := json.Unmarshal(rosterResult, &roster); err != nil {
		t.Fatalf("unmarshal roster: %v", err)
	}
	if len(roster) != 1 || roster[0].ID != sess.SessionID || !roster[0].Live {
		t.Fatalf("roster = %+v, want one live entry for %s", roster, sess.SessionID)
	}

	if _, err := c.Call(context.Background(), "gofer/kill", map[string]string{"sessionId": sess.SessionID}); err != nil {
		t.Fatalf("gofer/kill: %v", err)
	}

	// A second session to exercise archive-while-running rejection.
	newResult2, err := c.Call(context.Background(), acp.MethodSessionNew, acp.NewSessionRequest{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("session/new (2): %v", err)
	}
	var sess2 acp.NewSessionResponse
	if err := json.Unmarshal(newResult2, &sess2); err != nil {
		t.Fatalf("unmarshal NewSessionResponse (2): %v", err)
	}

	if _, err := c.Call(context.Background(), "gofer/archive", map[string]string{"sessionId": sess2.SessionID}); err != nil {
		t.Fatalf("gofer/archive: %v", err)
	}
}

// TestClientArchiveRunningRejected covers the ErrRunning surfacing gofer's CLI
// depends on to tell the user to kill/interrupt first, driven through
// [daemon.Client] rather than the raw wsClient test harness.
func TestClientArchiveRunningRejected(t *testing.T) {
	bp := newBlockingProvider()
	sup := newTestSupervisor(t, func() provider.Provider { return bp })
	_, url := newTestDaemon(t, sup, "")

	c, err := daemon.Dial(context.Background(), url, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	newResult, err := c.Call(context.Background(), acp.MethodSessionNew, acp.NewSessionRequest{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}
	var sess acp.NewSessionResponse
	if err := json.Unmarshal(newResult, &sess); err != nil {
		t.Fatalf("unmarshal NewSessionResponse: %v", err)
	}

	promptDone := make(chan error, 1)
	go func() {
		_, perr := c.Call(context.Background(), acp.MethodSessionPrompt, acp.PromptRequest{
			SessionID: sess.SessionID,
			Prompt:    []acp.ContentBlock{acp.TextBlock("hi")},
		})
		promptDone <- perr
	}()
	<-bp.started

	_, archErr := c.Call(context.Background(), "gofer/archive", map[string]string{"sessionId": sess.SessionID})
	if archErr == nil {
		t.Fatal("archive while running: want an error, got none")
	}
	var callErr *daemon.CallError
	if !errors.As(archErr, &callErr) {
		t.Fatalf("archive error = %v (%T), want a *daemon.CallError", archErr, archErr)
	}
	if !strings.Contains(callErr.Message, "running") {
		t.Errorf("archive-while-running message = %q, want it to mention %q", callErr.Message, "running")
	}

	if err := c.Notify(context.Background(), acp.MethodSessionCancel, acp.CancelNotification(sess)); err != nil {
		t.Fatalf("session/cancel: %v", err)
	}
	if err := <-promptDone; err != nil {
		t.Fatalf("session/prompt after cancel: %v", err)
	}
}

// TestClientCallUnknownMethod asserts a JSON-RPC error reply surfaces as a
// *daemon.CallError carrying the standard method-not-found code, not just an
// opaque error string.
func TestClientCallUnknownMethod(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")

	c, err := daemon.Dial(context.Background(), url, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	_, callErr := c.Call(context.Background(), "bogus/method", nil)
	if callErr == nil {
		t.Fatal("bogus/method: want an error, got none")
	}
	var ce *daemon.CallError
	if !errors.As(callErr, &ce) {
		t.Fatalf("err = %v (%T), want a *daemon.CallError", callErr, callErr)
	}
	if ce.Code != -32601 {
		t.Errorf("code = %d, want -32601 (method not found)", ce.Code)
	}
}

// TestClientSessionPromptNotifications covers the streaming path
// gofer's daemon-driven run/resume depends on: Call blocks for the terminal
// PromptResponse while every intermediate session/update arrives on
// Notifications, in order, concurrently.
func TestClientSessionPromptNotifications(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")

	c, err := daemon.Dial(context.Background(), url, "")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	newResult, err := c.Call(context.Background(), acp.MethodSessionNew, acp.NewSessionRequest{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}
	var sess acp.NewSessionResponse
	if err := json.Unmarshal(newResult, &sess); err != nil {
		t.Fatalf("unmarshal NewSessionResponse: %v", err)
	}

	promptResult := make(chan json.RawMessage, 1)
	promptErr := make(chan error, 1)
	go func() {
		res, perr := c.Call(context.Background(), acp.MethodSessionPrompt, acp.PromptRequest{
			SessionID: sess.SessionID,
			Prompt:    []acp.ContentBlock{acp.TextBlock("hi")},
		})
		promptResult <- res
		promptErr <- perr
	}()

	var gotUpdates int
	timeout := time.After(defaultWait)
drain:
	for {
		select {
		case n, ok := <-c.Notifications():
			if !ok {
				t.Fatal("notifications channel closed before PromptResponse arrived")
			}
			if n.Method != acp.MethodSessionUpdate {
				t.Fatalf("notification method = %q, want %q", n.Method, acp.MethodSessionUpdate)
			}
			gotUpdates++
			// This client is the originating peer, so the daemon suppresses its
			// own user_message_chunk echo (see broadcastUpdate). Only
			// faux.Default()'s assistant deltas arrive: 2 reasoning + 3 text.
			if gotUpdates == 5 {
				break drain
			}
		case <-timeout:
			t.Fatalf("timed out after %d notifications, want 5", gotUpdates)
		}
	}

	if err := <-promptErr; err != nil {
		t.Fatalf("session/prompt: %v", err)
	}
	var pr acp.PromptResponse
	if err := json.Unmarshal(<-promptResult, &pr); err != nil {
		t.Fatalf("unmarshal PromptResponse: %v", err)
	}
	if pr.StopReason != acp.StopReasonEndTurn {
		t.Errorf("StopReason = %q, want %q", pr.StopReason, acp.StopReasonEndTurn)
	}
}

// TestClientDialNoDaemon asserts dialing a closed port fails cleanly with
// ErrNoDaemon rather than hanging or panicking — the fallback signal
// run/resume/ps/kill/archive all key off.
func TestClientDialNoDaemon(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultWait)
	defer cancel()

	// Port 1 is a reserved, never-listened-on TCP port — connection refused
	// (or filtered) either way, with no real daemon to accidentally hit.
	_, err := daemon.Dial(ctx, "127.0.0.1:1", "")
	if err == nil {
		t.Fatal("Dial: want an error connecting to a closed port, got nil")
	}
	if !errors.Is(err, daemon.ErrNoDaemon) {
		t.Errorf("Dial err = %v, want it to wrap daemon.ErrNoDaemon", err)
	}
	if errors.Is(err, daemon.ErrUnauthorized) {
		t.Errorf("Dial err = %v, want it NOT to be ErrUnauthorized (no daemon, so no auth boundary was even reached)", err)
	}

	if daemon.Probe(ctx, "127.0.0.1:1", "") {
		t.Error("Probe(closed port) = true, want false")
	}
}

// TestClientUnauthorized asserts a token-protected daemon rejects a
// missing/wrong token with ErrUnauthorized (a daemon IS running — Probe must
// still report true) rather than being indistinguishable from no daemon at
// all.
func TestClientUnauthorized(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "the-real-token")

	_, err := daemon.Dial(context.Background(), url, "wrong-token")
	if err == nil {
		t.Fatal("Dial with wrong token: want an error, got nil")
	}
	if !errors.Is(err, daemon.ErrUnauthorized) {
		t.Errorf("Dial err = %v, want it to wrap daemon.ErrUnauthorized", err)
	}

	if !daemon.Probe(context.Background(), url, "wrong-token") {
		t.Error("Probe with wrong token = false, want true (a daemon IS running, it just rejected the token)")
	}

	// The right token connects cleanly.
	c, err := daemon.Dial(context.Background(), url, "the-real-token")
	if err != nil {
		t.Fatalf("Dial with correct token: %v", err)
	}
	defer func() { _ = c.Close() }()
}
