package daemon

import "context"

// MethodGoferEvent is the wire method the daemon relays a raw event under,
// exported for the external test package. A test that drains a peer's
// notification stream UNTIL a gofer/event sentinel arrives is silently turned
// into an infinite drain if that method is renamed and the test still matches
// the old string literal — the drain would then run until the test context is
// cancelled and report a phantom teardown. Matching this constant makes such a
// rename a compile error instead. See awaitSentinel.
const MethodGoferEvent = methodGoferEvent

// PeerHandle is an opaque reference to one peer attached to a session. It
// exists because [peer] is unexported and this package's tests live in the
// external package daemon_test, which still needs to name a specific peer as
// the ORIGIN of a fan-out.
type PeerHandle struct{ p *peer }

// AttachedPeers returns a handle to every peer attached to sessionID. The order
// is unspecified — the registry is a set (see [Daemon.peersForSession]).
func (d *Daemon) AttachedPeers(sessionID string) []PeerHandle {
	prs := d.peersForSession(sessionID)
	out := make([]PeerHandle, 0, len(prs))
	for _, pr := range prs {
		out = append(out, PeerHandle{p: pr})
	}
	return out
}

// BroadcastUpdate drives the unexported [Daemon.broadcastUpdate] with an
// explicit origin peer and caller context, reporting whether the ORIGIN write
// failed. It exposes the origin/non-origin write-context split directly, which
// is otherwise only reachable through a real in-flight session/prompt — and the
// interesting case (a fan-out running while the origin's context is already
// cancelled) is a race window no end-to-end test can hit deterministically. See
// TestFanOutNonOriginWriteSurvivesOriginContextCancel.
func (d *Daemon) BroadcastUpdate(ctx context.Context, sessionID string, origin PeerHandle, notif any) (originWriteFailed bool) {
	return d.broadcastUpdate(ctx, sessionID, origin.p, notif, false) != nil
}

// PeersForSessionCount reports how many peers are currently attached to
// sessionID in the fan-out registry. It is a test-only accessor — the daemon's
// tests live in the external package daemon_test and cannot reach
// [Daemon.sessionPeers] directly — used to assert attach-on-load/detach-on-close
// bookkeeping (see the fan-out tests).
func (d *Daemon) PeersForSessionCount(sessionID string) int {
	return len(d.peersForSession(sessionID))
}

// BeginPromptHandler / EndPromptHandler / PromptHandlerActive expose the M6
// event relay's DOUBLE-DELIVERY GUARD to the external test package. The guard is
// otherwise only observable through a real in-flight session/prompt, which makes
// the interesting case — a relayed broadcast arriving WHILE a prompt handler is
// running — awkward to hit deterministically. Exposing the marker lets a test
// drive the guard's two states directly and assert delivery/suppression with no
// sleeps. See [Daemon.BroadcastRawEvent]'s guard doc.
func (d *Daemon) BeginPromptHandler(sessionID string) { d.beginPromptHandler(sessionID) }

// EndPromptHandler releases one [Daemon.BeginPromptHandler] mark.
func (d *Daemon) EndPromptHandler(sessionID string) { d.endPromptHandler(sessionID) }

// PromptHandlerActive reports whether the relay is currently standing down for
// sessionID.
func (d *Daemon) PromptHandlerActive(sessionID string) bool {
	return d.promptHandlerActive(sessionID)
}

// OutstandingPermissionRequestCount reports how many permission call ids still
// have a live session/request_permission fan-out (their cancel func has not yet
// fired). It is a test-only accessor used to assert that resolving a permission
// by any path retracts the outstanding requests at every other peer, leaving no
// daemon-side waiter dangling.
func (d *Daemon) OutstandingPermissionRequestCount() int {
	d.permReqMu.Lock()
	defer d.permReqMu.Unlock()
	return len(d.permReqCancels)
}
