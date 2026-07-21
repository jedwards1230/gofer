package wirestream

import (
	"context"
	"encoding/json"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/decision"
)

// This file REPLAYS each session's typed [event.Event] stream from the
// daemon's gofer/event notifications — the M3 lossless-attach wire contract
// (internal/daemon/handlers.go's methodGoferEvent): each notification's
// params ARE one source event's own MarshalJSON envelope, verbatim, so
// reconstruction here is pure decode-and-republish — decode the envelope's
// "type" discriminator, rebuild the exact concrete [event.Event] via the
// SDK's exported event.New* constructors (see handleGoferEvent's dispatch
// table), and Publish it to this session's local broker. There is no lossy
// projection step and no open-message bookkeeping: every field the source
// event carried (incl. tool.call.delta's streaming input fragments and
// tool.call.finished's Diagnostics/Spill* fields, both entirely absent from
// ACP's session/update) survives the round trip. session/update itself is
// IGNORED on this path — it still goes out (serving an ACP client, on the
// same connection), this core just never reads it (see
// handleNotification). It also drives the turn lifecycle's one FALLBACK case
// [Reconstructor.Send] — the goroutine holding the blocking session/prompt Call
// and its PromptResponse — cannot observe any other way: a Call failure with
// no matching terminal gofer/event already replayed (see handleTurnEnd).
//
// It also, via [Reconstructor.loadHistory]/[Reconstructor.finishLoad], replays
// a session's settled history through this SAME gofer/event path the first
// time the core ever references it — see loadHistory's doc below for the
// full design (why it must run off the demuxer goroutine, and how it
// guarantees history is applied before any live event for the same session).
//
// # Single demuxer, one goroutine, three inputs
//
// [New] starts exactly one demux goroutine. It is the sole reader of
// [daemon.Client.Notifications] (required: Client's doc states any caller
// issuing a call that streams notifications — session/prompt, session/load —
// needs a peer goroutine draining Notifications concurrently, or the read
// loop stalls behind a full buffer); the sole reader of turnEndCh, the
// internal channel [Reconstructor.Send] posts its turn's outcome to once the
// daemon's session/prompt Call resolves; and the sole reader of loadCh, the
// analogous channel [Reconstructor.loadHistory] posts to once the daemon's
// session/load Call resolves. Because it is the only goroutine that ever
// mutates a sessionState's turnTerminated field or publishes to a session's
// broker for the replay path, event ordering within one session's stream is
// entirely determined by this goroutine's own sequential execution — no lock
// is needed for that state (see sessionState's doc).
//
// One shared demuxer across all sessions has a bounded head-of-line
// characteristic worth naming: it publishes must-deliver events into per-session
// brokers, and [event.Broker] blocks a publish up to its block-bound (5s in the
// SDK default) on a subscriber whose buffer is full before force-unsubscribing
// it. So a single wedged subscriber can stall reconstruction — and, if the
// 64-slot Notifications buffer then fills, in-flight control Calls — for other
// sessions for up to that bound. It is bounded (the SDK force-drops the wedged
// subscriber and the demuxer resumes) and low-likelihood in M2 (deltas ride the
// lossy tier and never block; only a backlog of must-deliver events could
// trigger it), and is accepted for M2; a per-session demuxer would remove it.
// Relatedly, sessionState entries are created on first reference and not reaped
// on Kill/Archive — bounded by the process lifetime of one consumer session, also
// accepted for M2.
//
// # The TurnFinished-vs-last-event ordering guarantee
//
// The daemon's handleSessionPrompt (internal/daemon/handlers.go) writes every
// notification for a turn — session/update AND gofer/event alike — to the
// wire, synchronously, BEFORE it writes the terminating session/prompt
// JSON-RPC response (it literally cannot do otherwise: the response is only
// sent once the handler observes the turn's terminal event, and every event
// up to and including that one is first pushed out as a gofer/event
// notification — see broadcastGoferEvent). [daemon.Client]'s single read loop
// reads frames strictly in wire order and, for a notification frame, SENDS it
// on the (buffered, capacity 64) Notifications channel BEFORE it advances to
// read the next frame. So the send of the turn's last notification onto that
// channel is program-order-before, and therefore happens-before, the read
// loop's later delivery of the matching response — which is what unblocks
// [Reconstructor.Send]'s Call and lets it post to turnEndCh.
//
// That establishes: by the time turnEndCh's send for a turn occurs, the
// turn's last notification (its terminal gofer/event turn.finished) has
// ALREADY been sent onto Notifications — it is either (a) already popped and
// replayed onto this session's broker by an earlier iteration of this
// goroutine (ordering trivially holds), or (b) still sitting in the
// Notifications channel's buffer, not yet popped. handleTurnEnd's first
// action is [Reconstructor.drainNotifications]: a non-blocking, exhaustive drain
// of Notifications run BY THIS SAME goroutine, synchronously, before it does
// anything else for the turn-end. Since this goroutine is Notifications' only
// consumer, a value already sent onto it cannot be lost or reordered out from
// under a later non-blocking receive attempt by that same sole consumer —
// case (b)'s pending notification is therefore guaranteed to be drained (and
// republished, updating rec.turnTerminated — see handleGoferEvent) before
// handleTurnEnd decides whether its fallback terminal event is even needed.
// There is no residual race: this holds for every interleaving of the two
// producer goroutines (the daemon.Client read loop, and Send's goroutine),
// because it rests only on ordinary Go channel semantics (a sent value
// persists until some receive takes it; a single consumer cannot miss what it
// hasn't yet received) plus the wire-order invariant above — not on
// scheduling luck.
//
// The identical argument, substituting handleSessionLoad for
// handleSessionPrompt and loadCh/finishLoad for turnEndCh/handleTurnEnd,
// establishes that every notification a session/load replayed is drained
// (and applied) before [Reconstructor.finishLoad] closes rec.loadDone — see
// [Reconstructor.loadHistory]'s doc.
func (r *Reconstructor) demux() {
	defer r.wg.Done()
	defer r.closeAllBrokers()
	for {
		select {
		case n, ok := <-r.client.Notifications():
			if !ok {
				return
			}
			r.handleNotification(n)
		case te := <-r.turnEndCh:
			r.drainNotifications()
			r.handleTurnEnd(te)
		case rec := <-r.loadCh:
			r.drainNotifications()
			r.finishLoad(rec)
		}
	}
}

