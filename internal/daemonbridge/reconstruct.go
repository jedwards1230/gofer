package daemonbridge

import (
	"context"
	"encoding/json"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// This file reconstructs each session's typed [event.Event] stream from the
// daemon's ACP session/update notifications, and drives the turn lifecycle
// (TurnStarted/TurnFinished) that only [Supervisor.Send] — the goroutine
// holding the blocking session/prompt Call and its PromptResponse — knows
// the outcome of. There is no reverse ACP→event.Event projection in the SDK's
// acp package (it is written for gofer to play the agent/server role, not
// the client role); this is gofer's own client-side projection.
//
// # Single demuxer, one goroutine, two inputs
//
// [New] starts exactly one demux goroutine. It is the sole reader of
// [daemon.Client.Notifications] (required: Client's doc states any caller
// issuing a call that streams notifications — session/prompt — needs a peer
// goroutine draining Notifications concurrently, or the read loop stalls
// behind a full buffer) and the sole reader of turnEndCh, the internal
// channel [Supervisor.Send] posts its turn's outcome to once the daemon's
// session/prompt Call resolves. Because it is the only goroutine that ever
// mutates a sessionState's open-message fields or publishes to a session's
// broker for the reconstruction path, event ordering within one session's
// stream is entirely determined by this goroutine's own sequential
// execution — no lock is needed for that state (see sessionState's doc).
//
// One shared demuxer across all sessions has a bounded head-of-line
// characteristic worth naming: it publishes must-deliver events into per-session
// brokers, and [event.Broker] blocks a publish up to its block-bound (5s in the
// SDK default) on a subscriber whose buffer is full before force-unsubscribing
// it. So a single wedged TUI subscriber can stall reconstruction — and, if the
// 64-slot Notifications buffer then fills, in-flight control Calls — for other
// sessions for up to that bound. It is bounded (the SDK force-drops the wedged
// subscriber and the demuxer resumes) and low-likelihood in M2 (deltas ride the
// lossy tier and never block; only a backlog of must-deliver events could
// trigger it), and is accepted for M2; a per-session demuxer would remove it.
// Relatedly, sessionState entries are created on first reference and not reaped
// on Kill/Archive — bounded by the process lifetime of one TUI session, also
// accepted for M2.
//
// # The TurnFinished-vs-last-delta ordering guarantee
//
// The daemon's handleSessionPrompt (internal/daemon/handlers.go) writes every
// session/update notification for a turn to the wire, synchronously, BEFORE
// it writes the terminating session/prompt JSON-RPC response (it literally
// cannot do otherwise: the response is only sent once the handler observes
// the turn's terminal event, and every event up to and including that one is
// first pushed out as a notification). [daemon.Client]'s single read loop
// reads frames strictly in wire order and, for a notification frame, SENDS it
// on the (buffered, capacity 64) Notifications channel BEFORE it advances to
// read the next frame. So the send of the turn's last notification onto that
// channel is program-order-before, and therefore happens-before, the read
// loop's later delivery of the matching response — which is what unblocks
// [Supervisor.Send]'s Call and lets it post to turnEndCh.
//
// That establishes: by the time turnEndCh's send for a turn occurs, the
// turn's last notification has ALREADY been sent onto Notifications — it is
// either (a) already popped and forwarded into this session's reconstruction
// by an earlier iteration of this goroutine (ordering trivially holds), or
// (b) still sitting in the Notifications channel's buffer, not yet popped.
// handleTurnEnd's first action is [Supervisor.drainNotifications]: a
// non-blocking, exhaustive drain of Notifications run BY THIS SAME
// goroutine, synchronously, before it does anything else for the turn-end.
// Since this goroutine is Notifications' only consumer, a value already sent
// onto it cannot be lost or reordered out from under a later non-blocking
// receive attempt by that same sole consumer — case (b)'s pending
// notification is therefore guaranteed to be drained (and its delta/tool
// event published) before handleTurnEnd flushes the open message and
// publishes TurnFinished. There is no residual race: this holds for every
// interleaving of the two producer goroutines (the daemon.Client read loop,
// and Send's goroutine), because it rests only on ordinary Go channel
// semantics (a sent value persists until some receive takes it; a single
// consumer cannot miss what it hasn't yet received) plus the wire-order
// invariant above — not on scheduling luck.
func (s *Supervisor) demux() {
	defer s.wg.Done()
	defer s.closeAllBrokers()
	for {
		select {
		case n, ok := <-s.client.Notifications():
			if !ok {
				return
			}
			s.handleNotification(n)
		case te := <-s.turnEndCh:
			s.drainNotifications()
			s.handleTurnEnd(te)
		}
	}
}

// drainNotifications forwards every notification currently buffered on
// Notifications, without blocking, then returns as soon as none is
// immediately available. See demux's doc for why this is the linchpin of the
// TurnFinished ordering guarantee.
func (s *Supervisor) drainNotifications() {
	for {
		select {
		case n, ok := <-s.client.Notifications():
			if !ok {
				return
			}
			s.handleNotification(n)
		default:
			return
		}
	}
}

// closeAllBrokers closes every session's reconstructed broker once the
// client connection (and so the demuxer) has shut down, so any still-live
// Subscribe channel observes a clean close instead of hanging forever.
func (s *Supervisor) closeAllBrokers() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.sessions {
		rec.broker.Close()
	}
}

