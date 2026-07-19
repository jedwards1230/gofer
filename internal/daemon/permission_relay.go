package daemon

import (
	"github.com/jedwards1230/agent-sdk-go/event"
)

// PermissionRelay is the daemon-side permission fan-out an ADOPTED session's
// standing watcher drives (docs/milestones/M6-process-isolation.md §7). An
// adopted session's turn runs on its worker, so — unlike a session a client
// drives via session/prompt — no [handleSessionPrompt] loop is observing its
// event stream to record permission routes and fan requests out to attached
// peers. Without that, a re-surfaced (or newly asked) permission on an adopted
// session would be invisible to daemon clients and unanswerable (its call id
// has no recorded route, so handlePermissionReply could not find its session).
//
// The M6 router bridges the gap: on adoption it starts a standing per-session
// watcher over the reconstructed broker and, for each permission event, calls
// this relay so the OUTER daemon records the route + pending payload and
// broadcasts exactly as the prompt handler would. *[Daemon] implements it; the
// router receives one via its own SetPermissionRelay and never imports daemon
// internals. Both methods are safe to call from the watcher goroutine
// concurrently with ordinary request handling: they take the daemon's own locks
// and the record/broadcast steps are the same the prompt handler uses.
type PermissionRelay interface {
	// RequestPermission records call id pe.ID → sessionID, retains the request
	// for replay-on-attach, and fans the gofer-native gofer/permission_requested
	// out to every peer attached to sessionID — exactly once even if the prompt
	// handler also observes the same event (see [Daemon.recordPermRoute]).
	RequestPermission(sessionID string, pe event.PermissionRequested)
	// ResolvePermission clears call id pe.ID's route + retained request and fans
	// the gofer-native gofer/permission_resolved out, exactly once, so route and
	// pending state never leak past a resolution.
	ResolvePermission(sessionID string, pe event.PermissionResolved)
}

// Daemon satisfies [PermissionRelay] so the M6 router can bridge an adopted
// session's reconstructed permission stream back into this daemon's fan-out.
var _ PermissionRelay = (*Daemon)(nil)

// RequestPermission implements [PermissionRelay]. It mirrors
// [handleSessionPrompt]'s PermissionRequested branch (route + pending + broadcast)
// for a session whose turn this daemon is not itself driving, using the daemon's
// base context for the fan-out (there is no per-request context here — the
// adopted turn's lifetime is the worker's, not a handler's). The gofer-native
// broadcast fires only for the FIRST observer of the request, so it never
// double-delivers when a client also drives a prompt on the same adopted session.
// It does NOT issue the spec-ACP session/request_permission request: only the
// prompt handler does that, so a re-surfaced permission on an adopted session is
// answerable by gofer clients (and by any client via a routed permission.reply);
// asking a pure-ACP peer to answer a re-surfaced request is left to Phase 3.
func (d *Daemon) RequestPermission(sessionID string, pe event.PermissionRequested) {
	params := permissionRequestedParams{
		SessionID: sessionID,
		ID:        pe.ID,
		Tool:      pe.Tool,
		Spec:      pe.Spec,
		Trace:     pe.Trace,
	}
	if d.recordPermRoute(pe.ID, sessionID) {
		d.recordPendingPerm(pe.ID, params)
		// broadcastPermission bounds the fan-out itself off the daemon's base
		// context (see [relayWriteTimeout]), which is what this path needs: it runs
		// on the router's per-worker demuxer goroutine, so an unbounded write to a
		// stalled peer would wedge that whole session's control plane. The route
		// and pending payload are recorded ABOVE the broadcast, so a client that
		// misses the timed-out notification still gets the request on its next
		// attach via the replay-on-attach path.
		d.broadcastPermission(sessionID, methodGoferPermissionRequested, params)
	}
}

// ResolvePermission implements [PermissionRelay]. It mirrors
// [handleSessionPrompt]'s PermissionResolved branch (clear route + pending, then
// broadcast) so an adopted session's route/pending state is released when its
// gate resolves. The resolution broadcast fires only for the observer that
// actually cleared the route, so it never double-delivers.
func (d *Daemon) ResolvePermission(sessionID string, pe event.PermissionResolved) {
	d.clearPermRoute(pe.ID)
	if d.clearPendingPerm(pe.ID) {
		// Bounded by broadcastPermission — same goroutine, same wedge (see
		// [relayWriteTimeout]).
		d.broadcastPermission(sessionID, methodGoferPermissionResolved, permissionResolvedParams{
			SessionID: sessionID,
			ID:        pe.ID,
			Verdict:   string(pe.Verdict),
			Rule:      pe.Rule,
		})
	}
}
