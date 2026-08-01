package daemon

import (
	"encoding/json"
)

// sessionObserver is the identity token of one standing out-of-turn event
// observer. It carries no state — the goroutine owns everything — and exists
// only so [Daemon.releaseSessionObserver] can tell "my own registry entry" from
// "a newer observer that replaced me", which a bare bool could not.
//
// It holds the session id (rather than being an empty struct) deliberately: Go
// may give every zero-sized allocation the SAME address, which would make two
// tokens compare equal and let an exiting observer delete its successor's entry.
type sessionObserver struct {
	session string
}

// startSessionObserver begins the standing out-of-turn event observer for
// sessionID: a no-replay subscription that drains the session's broker for the
// session's whole life and rebroadcasts every event to the peers attached to it.
//
// It is a NO-OP unless [Config.RelayOutOfTurnEvents] is set — see that field for
// why only the M6 session-worker sets it, and why enabling it on the in-process
// `gofer daemon` would double-deliver. Called from handleSessionNew and
// handleSessionLoad, which are where this daemon learns a session exists (the
// supervisor's own registration hook is one call-stack frame and one package
// away, inside worker.Serve, and hands no daemon back out — so the in-process
// OnRegister shape is not available here; the narrow [Supervisor] seam's
// SubscribeLive, which needs only the id, is).
//
// # Idempotent per session
//
// A session is loaded once per attaching client, so session/load runs many times
// for one session. The registry makes every call after the first a no-op: two
// subscriptions would deliver every out-of-turn event twice, which is the exact
// failure the relay's own guard doc calls worse than the window it accepts.
//
// # Subscribe timing: SubscribeLive at BOTH call sites
//
// [Supervisor.SubscribeLive] omits the broker's retained must-deliver backlog;
// Subscribe would pre-load it. Live is correct at both sites:
//
//   - At session/load, the backlog is the prior turn's terminal must-deliver
//     events — which handleSessionLoad ALREADY replays to the attaching peer as
//     folded history. Replaying them again through this relay would be a
//     straight duplication.
//   - At session/new, the choice is unobservable: handleSessionNew does not
//     attach the calling peer (only session/load and session/prompt do), so the
//     session's peer set is empty at that instant and a backlog would be
//     broadcast to nobody.
//
// The accepted limitation: notices published SYNCHRONOUSLY inside
// supervisor.Create (the MCP-down notices, the skipped-SKILL.md note) are
// already past by the time any daemon-side subscribe can happen — Create must
// return before its id is knowable — so this observer never carries them live.
// That is not a regression: the in-process watcher this mirrors subscribes with
// EventsLive too and loses exactly the same two notices.
//
// # Lifetime
//
// Two exits, both without extra bookkeeping. The subscription channel closes
// when the session's broker does (Supervisor.Kill/Close both close the session),
// ending the range; and d.ctx — cancelled by [Daemon.Shutdown] — ends every
// observer daemon-wide, so a shutdown leaves no goroutine per session behind.
func (d *Daemon) startSessionObserver(sessionID string) {
	if !d.cfg.RelayOutOfTurnEvents || sessionID == "" {
		return
	}

	// Reserve the registry slot BEFORE subscribing, so two concurrent
	// session/load calls for the same session cannot both get past the check and
	// open two subscriptions. A failed subscribe releases the slot below.
	obs := &sessionObserver{session: sessionID}
	d.observerMu.Lock()
	if _, running := d.observers[sessionID]; running {
		d.observerMu.Unlock()
		return
	}
	d.observers[sessionID] = obs
	d.observerMu.Unlock()

	// d.ctx, not the request context: the subscription outlives the RPC that
	// starts it by design. The supervisor reads the context only to fail fast on
	// an already-cancelled caller — it does not retain it (see
	// supervisor.SubscribeLive) — so this scopes the observer to the daemon.
	sub, err := d.sup.SubscribeLive(d.ctx, sessionID)
	if err != nil {
		// Not an error for the caller: a session that cannot be subscribed to is
		// one this daemon is not hosting live, and the RPC that got here has its
		// own reporting for that.
		d.releaseSessionObserver(obs)
		d.log.Debug("out-of-turn observer: subscribe failed", "session", sessionID, "err", err)
		return
	}

	go func() {
		defer sub.Close()
		defer d.releaseSessionObserver(obs)
		for {
			select {
			case <-d.ctx.Done():
				return
			case e, ok := <-sub.C:
				if !ok {
					return
				}
				raw, merr := json.Marshal(e)
				if merr != nil {
					// Kind only — never the marshalled event, which may carry
					// message content (see handleFrame's redaction rule).
					d.log.Debug("out-of-turn observer: marshal event failed",
						"session", sessionID, "kind", e.Kind(), "err", merr)
					continue
				}
				// The SAME exported, already-guarded relay methods the M6 router
				// drives. Their promptHandlerActive check is what stands this
				// observer down while a session/prompt handler in this daemon is
				// already fanning the session's events out.
				d.BroadcastRawEvent(sessionID, raw)
				d.BroadcastSessionUpdate(sessionID, e)
			}
		}
	}()
}

// releaseSessionObserver drops obs's registry entry, but ONLY if it is still the
// current one: a session closed and re-loaded gets a fresh observer, and the old
// goroutine must not delete the new one's slot as it exits.
func (d *Daemon) releaseSessionObserver(obs *sessionObserver) {
	d.observerMu.Lock()
	if d.observers[obs.session] == obs {
		delete(d.observers, obs.session)
	}
	d.observerMu.Unlock()
}