// turnEnd carries one session/prompt Call's outcome from [Supervisor.Send]'s
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
// internal/tui/app.go's doSend): it publishes a synthesized TurnStarted
// immediately, launches the actual session/prompt Call on its own goroutine,
// and returns. The Call blocks server-side for the whole turn — the daemon
// streams every event as a session/update notification the demuxer
// reconstructs — and resolves once the turn reaches a terminal stop reason.
// When it does, the goroutine posts the outcome to turnEndCh; the demuxer
// flushes any open message and publishes the terminal
// SessionError/TurnFinished pair (see handleTurnEnd).
//
// The prompt Call runs against context.Background(), not ctx: like
// cmd/gofer's driveDaemonSession, a turn started this way outlives the
// call that started it (the App always calls Send with context.Background()
// itself — see doSend — since Send is meant to keep running after the TUI
// event loop has moved on to render other state).
func (s *Supervisor) Send(_ context.Context, sessionID, prompt string) error {
	rec := s.session(sessionID)
	rec.broker.Publish(event.NewTurnStarted(sessionID))

	go func() {
		raw, err := s.client.Call(context.Background(), acp.MethodSessionPrompt, acp.PromptRequest{
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
		case s.turnEndCh <- te:
		case <-s.closed:
		}
	}()
	return nil
}

// handleTurnEnd flushes any still-open message and publishes the terminal
// event(s) for one turn: SessionError+TurnFinished(stop="error") on any Call
// failure, or TurnFinished(stop=te.stopReason) on success. Usage is always
// the zero value — ACP's PromptResponse carries no token/cost accounting;
// the roster's gofer/roster row is the daemon's authoritative source for
// cost/usage (see Roster), refreshed by the App's 1s poll.
func (s *Supervisor) handleTurnEnd(te turnEnd) {
	rec := s.session(te.sessionID)
	s.flushOpenMessage(rec)

	if te.err != "" {
		rec.broker.Publish(event.NewSessionError(te.sessionID, te.err, true))
		rec.broker.Publish(event.NewTurnFinished(te.sessionID, "error", provider.Usage{}))
		return
	}
	rec.broker.Publish(event.NewTurnFinished(te.sessionID, te.stopReason, provider.Usage{}))
}

// handleNotification decodes one inbound notification and applies it to its
// session's reconstruction state. Only session/update notifications carry
// reconstructable state (the M2 daemon never sends any other notification
// method — see internal/daemon's package doc); anything else, or anything
// that fails to decode (a protocol drift, not a reason to crash the
// reconstruction), is dropped.
func (s *Supervisor) handleNotification(n daemon.Notification) {
	if n.Method != acp.MethodSessionUpdate {
		return
	}
	var w notificationWire
	if err := json.Unmarshal(n.Params, &w); err != nil || w.SessionID == "" {
		return
	}
	var disc updateDisc
	if err := json.Unmarshal(w.Update, &disc); err != nil {
		return
	}

	rec := s.session(w.SessionID)
	switch disc.SessionUpdate {
	case "agent_message_chunk":
		s.appendDelta(rec, event.MessageText, w.Update)
	case "agent_thought_chunk":
		s.appendDelta(rec, event.MessageReasoning, w.Update)
	case "tool_call":
		s.handleToolCall(rec, w.Update)
	case "tool_call_update":
		s.handleToolCallUpdate(rec, w.Update)
	default:
		// Unrecognized/future session/update variant (e.g. user_message_chunk,
		// which this daemon never emits per acp.ToSessionUpdate's doc): no
		// event.Event projection exists for it in the minimal attach surface,
		// so it is accepted and ignored, mirroring tui.Model.Ingest's own
		// tolerance of event kinds it doesn't render.
	}
}

// appendDelta opens a message of kind (flushing a different-kind or
// already-open one first — see flushOpenMessage) if none is open, then
// applies one delta. Model.Ingest requires a MessageStarted before it will
// apply a MessageDelta to any item (see its openIndex/setOpen bookkeeping),
// so a MessageStarted is synthesized here whenever a chunk arrives with none
// already open for its kind.
func (s *Supervisor) appendDelta(rec *sessionState, kind event.MessageKind, raw json.RawMessage) {
	var c contentChunkWire
	_ = json.Unmarshal(raw, &c) // best-effort: a decode failure just yields an empty delta

	if !rec.hasOpen || rec.openKind != kind {
		s.flushOpenMessage(rec)
		rec.broker.Publish(event.NewMessageStarted(rec.id, kind))
		rec.hasOpen = true
		rec.openKind = kind
		rec.text = ""
	}
	rec.text += c.Content.Text
	rec.broker.Publish(event.NewMessageDelta(rec.id, kind, c.Content.Text))
}

// flushOpenMessage closes the currently open message, if any, with the text
// accumulated from its deltas — mirroring Model.Ingest's MessageFinished
// handling, which replaces the streamed text with the finished event's
// authoritative Content. Called before a kind change, a tool_call (a tool
// call always interrupts any in-progress text/reasoning stream), and a turn
// end.
func (s *Supervisor) flushOpenMessage(rec *sessionState) {
	if !rec.hasOpen {
		return
	}
	rec.broker.Publish(event.NewMessageFinished(rec.id, rec.openKind, rec.text))
	rec.hasOpen = false
	rec.text = ""
}

// handleToolCall flushes any open message (a tool call always ends the
// in-progress text/reasoning stream, per ToSessionUpdate's emission order)
// and publishes the reconstructed ToolCallStarted.
func (s *Supervisor) handleToolCall(rec *sessionState, raw json.RawMessage) {
	s.flushOpenMessage(rec)
	var tc toolCallWire
	_ = json.Unmarshal(raw, &tc)
	rec.broker.Publish(event.NewToolCallStarted(rec.id, tc.ToolCallID, tc.Title, tc.RawInput))
}

// handleToolCallUpdate publishes a reconstructed ToolCallFinished for a
// terminal (completed/failed) tool_call_update; a non-terminal status
// (pending/in_progress) has no event.Event projection in the minimal attach
// surface and is ignored.
func (s *Supervisor) handleToolCallUpdate(rec *sessionState, raw json.RawMessage) {
	var tc toolCallUpdateWire
	_ = json.Unmarshal(raw, &tc)
	if tc.Status != "completed" && tc.Status != "failed" {
		return
	}
	rec.broker.Publish(event.NewToolCallFinished(rec.id, tc.ToolCallID, tc.resultText(), tc.Status == "failed", nil))
}

// notificationWire is the wire shape of a session/update notification's
// params, decoded loosely (mirroring internal/daemon/prompt_test.go's
// sessionUpdateParams and cmd/gofer/acprender.go's acpUpdateWire, both of
// which take the same approach for the same reason: acp has no client-side
// decoder for the ACP messages gofer sends AS a client, only the ones it
// receives playing the agent/server role).
type notificationWire struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
}

