package daemonbridge

// reconstruct_internal_test.go covers handleNotification's
// gofer/permission_requested and gofer/permission_resolved reconstruction
// (see reconstruct.go) directly, without a real *daemon.Client or a live
// daemon: neither branch touches s.client (registerFresh pre-populates the
// session's reconstruction state so handleNotification's s.session lookup
// never falls to the loadHistory path that would need one), so a bare
// Supervisor value wired up the same way [New] does — minus the client and
// the demux goroutine — is enough to drive them synchronously.
// permission_test.go's (package daemonbridge_test)
// TestReplySendsPermissionReplyNotification is this package's other half of
// the M3 approvals-relay contract: the outbound "permission.reply"
// notification, over a real (if minimal) WebSocket connection.

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// newReconstructTestSupervisor returns a Supervisor with just enough state
// for handleNotification's permission branches: a session map and a closed
// channel selectable in [Supervisor.session]'s guard, no *daemon.Client.
func newReconstructTestSupervisor() *Supervisor {
	return &Supervisor{
		sessions: make(map[string]*sessionState),
		closed:   make(chan struct{}),
	}
}

// TestHandleNotificationReconstructsPermissionRequested asserts a
// "permission.requested" notification, in event.PermissionRequested's own
// MarshalJSON shape, reconstructs into an event.PermissionRequested on the
// named session's broker.
func TestHandleNotificationReconstructsPermissionRequested(t *testing.T) {
	s := newReconstructTestSupervisor()
	const sid = "sess-1"
	s.registerFresh(sid)

	sub, err := s.Subscribe(context.Background(), sid)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	params := json.RawMessage(`{"sessionId":"sess-1","id":"perm-1","tool":"bash","spec":{"cmd":"rm -rf /tmp/x"},"trace":["no rule"]}`)
	s.handleNotification(daemon.Notification{Method: methodGoferPermissionRequested, Params: params})

	select {
	case ev := <-sub.C:
		pr, ok := ev.(event.PermissionRequested)
		if !ok {
			t.Fatalf("got %T, want event.PermissionRequested", ev)
		}
		if pr.ID != "perm-1" || pr.Tool != "bash" {
			t.Errorf("PermissionRequested = %+v, want ID=perm-1 Tool=bash", pr)
		}
		if pr.Spec["cmd"] != "rm -rf /tmp/x" {
			t.Errorf("PermissionRequested.Spec = %+v, want cmd=rm -rf /tmp/x", pr.Spec)
		}
		if len(pr.Trace) != 1 || pr.Trace[0] != "no rule" {
			t.Errorf("PermissionRequested.Trace = %+v, want [no rule]", pr.Trace)
		}
		if pr.SessionID() != sid {
			t.Errorf("SessionID() = %q, want %q", pr.SessionID(), sid)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the reconstructed PermissionRequested")
	}
}

// TestHandleNotificationReconstructsPermissionResolved asserts a
// "permission.resolved" notification reconstructs into an
// event.PermissionResolved.
func TestHandleNotificationReconstructsPermissionResolved(t *testing.T) {
	s := newReconstructTestSupervisor()
	const sid = "sess-1"
	s.registerFresh(sid)

	sub, err := s.Subscribe(context.Background(), sid)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	params := json.RawMessage(`{"sessionId":"sess-1","id":"perm-1","verdict":"deny","rule":"deny bash rm"}`)
	s.handleNotification(daemon.Notification{Method: methodGoferPermissionResolved, Params: params})

	select {
	case ev := <-sub.C:
		pr, ok := ev.(event.PermissionResolved)
		if !ok {
			t.Fatalf("got %T, want event.PermissionResolved", ev)
		}
		if pr.ID != "perm-1" || pr.Verdict != event.VerdictDeny || pr.Rule != "deny bash rm" {
			t.Errorf("PermissionResolved = %+v, want ID=perm-1 Verdict=deny Rule=%q", pr, "deny bash rm")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the reconstructed PermissionResolved")
	}
}

// stripSeqTime marshals e (invoking its own MarshalJSON) and returns its
// envelope as a generic map with the "seq"/"time" fields removed — the ONLY
// fields a gofer/event round trip doesn't preserve byte-for-byte (event.New*
// always builds seq=0/time=zero; [event.Broker.Publish] reassigns REAL
// seq/time locally — see reconstruct.go's package doc, "seq/time note": this
// is by design, not a fidelity gap). A field-for-field fidelity comparison
// strips them before comparing two envelopes.
func stripSeqTime(t *testing.T, e event.Event) map[string]any {
	t.Helper()
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal %T: %v", e, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal %T: %v", e, err)
	}
	delete(m, "seq")
	delete(m, "time")
	return m
}

