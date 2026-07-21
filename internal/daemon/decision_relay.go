package daemon

import (
	"github.com/jedwards1230/agent-sdk-go/acp"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// Daemon satisfies [supervisor.DecisionRelay], the seam the hosted supervisor's
// standing per-session watcher drives so a structured decision can leave this
// process. The interface is declared at its consumer (internal/supervisor)
// because this package imports that one; this assertion is what turns a
// signature drift into a build failure here rather than a runtime surprise at
// the SetDecisionRelay call site in cmd/gofer.
//
// # Why a decision needs a standing watcher when a permission does not
//
// [handleSessionPrompt] observes every permission INLINE, while it drains the
// SDK event stream for the turn it is driving — permission.requested and
// permission.resolved are events. A decision is not: event.Event is a closed
// union with no decision kind (the reason internal/decision exists at all), so
// no handler here can ever see one by watching a stream. The supervisor's
// per-session watcher over the session's decision gate is the ONLY observation
// point, and these two methods are the whole of the daemon's decision ingress.
//
// One consequence is worth stating plainly, because it is a real behavior
// change and not an accident: that watcher is a subscriber, and
// decision.Gate.Request answers ErrNoClient purely on "is anything subscribed".
// So under a daemon ErrNoClient never fires, and an ask_user with ZERO peers
// attached simply stays open until a peer attaches (session/load replays it,
// see handleSessionLoad), the turn is interrupted, or the session ends —
// exactly what a permission asked with zero peers attached already does. See
// decision.Gate.Request's doc for the full argument, including why no
// decision-side timeout is invented here.
var _ supervisor.DecisionRelay = (*Daemon)(nil)

// RequestDecision implements [supervisor.DecisionRelay]. It records the
// request's route + retained payload, fans the gofer-native
// gofer/decision_requested out to every attached peer, and ALSO issues the
// spec-ACP session/request_decision REQUEST to every attached ACP peer — so a
// pure ACP client (a phone) can answer a question a gofer client (the TUI) also
// sees. First answer from either surface wins at the gate; the other becomes a
// no-op (see [Daemon.askPeerDecision]).
//
// It differs from [Daemon.RequestPermission] in issuing the ACP request itself.
// That method deliberately does not, because the prompt handler already does it
// for every live permission and doubling would be worse than the gap; here
// there is no prompt handler in the picture at all, so this is the only place
// the ACP fan-out can happen.
//
// Both fan-outs run under the daemon's own base context (see
// [relayWriteTimeout]) rather than any request context: this runs on the
// supervisor's watcher goroutine, which is the sole drainer of that session's
// decision subscription, so an unbounded write to a stalled peer would stall
// every subsequent decision for the session. The route and retained payload are
// recorded BEFORE the broadcast, so a peer that misses a timed-out notification
// still receives the request on its next attach.
func (d *Daemon) RequestDecision(sessionID, requestID string, questions []acp.DecisionQuestion) {
	key := decisionKey{session: sessionID, request: requestID}
	params := decisionRequestedParams{SessionID: sessionID, ID: requestID, Questions: questions}
	if !d.recordDecisionRoute(key, params) {
		return // another observer already routed and fanned this request out
	}
	d.broadcastDecision(sessionID, methodGoferDecisionRequested, params)
	d.requestDecisionFromPeers(key, questions)
}

// ResolveDecision implements [supervisor.DecisionRelay]. It retracts the
// outstanding ACP requests at every peer, releases the request's route and
// retained payload, and fans the gofer-native gofer/decision_resolved out so a
// client still rendering the prompt clears it.
//
// The resolution broadcast fires only for the observer that actually cleared
// the retained payload, so a request resolved through the eager cleanup in
// handleDecisionAnswer and then again by the gate's own update is announced
// exactly once.
func (d *Daemon) ResolveDecision(sessionID, requestID string) {
	key := decisionKey{session: sessionID, request: requestID}
	d.cancelDecisionRequest(key)
	if d.clearPendingDecision(key) {
		d.broadcastDecision(sessionID, methodGoferDecisionResolved, decisionResolvedParams{
			SessionID: sessionID,
			ID:        requestID,
		})
	}
}
