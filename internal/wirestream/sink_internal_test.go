package wirestream

// sink_internal_test.go pins the MARSHAL-ONCE contract at its source seam: the
// [EventSink] a Reconstructor is constructed with (see core.go's Option doc)
// must hand its consumer the gofer/event params EXACTLY as they arrived, byte
// for byte, alongside the event this core decoded from them.
//
// That property is what lets the M6 router forward a worker's event stream to
// its own clients without a decode→republish→re-encode round trip. The round
// trip would not merely cost CPU: routed through any lossy intermediate it
// silently sheds fields (ACP's session/update drops tool.call.finished's
// Diagnostics and all three Spill* fields entirely), so a regression would
// surface as missing data rather than as a crash. Asserting bytes.Equal here
// makes it surface as a failing test instead.
//
// These are internal tests because handleNotification is the seam: neither
// branch it drives touches r.client (RegisterFresh pre-populates the session
// state), so a bare Reconstructor wired the way [New] does — minus the client
// and the demux goroutine — drives them synchronously and deterministically,
// with no daemon, no socket, and no sleeps.

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// newSinkTestReconstructor returns a Reconstructor with sink installed and just
// enough state for handleNotification's gofer/event branch.
func newSinkTestReconstructor(sink EventSink) *Reconstructor {
	return &Reconstructor{
		sessions: make(map[string]*sessionState),
		closed:   make(chan struct{}),
		sink:     sink,
	}
}

// goferEventFrame wraps params as the gofer/event notification a worker sends.
func goferEventFrame(params string) daemon.Notification {
	return daemon.Notification{Method: methodGoferEvent, Params: json.RawMessage(params)}
}

// sinkCall is one observed [EventSink] invocation.
type sinkCall struct {
	sessionID string
	raw       json.RawMessage
	ev        event.Event
}

// spillFinishedParams is a tool.call.finished frame carrying Diagnostics and all
// three Spill* fields — precisely the corpus internal/daemonbridge's
// fidelity_test.go exercises, and precisely the fields ACP's session/update
// projection drops. If anything on the sink path ever decodes and re-encodes,
// this frame is where it shows up as a byte diff rather than as silent field
// loss.
//
// It is a hand-written literal rather than a marshaled event on purpose: the
// test must compare against bytes it CONTROLS, so a change to the event type's
// own MarshalJSON cannot quietly make both sides of the comparison agree. Note
// the discriminator is "type" (see [goferEventWire]); "kind" is the message-kind
// field, which a tool frame does not use.
const spillFinishedParams = `{"type":"tool.call.finished","session_id":"sess-spill","id":"call-7",` +
	`"name":"diag_tool","input":{"path":"main.go"},"result":"ok","is_error":false,` +
	`"diagnostics":["lint: unused variable x","vet: possible nil dereference at line 42"],` +
	`"spill_path":"/tmp/spill/call-7.txt","spill_bytes":41231,` +
	`"spill_sha256":"9f2c1e5b7a3d4f6089c2e1b5a7d3f46089c2e1b5a7d3f46089c2e1b5a7d3f460"}`

// TestEventSinkForwardsVerbatimBytes is the mechanical marshal-once assertion:
// the sink's raw must be byte-identical to the params handed to
// handleNotification.
func TestEventSinkForwardsVerbatimBytes(t *testing.T) {
	var got []sinkCall
	r := newSinkTestReconstructor(func(sid string, raw json.RawMessage, ev event.Event) {
		// Copy: raw is owned by the caller for the call's duration only (see
		// [EventSink]), and this test outlives the call.
		got = append(got, sinkCall{sessionID: sid, raw: bytes.Clone(raw), ev: ev})
	})
	const sid = "sess-spill"
	r.RegisterFresh(sid)

	r.handleNotification(goferEventFrame(spillFinishedParams))

	if len(got) != 1 {
		t.Fatalf("sink invoked %d times, want exactly 1", len(got))
	}
	call := got[0]
	if call.sessionID != sid {
		t.Errorf("sink session id = %q, want %q", call.sessionID, sid)
	}
	if !bytes.Equal(call.raw, []byte(spillFinishedParams)) {
		t.Errorf("sink raw is NOT byte-identical to the frame that arrived — something on the path re-encoded it\n got: %s\nwant: %s", call.raw, spillFinishedParams)
	}
	// The decoded half must be the same frame, handed over so a consumer driving
	// an ACP projection alongside the raw fan-out decodes nothing twice.
	fin, ok := call.ev.(event.ToolCallFinished)
	if !ok {
		t.Fatalf("sink event is %T, want event.ToolCallFinished", call.ev)
	}
	if len(fin.Diagnostics) != 2 || fin.SpillPath == "" || fin.SpillBytes == 0 || fin.SpillSHA256 == "" {
		t.Errorf("decoded event lost the ACP-dropped fields: diagnostics=%v spillPath=%q spillBytes=%d spillSHA=%q",
			fin.Diagnostics, fin.SpillPath, fin.SpillBytes, fin.SpillSHA256)
	}
}

