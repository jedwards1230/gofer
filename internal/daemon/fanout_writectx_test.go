package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"
)

// fanOutProbeWrites is how many times the probe below repeats its fan-out.
//
// It is NOT a retry of a flaky assertion — every repetition must succeed. It
// exists to defeat a race INSIDE coder/websocket that would otherwise let the
// bug slip through: writeFrame arms the close-on-cancel AfterFunc and then
// disarms it with `defer c.clearWriteTimeout()`, and it acquires the frame lock
// with a select that has both ctx.Done and the lock ready. So a single write
// under an already-cancelled context is two coin flips — it may succeed, and it
// may leave the connection open. Repeating makes the escape probability
// 2^-fanOutProbeWrites while keeping correct code perfectly deterministic: with
// the write context owned rather than borrowed, all N land and the connection
// stays open, every run.
const fanOutProbeWrites = 64

// awaitSentinel drains c's notifications until the sentinel gofer/event frame
// arrives, reporting how many session/update frames were seen before it and
// whether the sentinel arrived at all (a closed connection ends the drain
// instead).
//
// Both exits are deterministic — no sleep, no retry. WebSocket frames are
// ordered per connection, so a session/update written to this peer BEFORE the
// sentinel necessarily arrives before it; and a peer whose connection was torn
// down reaches the closed-channel exit instead. Either way the answer is settled
// by the time this returns.
//
// alive is the load-bearing half: the sentinel is written under the DAEMON's
// context to every attached peer, so it reaches any connection that is still
// open. Failing to see it means this peer's connection was destroyed.
func awaitSentinel(t *testing.T, c *wsClient) (updates int, alive bool) {
	t.Helper()
	for {
		select {
		case f, ok := <-c.notifications:
			if !ok {
				return updates, false // connection torn down
			}
			switch f.Method {
			case acp.MethodSessionUpdate:
				updates++
			case "gofer/event":
				return updates, true
			}
		case <-c.ctx.Done():
			t.Fatal("test context cancelled waiting for the sentinel frame")
		}
	}
}

// awaitConnClosed blocks until c's connection is torn down (its notification
// stream closes), reporting false if it is still open after defaultWait. The
// deadline only fires on failure: a write under an already-cancelled context has
// coder/websocket's close AfterFunc already armed, so the close is certain.
func awaitConnClosed(t *testing.T, c *wsClient) bool {
	t.Helper()
	deadline := time.After(defaultWait)
	for {
		select {
		case _, ok := <-c.notifications:
			if !ok {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// TestFanOutNonOriginWriteSurvivesOriginContextCancel pins the availability
// property behind the daemon's write-context split: cancelling ONE peer's
// context must not close ANOTHER peer's connection.
//
// It matters because coder/websocket's Write registers a context.AfterFunc that
// closes the WHOLE connection when the write's context is cancelled. A fan-out
// that wrote to every peer under the driving peer's request context therefore
// let a client disconnecting mid-turn tear down a different, healthy client's
// connection — in M6's geometry frequently a router's link to a live worker,
// which is how a running session gets marked offline.
//
// The probe drives broadcastUpdate with an origin context that is ALREADY
// cancelled — the race window's outcome, made deterministic — and asserts BOTH
// halves of the split at once:
//
//   - the observer's connection survives, receives the update, and is still
//     usable afterwards (the fix), and
//   - the ORIGIN's connection is torn down by that same cancellation, which is
//     the direct evidence that its write still runs under the turn's context
//     (the deliberate origin semantics, unchanged) — and, in the same breath, a
//     live demonstration of the close-on-cancel behaviour the observer is now
//     insulated from.
//
// Note the survival assertions rather than merely a delivery one: with the
// borrowed context restored, the close is asynchronous and can lose the race
// with the write itself, so the frame sometimes still lands on a connection that
// is already doomed — and the origin's write correspondingly sometimes reports
// success. Whether each connection is alive AFTER the fan-out is the stable
// property; per-write success is not.
func TestFanOutNonOriginWriteSurvivesOriginContextCancel(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	d, url := newTestDaemon(t, sup, "")

	ctx := context.Background()
	origin := dial(t, ctx, url, nil)   // the peer whose context gets cancelled
	observer := dial(t, ctx, url, nil) // the innocent bystander

	cwd := t.TempDir()
	sid := newSession(t, origin, cwd)

	// Attach the origin ALONE first, so the single handle in the registry is
	// unambiguously its peer — that is how this test names the origin without a
	// peer-identity accessor (AttachedPeers' order is unspecified). session/load
	// registers the caller before it returns, so no wait is needed here.
	loadSession(t, origin, sid, cwd)
	originPeers := d.AttachedPeers(sid)
	if len(originPeers) != 1 {
		t.Fatalf("attached peers after the origin's load = %d, want 1", len(originPeers))
	}
	originPeer := originPeers[0]

	loadSession(t, observer, sid, cwd)
	waitForPeerCount(t, d, sid, 2)

	notif, ok := acp.ToSessionUpdate(sid, event.NewMessageDelta(sid, event.MessageText, "survivor"))
	if !ok {
		t.Fatal("message.delta has no session/update projection")
	}

	// The origin's context, already cancelled: the state it is in the instant a
	// client hangs up mid-turn while the fan-out for the current event is in
	// flight.
	originCtx, cancelOrigin := context.WithCancel(ctx)
	cancelOrigin()

	for range fanOutProbeWrites {
		d.BroadcastUpdate(originCtx, sid, originPeer, notif)
	}

	// Close the observation window with a frame written under the DAEMON's
	// context, so the observer has a definite "nothing more is coming" marker to
	// stop at instead of a timeout — and so its absence proves a torn-down
	// connection.
	d.BroadcastRawEvent(sid, json.RawMessage(fmt.Sprintf(`{"type":"turn.started","session_id":%q}`, sid)))

	updates, alive := awaitSentinel(t, observer)
	if !alive {
		t.Fatal("the observer's connection was torn down by the ORIGIN peer's context cancellation: a non-origin fan-out write borrowed the origin's context")
	}
	if updates != fanOutProbeWrites {
		t.Errorf("observer received %d fanned session/update frames, want %d", updates, fanOutProbeWrites)
	}

	// A round trip proves the connection is not merely unclosed but still
	// serving: the non-origin write armed no close-on-cancel behaviour on it.
	if resp := observer.request("gofer/roster", nil); resp.Error != nil {
		t.Errorf("observer's connection unusable after the origin's context was cancelled: %+v", resp.Error)
	}

	// The origin's own connection, by contrast, MUST go down: its write ran
	// under the cancelled context, so coder/websocket's close AfterFunc fires.
	// This is what keeps the origin half of the split honest — if the origin
	// write ever stopped using the turn's context, its connection would survive
	// here and the fatal-write rule in handleSessionPrompt would be silently
	// dead.
	if !awaitConnClosed(t, origin) {
		t.Error("the origin's connection survived its own context's cancellation: the origin write no longer runs under the turn's context, so handleSessionPrompt's fatal-write rule can no longer fire")
	}
}
