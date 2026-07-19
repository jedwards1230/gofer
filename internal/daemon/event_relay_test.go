package daemon_test

// event_relay_test.go covers the M6 event relay (event_relay.go): the daemon-side
// fan-out an M6 router drives for a session whose turn this daemon is not itself
// hosting, and — the part that is easy to get wrong — its DOUBLE-DELIVERY GUARD.
//
// The guard exists because handleSessionPrompt ALREADY fans every event of the
// turn it drives out to every attached peer, off its own subscription. For a
// session a client drives through a router, the router's sink observes those
// same events, so without a guard every peer would receive each event TWICE.
//
// Every assertion here is deterministic. Suppression in particular is asserted
// WITHOUT a sleep: a suppressed frame is followed by a distinguishable allowed
// frame, and the peer's next delivery must be the second one. If the first had
// leaked, it would arrive first and fail the assertion. Absence is proven by
// ordering, not by waiting.

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/provider/faux"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// relayFrame builds a gofer/event params blob tagged with marker, so a test can
// tell which relayed frame a peer received. It is a message.delta because that
// kind also projects to an ACP session/update, exercising both relay methods.
func relayFrame(sid, marker string) json.RawMessage {
	return json.RawMessage(`{"type":"message.delta","session_id":"` + sid +
		`","kind":"assistant","text":"` + marker + `"}`)
}

// waitRawGoferEvent blocks for c's next gofer/event notification and returns its
// params UNDECODED. It is [waitGoferEvent]'s raw sibling: the relay's whole
// contract is that the bytes pass through unchanged, so the tests here must
// compare bytes rather than a decoded envelope that would hide a re-encode.
//
// It reads c.notifications directly because waitNotification deliberately SKIPS
// gofer/event frames, which are exactly what this file is about.
func waitRawGoferEvent(t *testing.T, c *wsClient) json.RawMessage {
	t.Helper()
	deadline := time.After(defaultWait)
	for {
		select {
		case f, ok := <-c.notifications:
			if !ok {
				t.Fatal("connection closed waiting for a gofer/event")
			}
			if f.Method != "gofer/event" {
				continue
			}
			return f.Params
		case <-deadline:
			t.Fatal("timed out waiting for a gofer/event notification")
		}
	}
}

// eventText decodes the "text" field of a relayed message.delta frame — the
// marker relayFrame stamped.
func eventText(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var p struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode relayed frame: %v", err)
	}
	return p.Text
}

// TestBroadcastRawEventReachesAttachedPeer is the relay's reason to exist: a
// turn running on a worker has no prompt handler in this daemon fanning its
// events out, so without the relay a client attached to such a session watches a
// silent stream until the turn ends.
func TestBroadcastRawEventReachesAttachedPeer(t *testing.T) {
	sup := newTestSupervisor(t, func() provider.Provider { return faux.New(faux.Default()) })
	d, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	cwd := t.TempDir()
	sid := newSession(t, c, cwd)
	loadSession(t, c, sid, cwd)

	sent := relayFrame(sid, "relayed")
	d.BroadcastRawEvent(sid, sent)

	got := waitRawGoferEvent(t, c)
	// Byte-identity, not equivalence: the relay must write the worker's frame
	// through UNCHANGED. A decode/re-encode round trip would still "work" here
	// while silently shedding fields ACP's projection drops, so equality of the
	// bytes is the assertion that actually pins marshal-once at this hop.
	if string(got) != string(sent) {
		t.Errorf("relayed frame is not byte-identical to what the worker emitted\n got: %s\nwant: %s", got, sent)
	}
}

// TestBroadcastRawEventSuppressedDuringPrompt is the double-delivery guard. A
// relayed frame arriving while a prompt handler is marked must be DROPPED — that
// handler is already fanning the same events out itself.
func TestBroadcastRawEventSuppressedDuringPrompt(t *testing.T) {
	sup := newTestSupervisor(t, func() provider.Provider { return faux.New(faux.Default()) })
	d, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	cwd := t.TempDir()
	sid := newSession(t, c, cwd)
	loadSession(t, c, sid, cwd)

	// While the mark is set the relay stands down...
	d.BeginPromptHandler(sid)
	if !d.PromptHandlerActive(sid) {
		t.Fatal("prompt handler mark did not take")
	}
	d.BroadcastRawEvent(sid, relayFrame(sid, "suppressed"))
	d.EndPromptHandler(sid)

	// ...and once released it delivers again. The peer's NEXT gofer/event must be
	// the allowed one: if the suppressed frame had leaked it would necessarily
	// arrive first (same connection, wire order), so this ordering proves the
	// drop with no sleep and no negative timeout.
	d.BroadcastRawEvent(sid, relayFrame(sid, "allowed"))
	if got := eventText(t, waitRawGoferEvent(t, c)); got != "allowed" {
		t.Errorf("peer received a %q frame; the guard did not suppress the relayed frame during the prompt", got)
	}
}

