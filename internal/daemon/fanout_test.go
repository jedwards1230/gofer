package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
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

// assertNoMoreUpdates asserts no further session/update notification arrives
// on c within a short grace window — the no-duplicate-delivery check.
// Trailing "gofer/event" frames (the turn's own terminal events, which the
// daemon ALSO fans on this same connection — see waitNotification's doc) are
// expected and skipped, not an error.
func assertNoMoreUpdates(t *testing.T, c *wsClient) {
	t.Helper()
	deadline := time.After(100 * time.Millisecond)
	for {
		select {
		case n, ok := <-c.notifications:
			if !ok {
				return
			}
			if n.Method == "gofer/event" || isSessionInfoUpdate(n) {
				continue
			}
			t.Errorf("unexpected extra notification: method=%q params=%s", n.Method, n.Params)
			return
		case <-deadline:
			return
		}
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

// goferEventEnvelope decodes just the discriminator fields shared by every
// gofer/event notification's params — the source event's own MarshalJSON
// envelope, verbatim (see internal/daemon/handlers.go's methodGoferEvent doc)
// — enough for this file's fidelity tests to assert kind/ordering without
// decoding every kind's full payload.
type goferEventEnvelope struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
}

// waitGoferEvent blocks for c's next gofer/event notification, silently
// skipping any interleaved session/update frame (the ACP projection this
// helper's callers aren't testing), and returns its decoded envelope.
func waitGoferEvent(t *testing.T, c *wsClient) goferEventEnvelope {
	t.Helper()
	deadline := time.After(defaultWait)
	for {
		select {
		case f, ok := <-c.notifications:
			if !ok {
				t.Fatalf("connection closed waiting for a gofer/event notification")
			}
			if f.Method != "gofer/event" {
				continue
			}
			var env goferEventEnvelope
			if err := json.Unmarshal(f.Params, &env); err != nil {
				t.Fatalf("unmarshal gofer/event params: %v", err)
			}
			return env
		case <-deadline:
			t.Fatalf("timed out waiting for a gofer/event notification")
		}
	}
}

// scriptedToolTurnSession is a hand-rolled supervisor.Session (mirroring
// approvals_test.go's approvalSession) whose Prompt publishes a full turn's
// event stream directly onto its broker, standing in for the SDK's real
// loop. It exists solely to prove broadcastGoferEvent (internal/daemon/
// handlers.go) fans EVERY non-permission event kind to every attached peer —
// including tool.call.delta, which no test-scriped provider.Provider in this
// package's harness (faux is text/reasoning-only; the loop never scripts
// StreamToolCallDelta through a real tool call in these tests) can produce,
// and which ACP's session/update has no projection for at all (the
// headline loss this whole feature fixes).
type scriptedToolTurnSession struct {
	id     string
	path   string
	broker *event.Broker
}

func newScriptedToolTurnSession(id, path string) *scriptedToolTurnSession {
	return &scriptedToolTurnSession{id: id, path: path, broker: event.NewBroker(event.WithReplay(64))}
}

func (s *scriptedToolTurnSession) ID() string               { return s.id }
func (s *scriptedToolTurnSession) JournalPath() string      { return s.path }
func (s *scriptedToolTurnSession) Fold() []provider.Message { return nil }
func (s *scriptedToolTurnSession) Events() *event.Subscription {
	return s.broker.Subscribe(event.FilterAll, 64)
}
func (s *scriptedToolTurnSession) EventsLive() *event.Subscription {
	return s.broker.SubscribeLive(event.FilterAll, 64)
}
func (s *scriptedToolTurnSession) Emit(e event.Event)       { s.broker.Publish(e) }
func (s *scriptedToolTurnSession) Cost() session.CostReport { return session.CostReport{} }

// SetModel is a no-op: this fake's Prompt is a fully scripted event
// sequence that never reads a model.
func (s *scriptedToolTurnSession) SetModel(string) error { return nil }

func (s *scriptedToolTurnSession) Close() error {
	s.broker.Close()
	return nil
}

// Prompt scripts, in order: turn.started, the user pair, a tool call with TWO
// tool.call.delta fragments (streaming input JSON pieces — provider.
// StreamToolCallDelta's projection), its tool.call.finished, an assistant
// text message, then the terminal turn.finished — every non-permission event
// kind broadcastGoferEvent must fan.
func (s *scriptedToolTurnSession) Prompt(_ context.Context, text string) error {
	s.broker.Publish(event.NewTurnStarted(s.id))
	s.broker.Publish(event.NewMessageStarted(s.id, event.MessageUser))
	s.broker.Publish(event.NewMessageFinished(s.id, event.MessageUser, text))
	s.broker.Publish(event.NewToolCallStarted(s.id, "tc-1", "read_file", json.RawMessage(`{}`)))
	s.broker.Publish(event.NewToolCallDelta(s.id, "tc-1", `{"partial`))
	s.broker.Publish(event.NewToolCallDelta(s.id, "tc-1", `":"a.txt"}`))
	s.broker.Publish(event.NewToolCallFinished(s.id, "tc-1", json.RawMessage(`{"partial":"a.txt"}`), "file contents", false, nil))
	s.broker.Publish(event.NewMessageStarted(s.id, event.MessageText))
	s.broker.Publish(event.NewMessageDelta(s.id, event.MessageText, "done"))
	s.broker.Publish(event.NewMessageFinished(s.id, event.MessageText, "done"))
	s.broker.Publish(event.NewTurnFinished(s.id, "end_turn", provider.Usage{}))
	return nil
}

// newScriptedToolTurnDaemon wires a Supervisor whose sessions are
// scriptedToolTurnSessions behind an in-process daemon.
func newScriptedToolTurnDaemon(t *testing.T) (*daemon.Daemon, string) {
	t.Helper()
	root := t.TempDir()
	var nextID int64
	build := func(id, cwd string) supervisor.Session {
		path := filepath.Join(root, "sessions", session.Slugify(cwd), id+".jsonl")
		return newScriptedToolTurnSession(id, path)
	}
	cfg := supervisor.Config{
		Root: root,
		NewSession: func(_ context.Context, opts runner.Options) (supervisor.Session, error) {
			id := fmt.Sprintf("sess-%d", atomic.AddInt64(&nextID, 1))
			return build(id, opts.Cwd), nil
		},
		ResumeSession: func(_ context.Context, id string, opts runner.Options) (supervisor.Session, error) {
			return build(id, opts.Cwd), nil
		},
	}
	sup, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })
	return newTestDaemon(t, sup, "")
}