// drainNotifications forwards every notification currently buffered on
// Notifications, without blocking, then returns as soon as none is
// immediately available. See demux's doc for why this is the linchpin of the
// TurnFinished ordering guarantee.
func (r *Reconstructor) drainNotifications() {
	for {
		select {
		case n, ok := <-r.client.Notifications():
			if !ok {
				return
			}
			r.handleNotification(n)
		default:
			return
		}
	}
}

// closeAllBrokers closes every session's reconstructed broker AND its decision
// stream once the client connection (and so the demuxer) has shut down, so any
// still-live Subscribe/Decisions channel observes a clean close instead of
// hanging forever. Closing the stream also publishes a resolution for every
// decision still open on it, so a consumer clears a prompt it is rendering
// rather than leaving a question on screen that nothing can answer any more (see
// [decision.Stream.Close]).
func (r *Reconstructor) closeAllBrokers() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range r.sessions {
		rec.broker.Close()
		rec.decisions.Close()
	}
}

// turnEnd carries one session/prompt Call's outcome from [Reconstructor.Send]'s
// goroutine to the demuxer, which alone is responsible for publishing the
// resulting SessionError/TurnFinished in the right place relative to any
// still-pending notifications (see demux's doc).
type turnEnd struct {
	sessionID  string
	stopReason string // acp.PromptResponse.StopReason on success; "" on error
	err        string // non-empty on any Call failure (network, decode, or *daemon.CallError)
}

