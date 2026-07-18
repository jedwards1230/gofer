package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
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
				// The M3 lossless-attach fanout also sends this turn's full
				// event stream as gofer/event on the SAME connection (see
				// internal/daemon/handlers.go's broadcastGoferEvent) — this
				// test is only about the ACP session/update projection, so
				// skip it rather than fail.
				continue
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

// serveUnix mounts a [daemon.Daemon] built over sup on an http.Server listening
// on an AF_UNIX socket named sockName, returning the "unix://<sockName>" address
// [daemon.Dial] takes. The caller must have already chdir'd into a fresh temp
// dir (t.Chdir): the socket is bound by its RELATIVE name so the kernel stores a
// short bind path, keeping it well under the ~104-byte sun_path limit
// (runtimedir.go) no matter how deep t.TempDir() nests on the host. The server
// is closed on cleanup.
func serveUnix(t *testing.T, sup *supervisor.Supervisor, cfg daemon.Config, sockName string) string {
	t.Helper()
	ln, err := net.Listen("unix", sockName)
	if err != nil {
		t.Fatalf("net.Listen unix %s: %v", sockName, err)
	}
	d := daemon.New(sup, cfg)
	srv := &http.Server{Handler: d.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "unix://" + sockName
}

// TestClientDialUnixSocket proves the full JSON-RPC/WebSocket round trip works
// over the unix transport M6 session-workers listen on (design §3/§4): Probe
// reports a live socket reachable and a dead one not, Dial of a dead socket
// surfaces ErrNoDaemon (the same no-listener signal as a refused TCP port), and
// a live socket drives a real session/new + session/prompt turn end to end.
func TestClientDialUnixSocket(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	t.Chdir(t.TempDir())
	addr := serveUnix(t, sup, daemon.Config{DefaultModel: "faux"}, "t.sock")

	if !daemon.Probe(context.Background(), addr, "") {
		t.Fatal("Probe(live unix socket) = false, want true")
	}

	// A path with no listener: connection refused at the socket, indistinguishable
	// from a closed TCP port — ErrNoDaemon, and Probe false.
	deadErr := func() error {
		_, err := daemon.Dial(context.Background(), "unix://dead.sock", "")
		return err
	}()
	if deadErr == nil {
		t.Fatal("Dial(dead unix socket): want an error, got nil")
	}
	if !errors.Is(deadErr, daemon.ErrNoDaemon) {
		t.Errorf("Dial(dead unix socket) err = %v, want it to wrap daemon.ErrNoDaemon", deadErr)
	}
	if errors.Is(deadErr, daemon.ErrUnauthorized) {
		t.Errorf("Dial(dead unix socket) err = %v, want it NOT to be ErrUnauthorized", deadErr)
	}
	if daemon.Probe(context.Background(), "unix://dead.sock", "") {
		t.Error("Probe(dead unix socket) = true, want false")
	}

	c, err := daemon.Dial(context.Background(), addr, "")
	if err != nil {
		t.Fatalf("Dial(unix): %v", err)
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
				continue // skip the same-connection gofer/event fanout — see TestClientSessionPromptNotifications
			}
			gotUpdates++
			// Originating peer: only faux.Default()'s assistant deltas arrive
			// (2 reasoning + 3 text), the user echo suppressed — same as the TCP
			// TestClientSessionPromptNotifications, proving the unix transport is
			// wire-identical.
			if gotUpdates == 5 {
				break drain
			}
		case <-timeout:
			t.Fatalf("timed out after %d notifications over unix, want 5", gotUpdates)
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

// TestClientDialUnixUnauthorized asserts the ErrUnauthorized-vs-ErrNoDaemon
// distinction Dial draws over TCP holds identically over the unix transport: a
// live worker socket that rejects the bearer token is reachable-but-unauthorized
// (Probe true, Dial ErrUnauthorized), NOT mistaken for an absent worker — the
// 401 rides the HTTP upgrade over the fixed unix conn exactly as over TCP.
func TestClientDialUnixUnauthorized(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	t.Chdir(t.TempDir())
	addr := serveUnix(t, sup, daemon.Config{BearerToken: "the-real-token", DefaultModel: "faux"}, "t.sock")

	_, err := daemon.Dial(context.Background(), addr, "wrong-token")
	if err == nil {
		t.Fatal("Dial(unix) with wrong token: want an error, got nil")
	}
	if !errors.Is(err, daemon.ErrUnauthorized) {
		t.Errorf("Dial(unix) err = %v, want it to wrap daemon.ErrUnauthorized", err)
	}
	if !daemon.Probe(context.Background(), addr, "wrong-token") {
		t.Error("Probe(unix) with wrong token = false, want true (a worker IS listening, it just rejected the token)")
	}

	c, err := daemon.Dial(context.Background(), addr, "the-real-token")
	if err != nil {
		t.Fatalf("Dial(unix) with correct token: %v", err)
	}
	defer func() { _ = c.Close() }()
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