// TestPromptFanOutGoferEventFullFidelity is the daemon-side proof that
// gofer/event carries the FULL event stream — every kind ACP's session/update
// drops entirely (turn.started, tool.call.delta, turn.finished) — to EVERY
// attached peer, uniformly (methodGoferEvent's doc: no per-client
// negotiation, no origin special-casing). It asserts the exact kind sequence
// on BOTH the driving peer (c1) and a merely-attached observer (c2),
// incl. two tool.call.delta frames proving incremental tool input crosses the
// wire — the headline loss this feature fixes.
func TestPromptFanOutGoferEventFullFidelity(t *testing.T) {
	_, url := newScriptedToolTurnDaemon(t)
	c1 := dial(t, context.Background(), url, nil)
	c2 := dial(t, context.Background(), url, nil)

	cwd := t.TempDir()
	sid := newSession(t, c1, cwd)
	loadSession(t, c1, sid, cwd)
	loadSession(t, c2, sid, cwd)

	respCh := make(chan rpcFrame, 1)
	go func() {
		respCh <- c1.request(acp.MethodSessionPrompt, acp.PromptRequest{
			SessionID: sid,
			Prompt:    []acp.ContentBlock{acp.TextBlock("read a.txt")},
		})
	}()

	wantKinds := []string{
		// session.info leads the stream: the supervisor derives the session's
		// title from its first prompt and emits it at enqueue, before the turn's
		// own events (see supervisor/managed.go's enqueue).
		"session.info",
		"turn.started",
		"message.started", "message.finished",
		"tool.call.started", "tool.call.delta", "tool.call.delta", "tool.call.finished",
		"message.started", "message.delta", "message.finished",
		"turn.finished",
	}

	for _, peer := range []*wsClient{c2, c1} {
		for i, want := range wantKinds {
			env := waitGoferEvent(t, peer)
			if env.Type != want {
				t.Errorf("peer gofer/event %d: type = %q, want %q", i, env.Type, want)
			}
			if env.SessionID != sid {
				t.Errorf("peer gofer/event %d: session_id = %q, want %q", i, env.SessionID, sid)
			}
		}
	}

	final := <-respCh
	if final.Error != nil {
		t.Fatalf("session/prompt error: %+v", final.Error)
	}
}