// TestEventSinkOrderAndCoverage pins that the sink observes EVERY decodable
// gofer/event, exactly once each, in wire order — the property the router's
// single-goroutine fan-out relies on to deliver a session's stream to clients in
// the order the worker produced it.
func TestEventSinkOrderAndCoverage(t *testing.T) {
	var kinds []string
	r := newSinkTestReconstructor(func(_ string, _ json.RawMessage, ev event.Event) {
		kinds = append(kinds, string(ev.Kind()))
	})
	const sid = "sess-order"
	r.RegisterFresh(sid)

	frames := []string{
		`{"type":"turn.started","session_id":"sess-order"}`,
		`{"type":"message.started","session_id":"sess-order","kind":"assistant"}`,
		`{"type":"message.delta","session_id":"sess-order","kind":"assistant","text":"hel"}`,
		`{"type":"message.delta","session_id":"sess-order","kind":"assistant","text":"lo"}`,
		`{"type":"turn.finished","session_id":"sess-order","stop_reason":"end_turn"}`,
	}
	for _, f := range frames {
		r.handleNotification(goferEventFrame(f))
	}

	want := []string{
		string(event.KindTurnStarted),
		string(event.KindMessageStarted),
		string(event.KindMessageDelta),
		string(event.KindMessageDelta),
		string(event.KindTurnFinished),
	}
	if len(kinds) != len(want) {
		t.Fatalf("sink saw %d events %v, want %d %v", len(kinds), kinds, len(want), want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("sink event %d = %q, want %q (wire order must be preserved)", i, kinds[i], want[i])
		}
	}
}

// TestEventSinkSkipsUndecodableFrame pins that a frame this core cannot decode
// does NOT reach the sink. A consumer must never forward a frame the core
// dropped locally, or its fan-out and this core's broker would disagree about
// what the session's stream contains.
func TestEventSinkSkipsUndecodableFrame(t *testing.T) {
	var calls int
	r := newSinkTestReconstructor(func(string, json.RawMessage, event.Event) { calls++ })
	const sid = "sess-drift"
	r.RegisterFresh(sid)

	// A kind from a future gofer, and a permission kind (excluded from
	// gofer/event by contract) — both are dropped before the sink.
	r.handleNotification(goferEventFrame(`{"type":"turn.teleported","session_id":"sess-drift"}`))
	r.handleNotification(goferEventFrame(`{"type":"permission.requested","session_id":"sess-drift","id":"c1"}`))

	if calls != 0 {
		t.Errorf("sink invoked %d times for undecodable frames, want 0", calls)
	}
}

// TestNilEventSinkIsInert pins that a Reconstructor built without the option —
// every non-router consumer, e.g. internal/daemonbridge — reconstructs normally.
func TestNilEventSinkIsInert(t *testing.T) {
	r := newSinkTestReconstructor(nil)
	const sid = "sess-nosink"
	r.RegisterFresh(sid)

	sub, err := r.Subscribe(context.Background(), sid)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	r.handleNotification(goferEventFrame(`{"type":"turn.started","session_id":"sess-nosink"}`))

	select {
	case ev := <-sub.C:
		if _, ok := ev.(event.TurnStarted); !ok {
			t.Errorf("published %T, want event.TurnStarted", ev)
		}
	default:
		t.Error("no event published to the broker with a nil sink")
	}
}

// TestWithEventSinkNilOptionIgnored pins [New]'s tolerance of a nil option, so a
// caller building options conditionally cannot panic the constructor.
func TestWithEventSinkNilOptionIgnored(t *testing.T) {
	r := &Reconstructor{sessions: make(map[string]*sessionState), closed: make(chan struct{})}
	WithEventSink(nil)(r)
	if r.sink != nil {
		t.Error("WithEventSink(nil) installed a non-nil sink")
	}
}