// Send submits prompt as sessionID's next turn. It is fire-and-forget by
// contract (the TUI's App calls it as a non-blocking Op — see
// internal/tui/app.go's doSend): it launches the actual session/prompt Call
// on its own goroutine and returns immediately, publishing nothing itself.
// The Call blocks server-side for the whole turn — the daemon streams every
// event as a gofer/event notification the demuxer replays verbatim (see
// handleGoferEvent), INCLUDING this turn's own TurnStarted and its
// MessageStarted/MessageFinished{MessageUser} pair carrying prompt: unlike
// the ACP session/update path, the daemon does NOT suppress the user-message
// echo to the driving peer on gofer/event (methodGoferEvent's doc: no
// origin special-casing), so there is nothing for Send to inject locally
// anymore — the real events arrive over the wire like any other peer's. The
// Call resolves once the turn reaches a terminal stop reason; when it does,
// the goroutine posts the outcome to turnEndCh, and the demuxer decides
// whether a fallback terminal event is even needed (see handleTurnEnd — on
// the ordinary path it is not, since the real turn.finished already arrived
// via gofer/event).
//
// Before firing the Call, Send waits on rec.loadDone: for a session
// this core is referencing for the first time (rec.loadDone was just
// opened by session's call to loadHistory), this blocks until that
// session's history replay has been fully applied — see loadHistory's doc
// for why this is the piece that makes "history before any live event"
// actually hold, not just "history requested before any live event". For
// every other session (already loaded, or RegisterFresh'd as history-free at
// create time), rec.loadDone is already closed and this is a non-blocking
// no-op.
//
// The prompt Call runs against context.Background(), not ctx: like
// cmd/gofer's driveDaemonSession, a turn started this way outlives the
// call that started it (the App always calls Send with context.Background()
// itself — see doSend — since Send is meant to keep running after the TUI
// event loop has moved on to render other state).
//
// One-outstanding-turn-per-session is CALLER-enforced: Send fires the Call
// unconditionally — the core keeps no prompt queue of its own. The
// invariant holds because the TUI App only sends to a session it sees as
// idle (see internal/tui's doSend); a caller that pipelined two Sends on one
// session would interleave two turns' replayed events.
func (r *Reconstructor) Send(_ context.Context, sessionID, prompt string) error {
	rec := r.session(sessionID)
	if rec == nil {
		return nil // reconstructor closed: a Send is a no-op
	}
	select {
	case <-rec.loadDone:
	case <-r.closed:
		return nil
	}

	go func() {
		raw, err := r.client.Call(context.Background(), acp.MethodSessionPrompt, acp.PromptRequest{
			SessionID: sessionID,
			Prompt:    []acp.ContentBlock{acp.TextBlock(prompt)},
		})
		te := turnEnd{sessionID: sessionID}
		switch {
		case err != nil:
			te.err = err.Error()
		default:
			var pr acp.PromptResponse
			if uerr := json.Unmarshal(raw, &pr); uerr != nil {
				te.err = uerr.Error()
			} else {
				te.stopReason = string(pr.StopReason)
			}
		}
		select {
		case r.turnEndCh <- te:
		case <-r.closed:
		}
	}()
	return nil
}

// handleTurnEnd is the FALLBACK path for a turn's terminal event: on the
// ordinary path the daemon's own real turn.finished (and, on a fatal path,
// its preceding session.error) already arrived and was replayed onto rec's
// broker via handleGoferEvent — publishing another here would double-deliver
// it. This only publishes a synthesized SessionError+TurnFinished("error")
// pair when te.err is set AND no terminal gofer/event turn.finished was
// already replayed for this turn (!rec.turnTerminated) — i.e. the
// session/prompt Call itself failed (a dropped connection, a decode error)
// with nothing terminal ever having reached the wire, or the documented
// "fatal session.error with no turn.finished" case (see
// internal/daemon/handlers.go's handleSessionPrompt doc). rec.turnTerminated
// is demuxer-only (set in handleGoferEvent, read here — both run only on the
// demuxer goroutine — see the package doc), so no locking is needed, and
// [Reconstructor.drainNotifications] (see demux) has already forwarded every
// notification this turn produced, incl. its terminal one if any, before
// this runs — so the read below is never stale.
func (r *Reconstructor) handleTurnEnd(te turnEnd) {
	rec := r.session(te.sessionID)
	if rec == nil {
		return // reconstructor closing: drop the terminal event
	}
	if te.err != "" && !rec.turnTerminated {
		rec.broker.Publish(event.NewSessionError(te.sessionID, te.err, true))
		rec.broker.Publish(event.NewTurnFinished(te.sessionID, "error", provider.Usage{}))
	}
}