// TestPromptHandlerMarkIsCounted pins that the mark is a COUNTER, not a flag.
// Nothing in the wire contract forbids two concurrent session/prompt calls for
// one session, and a misbehaving client is not a reason to un-suppress the relay
// — so the mark must clear only when the LAST handler leaves.
func TestPromptHandlerMarkIsCounted(t *testing.T) {
	sup := newTestSupervisor(t, func() provider.Provider { return faux.New(faux.Default()) })
	d, _ := newTestDaemon(t, sup, "")
	const sid = "sess-counted"

	d.BeginPromptHandler(sid)
	d.BeginPromptHandler(sid)
	d.EndPromptHandler(sid)
	if !d.PromptHandlerActive(sid) {
		t.Error("mark cleared while a second prompt handler was still running")
	}
	d.EndPromptHandler(sid)
	if d.PromptHandlerActive(sid) {
		t.Error("mark still set after the last prompt handler left")
	}
}

// TestClientPromptedSessionDeliversEachEventOnce is the guard's end-to-end
// statement, and the regression this whole mechanism exists to prevent: a
// session a client drives via session/prompt delivers each event to an attached
// observer EXACTLY once, even though the relay is wired and observing the same
// stream.
//
// The session used here RELAYS INLINE: as it publishes each event of the turn it
// also calls BroadcastRawEvent for that event, on the same goroutine, before
// moving on. That is a faithful model of the router's wirestream sink, which
// runs on the reconstruction demuxer goroutine synchronously with event
// production (see wirestream's handleGoferEvent) — and NOT of a lagging
// subscriber, which could drain past the prompt handler's exit and so past the
// mark, exaggerating a one-event boundary window into a total failure.
//
// Without the inline relay the test would be vacuous: nothing else would be
// broadcasting, so "exactly once" would hold trivially. With it, removing the
// guard makes every event of the turn arrive twice.
//
// The observer merely attaches (it does not drive), so it sees the turn purely
// through the daemon's fan-out. Counting its gofer/event frames by (kind, seq) —
// the envelope's own identity — makes a double delivery show up as a count of 2
// rather than as a vague "too many events".
func TestClientPromptedSessionDeliversEachEventOnce(t *testing.T) {
	d, url := newInlineRelayDaemon(t)

	driver := dial(t, context.Background(), url, nil)
	cwd := t.TempDir()
	sid := newSession(t, driver, cwd)
	// session/new does NOT attach the creating peer to the fan-out set; only
	// session/load does. Both peers attach explicitly so the turn below is fanned
	// out to a real two-peer set, which is the shape the guard has to get right.
	loadSession(t, driver, sid, cwd)

	observer := dial(t, context.Background(), url, nil)
	loadSession(t, observer, sid, cwd)
	waitForPeerCount(t, d, sid, 2)
	setInlineRelayTarget(d)

	// Drive a full turn to completion. The response lands only after the turn's
	// final event has been fanned out, which is what makes the drain below
	// complete rather than merely likely-complete.
	resp := driver.request(acp.MethodSessionPrompt, acp.PromptRequest{
		SessionID: sid,
		Prompt:    []acp.ContentBlock{acp.TextBlock("hi")},
	})
	if resp.Error != nil {
		t.Fatalf("session/prompt error: %+v", resp.Error)
	}

	// Now relay a frame and wait for it at the observer. Because the prompt has
	// returned, the mark is released and this frame IS delivered — and because it
	// travels the same connection in wire order, receiving it proves every event
	// of the turn has already been delivered. That is the drain barrier: no sleep.
	const sentinel = "sentinel-after-turn"
	d.BroadcastRawEvent(sid, relayFrame(sid, sentinel))

	seen := map[string]int{}
	for {
		raw := waitRawGoferEvent(t, observer)
		var env struct {
			Type string `json:"type"`
			Seq  int64  `json:"seq"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("decode gofer/event: %v", err)
		}
		if env.Text == sentinel {
			break
		}
		seen[env.Type+"#"+strconv.FormatInt(env.Seq, 10)]++
	}

	if len(seen) == 0 {
		t.Fatal("observer received no events for the driven turn")
	}
	for key, n := range seen {
		if n != 1 {
			t.Errorf("event %s delivered %d times, want exactly 1 (the relay double-delivered a client-driven turn)", key, n)
		}
	}
	// A turn must at minimum have produced its terminal event.
	var sawTerminal bool
	for key := range seen {
		if strings.HasPrefix(key, string(event.KindTurnFinished)+"#") {
			sawTerminal = true
		}
	}
	if !sawTerminal {
		t.Errorf("observer never saw a %s; the fan-out is not carrying the turn: %v", event.KindTurnFinished, seen)
	}
}

// inlineRelayTarget is the daemon an [inlineRelaySession] relays into. It is a
// package-level handle rather than a constructor argument because the session is
// built by the supervisor's NewSession callback, which runs inside daemon.New's
// own call graph — the daemon does not exist yet when the callback is defined.
// Exactly the same chicken-and-egg the router solves with SetEventRelay.
var inlineRelayTarget struct {
	mu sync.Mutex
	d  *daemon.Daemon
}

func setInlineRelayTarget(d *daemon.Daemon) {
	inlineRelayTarget.mu.Lock()
	inlineRelayTarget.d = d
	inlineRelayTarget.mu.Unlock()
}

func relayTarget() *daemon.Daemon {
	inlineRelayTarget.mu.Lock()
	defer inlineRelayTarget.mu.Unlock()
	return inlineRelayTarget.d
}

// inlineRelaySession is a hand-rolled supervisor.Session (mirroring
// fanout_test.go's scriptedToolTurnSession) whose Prompt publishes a scripted
// turn AND relays each event through [daemon.Daemon.BroadcastRawEvent] inline,
// on the publishing goroutine, before publishing the next one.
//
// That inline position is the whole point: it is where an M6 router's wirestream
// sink sits relative to event production, so the guard is exercised under the
// timing it actually has to survive.
type inlineRelaySession struct {
	id     string
	path   string
	broker *event.Broker
}

func newInlineRelaySession(id, path string) *inlineRelaySession {
	return &inlineRelaySession{id: id, path: path, broker: event.NewBroker(event.WithReplay(64))}
}

func (s *inlineRelaySession) ID() string               { return s.id }
func (s *inlineRelaySession) JournalPath() string      { return s.path }
func (s *inlineRelaySession) Fold() []provider.Message { return nil }
func (s *inlineRelaySession) Events() *event.Subscription {
	return s.broker.Subscribe(event.FilterAll, 64)
}
func (s *inlineRelaySession) EventsLive() *event.Subscription {
	return s.broker.SubscribeLive(event.FilterAll, 64)
}
func (s *inlineRelaySession) Emit(e event.Event)       { s.broker.Publish(e) }
func (s *inlineRelaySession) Cost() session.CostReport { return session.CostReport{} }
func (s *inlineRelaySession) SetModel(string) error    { return nil }
func (s *inlineRelaySession) Close() error             { s.broker.Close(); return nil }

// emit publishes e locally and relays it, in the sink's order: the router's sink
// runs BEFORE the local publish (see wirestream's handleGoferEvent), so the
// relay call comes first here too.
func (s *inlineRelaySession) emit(e event.Event) {
	if d := relayTarget(); d != nil {
		if raw, err := json.Marshal(e); err == nil {
			d.BroadcastRawEvent(s.id, raw)
		}
	}
	s.broker.Publish(e)
}

func (s *inlineRelaySession) Prompt(_ context.Context, text string) error {
	s.emit(event.NewTurnStarted(s.id))
	s.emit(event.NewMessageStarted(s.id, event.MessageUser))
	s.emit(event.NewMessageFinished(s.id, event.MessageUser, text))
	s.emit(event.NewMessageStarted(s.id, event.MessageText))
	s.emit(event.NewMessageDelta(s.id, event.MessageText, "done"))
	s.emit(event.NewMessageFinished(s.id, event.MessageText, "done"))
	s.emit(event.NewTurnFinished(s.id, "end_turn", provider.Usage{}))
	return nil
}

// newInlineRelayDaemon wires a supervisor whose sessions are
// inlineRelaySessions behind an in-process daemon.
func newInlineRelayDaemon(t *testing.T) (*daemon.Daemon, string) {
	t.Helper()
	root := t.TempDir()
	var nextID int64
	build := func(id, cwd string) supervisor.Session {
		return newInlineRelaySession(id, filepath.Join(root, "sessions", session.Slugify(cwd), id+".jsonl"))
	}
	sup, err := supervisor.New(supervisor.Config{
		Root: root,
		NewSession: func(_ context.Context, opts runner.Options) (supervisor.Session, error) {
			return build(fmt.Sprintf("sess-%d", atomic.AddInt64(&nextID, 1)), opts.Cwd), nil
		},
		ResumeSession: func(_ context.Context, id string, opts runner.Options) (supervisor.Session, error) {
			return build(id, opts.Cwd), nil
		},
	})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close(); setInlineRelayTarget(nil) })
	return newTestDaemon(t, sup, "")
}

// Daemon must satisfy the relay interface the router depends on; a signature
// drift should fail here rather than at the router's own wiring.
var _ daemon.EventRelay = (*daemon.Daemon)(nil)
