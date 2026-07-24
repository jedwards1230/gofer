package supervisor

import (
	"github.com/jedwards1230/agent-sdk-go/acp"
)

// DecisionRelay is the seam a HOST puts under the supervisor so structured
// decisions can leave the process: for each session, the supervisor runs a
// standing watcher over its [decision.Gate] and calls this relay as requests
// open and resolve. *internal/daemon.Daemon implements it — RequestDecision
// records the request's route, retains it for replay-on-attach, and fans a
// gofer/decision_requested out to every attached peer; ResolveDecision releases
// that state and fans the resolution out.
//
// # Why this is not the permission arrangement
//
// A permission rides the SDK's event stream, so the daemon's session/prompt
// handler already observes every one inline while it drains that stream, and
// the analogous internal/daemon.PermissionRelay exists only for the ONE case
// that handler cannot see (an M6 worker's adopted session). A decision rides
// nothing: event.Event is a closed union with no decision kind — the whole
// reason internal/decision exists — so there is no stream for a handler to
// notice one on, and the only place a decision is observable is the gate
// itself. This relay is therefore the sole path, not a supplement, and its
// watcher runs for every session rather than for adopted ones.
//
// # Why the interface lives here rather than in internal/daemon
//
// internal/daemon imports this package (it hosts a [Supervisor]), so this
// package cannot import it back. The interface is declared at the CONSUMER, the
// idiomatic direction, and internal/daemon asserts *Daemon satisfies it — a
// signature drift fails that build, not a call site.
//
// Both methods are called from the per-session watcher goroutine and must be
// safe for concurrent use across sessions. They must also be BOUNDED: the
// watcher is the only thing draining its gate's subscription, so a relay that
// blocks indefinitely would stall that session's decision stream (the daemon's
// implementation bounds every fan-out write — see its relayWriteTimeout).
type DecisionRelay interface {
	// RequestDecision reports that requestID opened on sessionID with questions.
	// requestID is unique only WITHIN the session (the gate mints "dec-1",
	// "dec-2", … per session), so an implementation must key its bookkeeping on
	// the pair, never on the id alone.
	RequestDecision(sessionID, requestID string, questions []acp.DecisionQuestion)
	// ResolveDecision reports that requestID left sessionID's open set — it was
	// answered (by any peer), its turn was interrupted, or the session ended.
	ResolveDecision(sessionID, requestID string)
}

// SetDecisionRelay installs relay as the destination for every live session's
// decision updates and starts a standing watcher for each session that does not
// already have one. It is idempotent per session (a session's watcher is
// started exactly once, whichever of this and [Supervisor.register] gets there
// first) but NOT a way to swap relays: a session already watching keeps the
// relay it started with, so a second call with a different relay affects only
// sessions registered afterwards. There is one host per process, installing
// once at startup, so that limitation is a simplification rather than a
// constraint anyone runs into.
//
// A nil relay is ignored — "no relay" is the daemonless configuration, and the
// difference is load-bearing: with no relay there is no watcher, so nothing is
// subscribed to a session's gate unless a client subscribes, and
// [decision.Gate.Request] still answers ErrNoClient for a session nobody is
// watching. Installing one flips that (see Gate.Request's doc), which is
// exactly the behavior a daemon wants and exactly the behavior an in-process
// TUI does not.
//
// The daemon calls it once, after building the Daemon and before serving, so in
// practice no session exists yet; the retro-start loop below exists so the
// contract does not silently depend on that ordering.
func (s *Supervisor) SetDecisionRelay(relay DecisionRelay) {
	if relay == nil {
		return
	}
	s.mu.Lock()
	s.decisionRelay = relay
	live := make([]*managed, 0, len(s.roster))
	for _, m := range s.roster {
		live = append(live, m)
	}
	s.mu.Unlock()

	// Started outside s.mu: watchDecisions subscribes to the session's gate and
	// the roster lock must never be held across another component's lock.
	for _, m := range live {
		m.startDecisionWatch(relay)
	}
}