// TestSessionLoadReplaysGoferEventHistory asserts a loading peer receives the
// session's settled history as gofer/event frames too, not just
// session/update — the M3 lossless-attach counterpart to
// TestSessionLoadReplaysHistoryBeforeResponse (conformance_test.go), which
// only checks the ACP projection. historyEvents (internal/daemon/
// history_events.go) mirrors acp.ReplayNotifications' block-walking but
// emits event.Event values with no synthesized deltas: one settled
// MessageStarted/MessageFinished pair per non-empty block, and no
// turn.started/turn.finished (a history replay carries no turn-lifecycle
// boundary of its own).
func TestSessionLoadReplaysGoferEventHistory(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")

	setup := dial(t, context.Background(), url, nil)
	cwd := t.TempDir()
	sid := newSession(t, setup, cwd)
	if resp := setup.request(acp.MethodSessionPrompt, acp.PromptRequest{
		SessionID: sid,
		Prompt:    []acp.ContentBlock{acp.TextBlock("hi")},
	}); resp.Error != nil {
		t.Fatalf("session/prompt error: %+v", resp.Error)
	}

	loader := dial(t, context.Background(), url, nil)
	loadResp := loader.request(acp.MethodSessionLoad, acp.LoadSessionRequest{SessionID: sid, Cwd: cwd})
	if loadResp.Error != nil {
		t.Fatalf("session/load error: %+v", loadResp.Error)
	}

	// By the time the request above returned, every notification the daemon
	// wrote for this load (session/update AND gofer/event alike) is already
	// sitting in loader.notifications' buffer — handleSessionLoad writes the
	// response only after every replay notification (see its ordering-
	// contract doc) — so a non-blocking drain is sufficient; no timing-based
	// wait is needed.
	var gotKinds []string
drain:
	for {
		select {
		case f, ok := <-loader.notifications:
			if !ok {
				break drain
			}
			if f.Method != "gofer/event" {
				continue
			}
			var env goferEventEnvelope
			if err := json.Unmarshal(f.Params, &env); err != nil {
				t.Fatalf("unmarshal gofer/event: %v", err)
			}
			gotKinds = append(gotKinds, env.Type)
		default:
			break drain
		}
	}

	wantKinds := []string{
		"message.started", "message.finished", // user "hi"
		"message.started", "message.finished", // assistant reasoning
		"message.started", "message.finished", // assistant text
	}
	if len(gotKinds) != len(wantKinds) {
		t.Fatalf("got %d gofer/event history frames = %v, want %d (%v)", len(gotKinds), gotKinds, len(wantKinds), wantKinds)
	}
	for i, want := range wantKinds {
		if gotKinds[i] != want {
			t.Errorf("gofer/event history frame %d: type = %q, want %q", i, gotKinds[i], want)
		}
	}
}

// TestPermissionEventsExcludedFromGoferEvent asserts permission.requested/
// permission.resolved NEVER arrive as gofer/event frames — they keep their
// existing dedicated gofer/permission_requested/resolved notifications (see
// methodGoferEvent's doc); a violation here would double-deliver them to a
// gofer client (internal/daemonbridge ignores gofer/event's permission.*
// case defensively, but the daemon must never actually send it).
func TestPermissionEventsExcludedFromGoferEvent(t *testing.T) {
	h := newApprovalHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), defaultWait)
	defer cancel()
	c := dial(t, ctx, h.url, nil)

	cwd := t.TempDir()
	newResp := c.request("session/new", map[string]any{"cwd": cwd})
	if newResp.Error != nil {
		t.Fatalf("session/new: %v", newResp.Error)
	}
	var created struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(newResp.Result, &created); err != nil {
		t.Fatalf("decode session/new: %v", err)
	}
	sid := created.SessionID

	promptDone := make(chan rpcFrame, 1)
	go func() {
		promptDone <- c.request("session/prompt", map[string]any{"sessionId": sid, "text": "rm -rf /"})
	}()

	var goferEventKinds []string
	var permID string
	deadline := time.After(defaultWait)
collect:
	for {
		select {
		case f, ok := <-c.notifications:
			if !ok {
				t.Fatal("connection closed mid-test")
			}
			switch f.Method {
			case "gofer/permission_requested":
				var pr struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(f.Params, &pr); err != nil {
					t.Fatalf("decode permission_requested: %v", err)
				}
				permID = pr.ID
				c.notify("permission.reply", map[string]any{"id": permID, "verdict": "allow"})
			case "gofer/event":
				var env goferEventEnvelope
				if err := json.Unmarshal(f.Params, &env); err != nil {
					t.Fatalf("unmarshal gofer/event: %v", err)
				}
				goferEventKinds = append(goferEventKinds, env.Type)
			}
		case resp := <-promptDone:
			if resp.Error != nil {
				t.Fatalf("session/prompt: %v", resp.Error)
			}
			break collect
		case <-deadline:
			t.Fatal("timed out waiting for session/prompt to resolve")
		}
	}

	if permID == "" {
		t.Fatal("never observed a gofer/permission_requested notification")
	}
	for _, kind := range goferEventKinds {
		if kind == "permission.requested" || kind == "permission.resolved" {
			t.Errorf("gofer/event carried a %q frame — permission.* must stay on gofer/permission_* only", kind)
		}
	}
}
