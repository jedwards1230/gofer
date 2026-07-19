package daemon

import (
	"encoding/json"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"
)

// EventRelay is the daemon-side SESSION-EVENT fan-out an M6 router drives for a
// session whose turn it is not itself hosting (docs/milestones/M6-process-isolation.md
// §5). It is the exact sibling of [PermissionRelay]: that one bridges an adopted
// session's reconstructed PERMISSION events back into this daemon's fan-out,
// this one bridges its ORDINARY event stream.
//
// The gap it closes: a turn running on a worker the router merely adopted (or
// one the router itself dispatched via Create's first prompt) has NO
// [handleSessionPrompt] loop in this daemon observing its events, so nothing
// fans them out to attached clients — a client attached to such a session would
// see a silent stream until the turn ended. The router closes that gap by
// installing a [github.com/jedwards1230/gofer/internal/wirestream.EventSink] on
// each worker's reconstruction core and calling this relay from it.
//
// # Marshal-once
//
// A worker's gofer/event frame ALREADY carries the source event's own
// MarshalJSON envelope, verbatim — it is the same wire this daemon writes in
// [Daemon.broadcastGoferEvent]. So [Daemon.BroadcastRawEvent] takes those bytes
// and writes them through UNCHANGED: there is no json.Marshal anywhere on that
// path, and the frame a client receives is byte-identical to the one the worker
// emitted. That is not a micro-optimization — a decode/re-encode round trip
// through any lossy intermediate (chiefly ACP's session/update, which drops
// tool.call.finished's Diagnostics/Spill* and tool.call.delta fragments
// entirely) would silently shed fields. [Daemon.BroadcastSessionUpdate] handles
// the ACP projection separately, from the event the relay's caller ALREADY
// decoded, so the lossy projection never sits on the lossless path.
//
// *[Daemon] implements it; the router receives one via its own SetEventRelay and
// never imports daemon internals. Both methods are safe to call from the
// router's per-session sink goroutine concurrently with ordinary request
// handling: they take only the peer-registry read lock, released before any
// socket write (see [Daemon.peersForSession]).
type EventRelay interface {
	// BroadcastRawEvent forwards one worker gofer/event frame's params, verbatim,
	// to every peer attached to sessionID. It is a no-op while a prompt handler
	// for that session is active in this daemon (see the double-delivery guard on
	// [Daemon.BroadcastRawEvent]).
	BroadcastRawEvent(sessionID string, raw json.RawMessage)
	// BroadcastSessionUpdate projects ev to its ACP session/update — for the
	// pure-ACP peers that do not read gofer/event — and fans that out to every
	// peer attached to sessionID. ev is the event the caller already decoded, so
	// this costs no second decode. Gated by the same guard.
	BroadcastSessionUpdate(sessionID string, ev event.Event)
}

// Daemon satisfies [EventRelay] so the M6 router can bridge a worker-hosted
// session's reconstructed event stream into this daemon's client fan-out.
var _ EventRelay = (*Daemon)(nil)

// BroadcastRawEvent implements [EventRelay]. It mirrors
// [Daemon.broadcastGoferEvent]'s fan-out exactly — every peer attached to the
// session, a per-peer write failure logged and skipped rather than escalated —
// with ONE difference: it never marshals, because its caller already holds the
// verbatim envelope (see the package-level marshal-once note above).
//
// # Double-delivery guard
//
// [handleSessionPrompt] already fans every event of the turn it drives out to
// every attached peer, off its own subscription. For a session a client drives
// that way through a router, the router's sink ALSO observes those same events —
// so without a guard every peer would receive each event TWICE. The daemon
// therefore marks a session while a prompt handler is running for it
// ([Daemon.beginPromptHandler], registered at handler entry and released by a
// defer that runs after its final broadcast), and this relay returns early while
// that mark is set. It is the same shape as the first-observer gates
// [Daemon.recordPermRoute]/[Daemon.clearPendingPerm] already use for the
// permission fan-out, not a new dedup concept.
//
// The guard's window is a TURN BOUNDARY of at most one event: the mark is taken
// before the handler subscribes, so an event published in between is suppressed
// here and not observed there. It is accepted rather than closed, because the
// alternative ordering (subscribe first, then mark) trades that for the far
// worse failure — a genuinely DOUBLED event on every such boundary — and a
// client's durable transcript comes from the journal replay on attach, not from
// this live path.
func (d *Daemon) BroadcastRawEvent(sessionID string, raw json.RawMessage) {
	if d.promptHandlerActive(sessionID) {
		return
	}
	for _, pr := range d.peersForSession(sessionID) {
		if werr := pr.notify(d.ctx, methodGoferEvent, raw); werr != nil {
			// Session id only — never the frame (it may carry prompt/message
			// content); see handleFrame's redaction rule.
			d.log.Debug("gofer/event relay: peer notify failed", "session", sessionID, "err", werr)
		}
	}
}

// BroadcastSessionUpdate implements [EventRelay]. It is the ACP half of the same
// bridge: a pure-ACP peer (a phone) reads session/update, not gofer/event, so a
// relayed turn must reach it too. Events ACP cannot express (acp.ToSessionUpdate
// reports ok=false — turn.started, tool.call.delta, permission.*, …) are simply
// not projected, exactly as in [handleSessionPrompt].
//
// Unlike the prompt handler's [Daemon.broadcastUpdate] there is no origin peer
// here — no client is driving this turn through this daemon — so there is no
// user-echo suppression and no fatal-write rule: those two behaviors belong to
// the prompt path and stay there (a relay that owned all fan-out would silently
// drop both). A write failure is logged and skipped, like every other
// non-origin delivery.
func (d *Daemon) BroadcastSessionUpdate(sessionID string, ev event.Event) {
	if d.promptHandlerActive(sessionID) {
		return
	}
	notif, ok := acp.ToSessionUpdate(sessionID, ev)
	if !ok {
		return
	}
	for _, pr := range d.peersForSession(sessionID) {
		if werr := pr.notify(d.ctx, acp.MethodSessionUpdate, notif); werr != nil {
			d.log.Debug("session/update relay: peer notify failed", "session", sessionID, "err", werr)
		}
	}
}

// beginPromptHandler marks sessionID as having a live [handleSessionPrompt] loop
// in this daemon, so the event relay stands down for it (see
// [Daemon.BroadcastRawEvent]'s double-delivery guard). It is a COUNTER, not a
// flag: nothing in the wire contract forbids two concurrent session/prompt calls
// for one session (the M2 contract asks callers not to, but a misbehaving client
// is not a reason to un-suppress the relay), so the mark clears only when the
// last handler leaves.
func (d *Daemon) beginPromptHandler(sessionID string) {
	d.promptMu.Lock()
	d.promptHandlers[sessionID]++
	d.promptMu.Unlock()
}

// endPromptHandler releases one [Daemon.beginPromptHandler] mark, deleting the
// entry at zero so the map stays bounded by CONCURRENT prompts rather than by
// sessions ever prompted.
func (d *Daemon) endPromptHandler(sessionID string) {
	d.promptMu.Lock()
	if n := d.promptHandlers[sessionID] - 1; n > 0 {
		d.promptHandlers[sessionID] = n
	} else {
		delete(d.promptHandlers, sessionID)
	}
	d.promptMu.Unlock()
}

// promptHandlerActive reports whether a prompt handler in this daemon is
// currently driving sessionID — the relay's stand-down condition.
func (d *Daemon) promptHandlerActive(sessionID string) bool {
	d.promptMu.Lock()
	defer d.promptMu.Unlock()
	return d.promptHandlers[sessionID] > 0
}