// updateDisc decodes just the "sessionUpdate" discriminator shared by every
// session/update variant.
type updateDisc struct {
	SessionUpdate string `json:"sessionUpdate"`
}

// contentChunkWire decodes the agent_message_chunk / agent_thought_chunk
// shape: {"content":{"type":"text","text":...}}. Decoded independently of
// toolCallWire/toolCallUpdateWire against the same raw update bytes, since
// "content" means a different JSON shape (a single object here, an array of
// tagged variants there) depending on the variant.
type contentChunkWire struct {
	Content struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

// toolCallWire decodes the tool_call fields the reconstruction needs:
// {"toolCallId":...,"title":...,"rawInput":...}.
type toolCallWire struct {
	ToolCallID string          `json:"toolCallId"`
	Title      string          `json:"title"`
	RawInput   json.RawMessage `json:"rawInput"`
}

// toolCallUpdateWire decodes the tool_call_update fields the reconstruction
// needs: {"toolCallId":...,"status":...,"content":[{"type":"content",
// "content":{"type":"text","text":...}}]} — see acp.ToolCallContentBlock's
// wire shape.
type toolCallUpdateWire struct {
	ToolCallID string `json:"toolCallId"`
	Status     string `json:"status"`
	Content    []struct {
		Type    string `json:"type"`
		Content struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"content"`
}

// resultText extracts the first text content block's text from a
// tool_call_update's content array, or "" if it carries none (e.g. a failed
// call with no output) — the shape [acp.ToSessionUpdate] emits for
// [event.ToolCallFinished].
func (w toolCallUpdateWire) resultText() string {
	for _, c := range w.Content {
		if c.Type == "content" && c.Content.Type == "text" {
			return c.Content.Text
		}
	}
	return ""
}