// loadHistory issues session/load for rec.id — the reconstruction's answer
// to the M1 bug this exists to fix: attaching over the daemon rendered a
// blank transcript even for a session with prior turns, because reconstruct.go
// only ever built a session's [event.Event] stream from LIVE notifications.
// [Reconstructor.session] starts loadHistory on its own goroutine at most once
// per session id — see its doc — the moment the core references a session
// the caller did not itself just create (which pre-registers via RegisterFresh
// instead, skipping the load entirely: a brand-new session has no history).
//
// # Why this must run off the demuxer goroutine
//
// [daemon.Client]'s single read loop demuxes both call responses and
// notifications onto, respectively, a per-call channel and the shared
// Notifications channel (64-slot buffer) — see its doc. handleSessionLoad
// (internal/daemon/handlers.go) writes every replay notification to the wire
// strictly before the session/load response, so that response can only be
// read once every replay notification has already been enqueued onto
// Notifications. If the demuxer goroutine — Notifications' ONLY consumer —
// were the one blocked awaiting that response (i.e. if it issued this Call
// inline instead of handing it to a dedicated goroutine), a session whose
// history exceeds the buffer's 64 slots would deadlock: the read loop's
// blocking send of the 65th replay notification would never complete, since
// nothing would be left to drain the channel, so the response — and every
// notification behind it — could never arrive either. Running the Call on
// its own goroutine, exactly the pattern [Reconstructor.Send] already uses for
// session/prompt, keeps the demuxer free to keep draining Notifications (and
// therefore keep accepting replay notifications) throughout the load.
//
// # Ordering: history before any live event for the same session
//
// loadHistory itself never touches rec's broker or turnTerminated state —
// that stays demuxer-only (see sessionState's doc) — it only issues the RPCs
// and hands rec off to the demuxer via loadCh once the Call resolves, success
// or failure alike (a failed load — e.g. an id the daemon doesn't recognize —
// just leaves the session starting from whatever live events arrive next,
// the pre-existing M1 behavior, rather than failing attach outright). The
// demuxer's loadCh case (see demux) drains every notification still
// buffered before calling [Reconstructor.finishLoad] — by the identical
// wire-order argument demux's doc makes for turnEndCh/handleTurnEnd, that
// drain is guaranteed to forward every gofer/event this load replayed — and
// finishLoad closes rec.loadDone only once that drain has run.
// [Reconstructor.Send] waits on rec.loadDone before dispatching anything for a
// session (see its doc), so a live turn the core itself starts can never race
// a still-settling history replay onto the broker ahead of it.
//
// A live turn a DIFFERENT peer drives now CAN interleave with this core's
// history load: the daemon fans each turn's gofer/event out to every peer
// attached to the session — including one that just issued session/load — not
// only to the peer whose own call produced them (see internal/daemon's
// broadcastGoferEvent; the core attaches by issuing the session/load Call
// loadHistory makes). Replay stays correct because the SAME demuxer goroutine
// applies both streams: the session/load response can only be read once every
// replay notification has been enqueued onto Notifications ahead of it
// (handleSessionLoad writes them to the wire first), and the demuxer's loadCh
// case drains all of those before finishLoad closes rec.loadDone — so a
// concurrent peer's live gofer/event, arriving as an ordinary notification, is
// applied either fully before the load settles or after it, never torn across
// it. What is NOT guaranteed once a second peer drives a turn during this load
// is the relative ORDER of that live turn's events against the tail of the
// replayed history — but the daemon does not order events across independent
// turns from different clients, and a consumer's transcript reconstruction
// (tui.Model.Ingest) rebuilds each item by its own started/finished boundary,
// so the transcript stays coherent regardless.
func (r *Reconstructor) loadHistory(rec *sessionState) {
	ctx := context.Background()
	cwd := r.sessionCwd(ctx, rec.id)
	_, _ = r.client.Call(ctx, acp.MethodSessionLoad, acp.LoadSessionRequest{SessionID: rec.id, Cwd: cwd})
	select {
	case r.loadCh <- rec:
	case <-r.closed:
	}
}