// TestHandleNotificationReplaysGoferEventKinds is the unit-level fidelity
// proof for handleGoferEvent's dispatch table: for every non-permission
// [event.Event] kind, marshal a source event built with event.New* (its own
// MarshalJSON — the exact bytes the daemon's broadcastGoferEvent sends),
// push it through s.handleNotification as a gofer/event notification (the
// SAME internal seam TestHandleNotificationReconstructsPermissionRequested
// uses — no real daemon.Client needed), and assert the event the broker
// actually publishes is field-for-field equal to the source, ignoring
// seq/time (see stripSeqTime). tool.call.delta and a tool.call.finished
// carrying Diagnostics + all three Spill* fields are the two cases the OLD
// ACP-projection reconstruction could never round-trip at all — the entire
// point of this feature.
func TestHandleNotificationReplaysGoferEventKinds(t *testing.T) {
	const sid = "sess-1"

	cases := []event.Event{
		event.NewSessionCreated(sid),
		event.NewSessionResumed(sid),
		event.NewSessionForked(sid),
		event.NewSessionCompacted(sid),
		event.NewSessionKilled(sid),
		event.NewSessionArchived(sid),
		event.NewSessionError(sid, "boom", true),
		event.NewTurnStarted(sid),
		event.NewTurnFinishedCost(sid, "end_turn",
			provider.Usage{InputTokens: 100, OutputTokens: 42, CacheReadTokens: 7, CacheWriteTokens: 3},
			&provider.Cost{USD: 0.0123, InputUSD: 0.01, OutputUSD: 0.0023}),
		event.NewTurnFinished(sid, "tool_use", provider.Usage{InputTokens: 5}), // no cost: nil ok (spec table note)
		event.NewMessageStarted(sid, event.MessageReasoning),
		event.NewMessageDelta(sid, event.MessageText, "a fragment of streamed text"),
		event.NewMessageFinishedMeta(sid, event.MessageReasoning, "the settled reasoning content",
			map[string]string{"anthropic.signature": "sig-abc123"}),
		event.NewToolCallStarted(sid, "tc-1", "bash", json.RawMessage(`{"command":"ls -la"}`)),
		// tool.call.delta: a fragment of the streaming INPUT (partial JSON
		// arguments) — entirely dropped by ACP's session/update (no
		// incremental-tool concept). This is the headline loss.
		event.NewToolCallDelta(sid, "tc-1", `{"comm`),
		// tool.call.finished with Diagnostics + all three Spill* fields —
		// also entirely dropped by the ACP projection.
		event.NewToolCallFinishedSpill(sid, "tc-1", "bounded excerpt of the output",
			true, []string{"lint: unused variable x", "vet: possible nil deref"},
			"sessions/proj/sess-1/calls/tc-1.log", 123456, "deadbeefcafef00d"),
	}

	for _, src := range cases {
		t.Run(src.Kind(), func(t *testing.T) {
			s := newReconstructTestSupervisor()
			s.registerFresh(sid)
			sub, err := s.Subscribe(context.Background(), sid)
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			defer sub.Close()

			raw, err := json.Marshal(src)
			if err != nil {
				t.Fatalf("marshal source %T: %v", src, err)
			}
			s.handleNotification(daemon.Notification{Method: methodGoferEvent, Params: raw})

			select {
			case dst := <-sub.C:
				if dst.Kind() != src.Kind() {
					t.Fatalf("Kind() = %q, want %q", dst.Kind(), src.Kind())
				}
				if dst.SessionID() != sid {
					t.Errorf("SessionID() = %q, want %q", dst.SessionID(), sid)
				}
				want := stripSeqTime(t, src)
				got := stripSeqTime(t, dst)
				if !reflect.DeepEqual(got, want) {
					t.Errorf("replayed payload = %+v, want %+v", got, want)
				}
			case <-time.After(time.Second):
				t.Fatalf("timed out waiting for the replayed %s", src.Kind())
			}
		})
	}
}

// TestHandleNotificationIgnoresPermissionKindsViaGoferEvent asserts that a
// permission.requested/permission.resolved envelope arriving via the
// gofer/event method (which should never happen — see methodGoferEvent's
// doc — but is defensively guarded) is dropped rather than mis-dispatched:
// handleGoferEvent's dispatch table has no case for either kind, so it falls
// to the default branch and returns without publishing anything.
func TestHandleNotificationIgnoresPermissionKindsViaGoferEvent(t *testing.T) {
	const sid = "sess-1"
	s := newReconstructTestSupervisor()
	s.registerFresh(sid)
	sub, err := s.Subscribe(context.Background(), sid)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	for _, raw := range []json.RawMessage{
		[]byte(`{"type":"permission.requested","session_id":"sess-1","id":"perm-1","tool":"bash"}`),
		[]byte(`{"type":"permission.resolved","session_id":"sess-1","id":"perm-1","verdict":"allow"}`),
	} {
		s.handleNotification(daemon.Notification{Method: methodGoferEvent, Params: raw})
	}

	select {
	case ev := <-sub.C:
		t.Fatalf("got %+v, want nothing published for a permission.* gofer/event envelope", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestHandleNotificationDropsMalformedPermissionRequested asserts a
// permission.requested notification with no session_id (or invalid JSON) is
// dropped rather than panicking or creating a stray session entry — the same
// tolerance handleNotification's ACP session/update path already has for a
// protocol drift.
func TestHandleNotificationDropsMalformedPermissionRequested(t *testing.T) {
	s := newReconstructTestSupervisor()
	s.handleNotification(daemon.Notification{Method: methodGoferPermissionRequested, Params: json.RawMessage(`{}`)})
	s.handleNotification(daemon.Notification{Method: methodGoferPermissionRequested, Params: json.RawMessage(`not json`)})

	s.mu.Lock()
	n := len(s.sessions)
	s.mu.Unlock()
	if n != 0 {
		t.Errorf("sessions = %d, want 0 (malformed notification should not register a session)", n)
	}
}