// finishLoad settles rec's history load. Called from the demuxer only after
// drainNotifications has exhaustively forwarded every notification currently
// buffered (see demux's loadCh case and loadHistory's doc), so every
// gofer/event this load replayed has already been applied via
// handleGoferEvent by the time this runs. With verbatim replay there is no
// open-message state left to flush (each replayed message arrived as its own
// complete MessageStarted/MessageFinished pair — see historyEvents in
// internal/daemon), so this simply unblocks any [Reconstructor.Send] waiting on
// rec.loadDone.
func (r *Reconstructor) finishLoad(rec *sessionState) {
	close(rec.loadDone)
}

// handleNotification decodes one inbound notification and applies it to its
// session's replay state. gofer/event carries the M3 lossless-attach replay
// (see handleGoferEvent); gofer/permission_requested / gofer/permission_resolved
// carry the M3 approvals-relay events (see handlePermissionRequested/
// handlePermissionResolved) — permission.* deliberately never arrives via
// gofer/event (methodGoferEvent's doc), so there is no double-delivery risk
// between the two. session/update — still sent by the daemon, serving an ACP
// client on the same connection — is IGNORED here: the core gets the
// identical events, losslessly, via gofer/event instead, so there is nothing
// for it to reconstruct from the lossy ACP projection anymore. Anything else,
// or anything that fails to decode, is a protocol drift, not a reason to
// crash replay, and is silently dropped.
func (r *Reconstructor) handleNotification(n daemon.Notification) {
	switch n.Method {
	case methodGoferPermissionRequested:
		r.handlePermissionRequested(n.Params)
	case methodGoferPermissionResolved:
		r.handlePermissionResolved(n.Params)
	case methodGoferDecisionRequested:
		r.handleDecisionRequested(n.Params)
	case methodGoferDecisionResolved:
		r.handleDecisionResolved(n.Params)
	case methodGoferEvent:
		r.handleGoferEvent(n.Params)
	}
}

// handleGoferEvent decodes one gofer/event notification's params — the
// source [event.Event]'s own MarshalJSON envelope, verbatim (methodGoferEvent's
// doc) — and republishes the exact same concrete event onto its session's
// broker, via the SDK's exported event.New* constructors: a pure
// decode-dispatch-publish, no open-message bookkeeping. seq/time are NOT
// restored (event.New* always builds seq=0/time=zero); rec.broker reassigns
// them at Publish, same as it already does for every other event this core
// publishes — "lossless" here means every event kind, every payload field,
// and ordering, not source seq/time (see the package doc for why that's by
// design, not a gap).
//
// It also maintains rec.turnTerminated, the demuxer-only signal
// [Reconstructor.handleTurnEnd] reads to decide whether its fallback terminal
// event is needed: set false on replaying turn.started (a new turn is now
// open), true on replaying a turn.finished whose stop reason is not
// "tool_use" (the loop's mid-turn marker — see [event.TurnFinished]'s doc).
// Both this method and handleTurnEnd run only on the demuxer goroutine (see
// the package doc), so no lock guards turnTerminated.
//
// r.session(w.SessionID) below will, in practice, always find an
// already-mapped entry rather than create one: this connection only receives
// a notification for a session it has ATTACHED to, and it attaches only by
// issuing session/load (loadHistory) or session/prompt (Send) for that
// session — both of which reference the session via session() (creating its
// entry, and for loadHistory starting the load) before dispatching their
// Call. Crucially, the notification may now be for a turn a DIFFERENT peer
// drove — the daemon fans each turn out to every attached peer, not just the
// caller whose Call produced it (see internal/daemon's broadcastGoferEvent) —
// but the core still only ever attaches through its own session()-backed
// Call, so the entry exists regardless of which peer's turn the notification
// carries. The lookup-or-create fallback here exists only so a genuinely
// unexpected notification (a protocol drift) degrades to "replay into a
// fresh, unloaded broker" rather than a nil dereference, not because this
// path is expected to fire in normal operation.
func (r *Reconstructor) handleGoferEvent(raw json.RawMessage) {
	var w goferEventWire
	if err := json.Unmarshal(raw, &w); err != nil || w.SessionID == "" {
		return
	}
	rec := r.session(w.SessionID)
	if rec == nil {
		return // reconstructor closing: drop the event
	}

	var ev event.Event
	switch w.Type {
	case event.KindSessionCreated:
		ev = event.NewSessionCreated(w.SessionID)
	case event.KindSessionResumed:
		ev = event.NewSessionResumed(w.SessionID)
	case event.KindSessionForked:
		ev = event.NewSessionForked(w.SessionID)
	case event.KindSessionCompacted:
		ev = event.NewSessionCompacted(w.SessionID)
	case event.KindSessionKilled:
		ev = event.NewSessionKilled(w.SessionID)
	case event.KindSessionArchived:
		ev = event.NewSessionArchived(w.SessionID)
	case event.KindSessionError:
		ev = event.NewSessionError(w.SessionID, w.Err, w.Fatal)
	case event.KindSessionInfo:
		ev = event.NewSessionInfoUpdated(w.SessionID, w.Title)
	case event.KindTurnStarted:
		rec.turnTerminated = false
		ev = event.NewTurnStarted(w.SessionID)
	case event.KindTurnFinished:
		if w.StopReason != "tool_use" {
			rec.turnTerminated = true
		}
		ev = event.NewTurnFinishedCost(w.SessionID, w.StopReason, w.Usage, w.Cost)
	case event.KindMessageStarted:
		ev = event.NewMessageStarted(w.SessionID, w.Kind)
	case event.KindMessageDelta:
		ev = event.NewMessageDelta(w.SessionID, w.Kind, w.Text)
	case event.KindMessageFinished:
		ev = event.NewMessageFinishedMeta(w.SessionID, w.Kind, w.Content, w.Meta)
	case event.KindToolCallStarted:
		// Agent is assigned AFTER construction because the SDK carries it that
		// way too — it is set on the built event at emit time rather than taken
		// as a constructor argument (see event.ToolCallStarted.Agent's doc), so
		// mirroring that here keeps the reconstruction field-for-field faithful
		// to what the daemon marshalled. Dropping it would silently
		// un-attribute every subagent tool call for a remote client (the TUI's
		// approval prompt reads it to say "from the `<agent>` agent").
		started := event.NewToolCallStarted(w.SessionID, w.ID, w.Name, w.Input)
		started.Agent = w.Agent
		ev = started
	case event.KindToolCallDelta:
		delta := event.NewToolCallDelta(w.SessionID, w.ID, w.Delta)
		delta.Agent = w.Agent
		ev = delta
	case event.KindToolCallFinished:
		finished := event.NewToolCallFinishedSpill(w.SessionID, w.ID, w.Input, w.Result, w.IsError, w.Diagnostics, w.SpillPath, w.SpillBytes, w.SpillSHA256)
		finished.Agent = w.Agent
		ev = finished
	default:
		// permission.* (excluded from gofer/event by contract — see
		// methodGoferEvent's doc) or an unknown/future kind: protocol-drift
		// tolerance, not a reason to crash replay.
		return
	}
	// The sink sees the frame BOTH ways — the verbatim bytes and the event just
	// decoded from them — immediately before the local publish, so a consumer
	// forwarding this stream onwards re-emits the SAME bytes (marshal-once, see
	// [EventSink]) while any projection it also drives reuses ev rather than
	// decoding twice. Deliberately before Publish: a publish can block on
	// must-deliver backpressure, and the forwarded frame should not wait on this
	// core's own local subscribers. An unknown kind returns above WITHOUT
	// invoking the sink — a consumer must not forward a frame this core could not
	// decode, or the two fan-outs would disagree about what the stream contains.
	if r.sink != nil {
		r.sink(w.SessionID, raw, ev)
	}
	rec.broker.Publish(ev)
}

// goferEventWire decodes a gofer/event notification's params: the union of
// every [event.Event] concrete type's MarshalJSON payload fields this core
// needs to rebuild one via the matching event.New* constructor (see
// handleGoferEvent's dispatch table). One struct covers every kind because
// encoding/json ignores JSON fields absent from a given kind's envelope and
// leaves the corresponding Go fields at their zero value — exactly what an
// unpopulated kind's fields should decode to anyway. Field names/tags mirror
// each type's MarshalJSON in the SDK's event/event.go exactly.
type goferEventWire struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`

	// session.error
	Err   string `json:"error"`
	Fatal bool   `json:"fatal"`

	// session.info
	Title string `json:"title"`

	// turn.finished
	StopReason string         `json:"stop_reason"`
	Usage      provider.Usage `json:"usage"`
	Cost       *provider.Cost `json:"cost"`

	// message.started / message.delta / message.finished
	Kind    event.MessageKind `json:"kind"`
	Text    string            `json:"text"`
	Content string            `json:"content"`
	Meta    map[string]string `json:"meta"`

	// tool.call.started / tool.call.delta / tool.call.finished
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	Delta string          `json:"delta"`
	// Agent is the originating agent id all three tool-call kinds carry
	// (omitempty on the wire — an un-attributed call simply has no "agent"
	// key, which decodes to ""). handleGoferEvent assigns it after
	// construction, matching how the SDK sets it.
	Agent       string   `json:"agent"`
	Result      string   `json:"result"`
	IsError     bool     `json:"is_error"`
	Diagnostics []string `json:"diagnostics"`
	SpillPath   string   `json:"spill_path"`
	SpillBytes  int64    `json:"spill_bytes"`
	SpillSHA256 string   `json:"spill_sha256"`
}

// handlePermissionRequested reconstructs a gofer/permission_requested
// notification into an [event.PermissionRequested], published straight to
// its session's broker. Unlike session/update, this is not an ACP
// projection: acp.SessionUpdate has no permission variant (ACP-native
// clients like Agmente instead see the standard session/request_permission
// RPC — see docs/PRD.md's Approvals section, and does not fit a must-deliver
// fan-out to N attached peers besides), so the daemon fans this event out to
// every attached peer under its own gofer-native notification (see
// internal/daemon/handlers.go's methodGoferPermissionRequested doc), with
// params a lossless projection of the event plus the routing session id. A
// decode failure is a protocol drift, not a reason to crash reconstruction,
// so it is dropped like any other malformed notification (see
// handleNotification's doc).
func (r *Reconstructor) handlePermissionRequested(raw json.RawMessage) {
	var w permissionRequestedWire
	if err := json.Unmarshal(raw, &w); err != nil || w.SessionID == "" {
		return
	}
	rec := r.session(w.SessionID)
	if rec == nil {
		return // reconstructor closing: drop the update
	}
	rec.broker.Publish(event.NewPermissionRequested(w.SessionID, w.ID, w.Tool, w.Spec, w.Trace))
}

// handlePermissionResolved reconstructs a gofer/permission_resolved
// notification into an [event.PermissionResolved] — see
// handlePermissionRequested's doc for the shared design.
func (r *Reconstructor) handlePermissionResolved(raw json.RawMessage) {
	var w permissionResolvedWire
	if err := json.Unmarshal(raw, &w); err != nil || w.SessionID == "" {
		return
	}
	rec := r.session(w.SessionID)
	if rec == nil {
		return // reconstructor closing: drop the update
	}
	rec.broker.Publish(event.NewPermissionResolved(w.SessionID, w.ID, event.Verdict(w.Verdict), w.Rule))
}

// permissionRequestedWire decodes a gofer/permission_requested notification's
// params — internal/daemon/wire.go's permissionRequestedParams:
// {"sessionId","id","tool","spec","trace"}.
type permissionRequestedWire struct {
	SessionID string         `json:"sessionId"`
	ID        string         `json:"id"`
	Tool      string         `json:"tool"`
	Spec      map[string]any `json:"spec"`
	Trace     []string       `json:"trace"`
}

// permissionResolvedWire decodes a gofer/permission_resolved notification's
// params — internal/daemon/wire.go's permissionResolvedParams:
// {"sessionId","id","verdict","rule"}. Verdict decodes as a plain string
// (the daemon's own wire type, matching event.Verdict's underlying type)
// rather than [event.Verdict] directly, so this stays decodable even if that
// SDK type ever grows unmarshal-side validation.
type permissionResolvedWire struct {
	SessionID string `json:"sessionId"`
	ID        string `json:"id"`
	Verdict   string `json:"verdict"`
	Rule      string `json:"rule"`
}

// handleDecisionRequested reconstructs a gofer/decision_requested notification
// into a [decision.Update] applied to its session's client-side decision stream
// — the structured-decision analogue of handlePermissionRequested, differing in
// its destination for a structural reason: a decision is not an
// [event.Event] (the SDK's union is closed and has no decision kind, see
// internal/decision's package doc), so there is no broker to publish it onto and
// it lands on the session's [decision.Stream] instead.
//
// The daemon's params carry the questions verbatim as acp types, so this is a
// pure decode-and-apply with no projection. A decode failure, or params with no
// session id, is a protocol drift — dropped like any other malformed
// notification (see handleNotification's doc), never a crash.
func (r *Reconstructor) handleDecisionRequested(raw json.RawMessage) {
	var w decisionRequestedWire
	if err := json.Unmarshal(raw, &w); err != nil || w.SessionID == "" || w.ID == "" {
		return
	}
	rec := r.session(w.SessionID)
	if rec == nil {
		return // reconstructor closing: drop the update
	}
	rec.decisions.Apply(decision.Update{
		Kind: decision.UpdateRequested,
		Request: decision.Request{
			ID:        w.ID,
			SessionID: w.SessionID,
			Questions: w.Questions,
		},
	})
}

// handleDecisionResolved reconstructs a gofer/decision_resolved notification —
// see handleDecisionRequested for the shared design. Only the ids are
// meaningful on a resolution (see [decision.Update]): it tells a client to stop
// rendering the prompt, whoever answered it and however.
func (r *Reconstructor) handleDecisionResolved(raw json.RawMessage) {
	var w decisionResolvedWire
	if err := json.Unmarshal(raw, &w); err != nil || w.SessionID == "" || w.ID == "" {
		return
	}
	rec := r.session(w.SessionID)
	if rec == nil {
		return // reconstructor closing: drop the update
	}
	rec.decisions.Apply(decision.Update{
		Kind:    decision.UpdateResolved,
		Request: decision.Request{ID: w.ID, SessionID: w.SessionID},
	})
}

// decisionRequestedWire decodes a gofer/decision_requested notification's
// params — internal/daemon/wire.go's decisionRequestedParams:
// {"sessionId","id","questions"}. The questions decode straight into the acp
// types the daemon marshalled them from; this core adds nothing to them.
type decisionRequestedWire struct {
	SessionID string                 `json:"sessionId"`
	ID        string                 `json:"id"`
	Questions []acp.DecisionQuestion `json:"questions"`
}

// decisionResolvedWire decodes a gofer/decision_resolved notification's params
// — internal/daemon/wire.go's decisionResolvedParams: {"sessionId","id"}.
type decisionResolvedWire struct {
	SessionID string `json:"sessionId"`
	ID        string `json:"id"`
}
