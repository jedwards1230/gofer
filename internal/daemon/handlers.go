package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// gofer-native control methods, namespaced so they never collide with an ACP
// method name. They serve the CLI client (a later PR): roster/ps mirror
// [supervisor.Supervisor.Roster]/[supervisor.Supervisor.List], kill/archive
// mirror the lifecycle operations.
const (
	methodGoferRoster  = "gofer/roster"
	methodGoferPS      = "gofer/ps"
	methodGoferKill    = "gofer/kill"
	methodGoferArchive = "gofer/archive"

	// methodGoferPermissionRequested / methodGoferPermissionResolved are the
	// gofer-native notifications the daemon fans a session's permission events
	// out to every attached peer with. ACP deliberately keeps permission.*
	// outside its session/update surface (see acp.ToSessionUpdate) and models a
	// request as a client-answered REQUEST, which does not fit a must-deliver
	// fan-out to N peers — so gofer emits its own notifications instead. Their
	// params are a lossless projection of event.PermissionRequested /
	// event.PermissionResolved (plus the session id for routing), so a client
	// reconstructs the events directly (see internal/daemonbridge).
	methodGoferPermissionRequested = "gofer/permission_requested"
	methodGoferPermissionResolved  = "gofer/permission_resolved"

	// methodGoferEvent is the gofer-native, full-fidelity notification the M3
	// lossless-attach work fans a session's ENTIRE non-permission event stream
	// out through: its params are the source [event.Event]'s own MarshalJSON
	// envelope, verbatim, sent UNIFORMLY to every attached peer alongside (not
	// instead of) the lossy acp.ToSessionUpdate projection on session/update —
	// see broadcastGoferEvent and handleSessionPrompt. An ACP-only client
	// ignores this unknown notification (per JSON-RPC 2.0); a gofer client
	// (internal/daemonbridge) ignores session/update instead and reconstructs
	// its Event stream from this one, byte-exactly, via the SDK's exported
	// event.New* constructors. Mirrors the gofer/permission_* precedent, just
	// for every OTHER event kind too, so nothing the daemon observes is lost
	// to a gofer client the way it is to an ACP one (turn.started, session.error,
	// message.started, tool.call.delta, and ToolCallFinished's
	// Diagnostics/Spill* fields all have no session/update projection at all —
	// see acp.ToSessionUpdate).
	methodGoferEvent = "gofer/event"

	// methodPermissionReply is the inbound op a client sends to answer a
	// permission request (contract: JSON-RPC method "permission.reply", params
	// {id, verdict, remember?}). It is a notification — no result.
	methodPermissionReply = "permission.reply"
)

// methodHandler answers one JSON-RPC method call. params is the raw request
// params (nil for a method that takes none). The router (see [peer.handleFrame])
// sends the returned result/error as the response for a request and discards
// both for a notification.
type methodHandler func(d *Daemon, ctx context.Context, p *peer, params json.RawMessage) (any, *rpcError)

// methodTable is the daemon's full method router: ACP methods projected
// through the SDK's acp package, plus the gofer-native control methods.
var methodTable = map[string]methodHandler{
	acp.MethodInitialize:    handleInitialize,
	acp.MethodAuthenticate:  handleAuthenticate,
	acp.MethodSessionNew:    handleSessionNew,
	acp.MethodSessionLoad:   handleSessionLoad,
	acp.MethodSessionPrompt: handleSessionPrompt,
	acp.MethodSessionCancel: handleSessionCancel,
	acp.MethodSessionList:   handleSessionList,

	methodGoferRoster:  handleGoferRoster,
	methodGoferPS:      handleGoferPS,
	methodGoferKill:    handleGoferKill,
	methodGoferArchive: handleGoferArchive,

	methodPermissionReply: handlePermissionReply,
}

// decodeOp decodes method's params via acp.DecodeOp and asserts the result to
// T, the concrete event.Op type that method projects to (per the mapping in
// acp.DecodeOp's doc). The ok=false/type-mismatch branches are daemon bugs —
// a drift between this table and acp.DecodeOp's method set — surfaced as an
// internal error rather than a panic.
func decodeOp[T event.Op](method string, params json.RawMessage) (T, *rpcError) {
	var zero T
	op, ok, err := acp.DecodeOp(method, params)
	if err != nil {
		return zero, invalidParams(err)
	}
	if !ok {
		return zero, internalErr(fmt.Errorf("acp: %s decoded no op", method))
	}
	typed, ok := op.(T)
	if !ok {
		return zero, internalErr(fmt.Errorf("acp: %s decoded unexpected op type %T", method, op))
	}
	return typed, nil
}

// handleInitialize negotiates the protocol version. It always replies at
// [acp.ProtocolVersion]; a client that cannot speak it is expected to
// disconnect. The response advertises session/load (with full-history
// replay, see handleSessionLoad) and session/list support.
func handleInitialize(_ *Daemon, _ context.Context, _ *peer, params json.RawMessage) (any, *rpcError) {
	if _, err := acp.DecodeInitialize(params); err != nil {
		return nil, invalidParams(err)
	}
	resp := acp.NewInitializeResponse()
	resp.AgentCapabilities = acp.AgentCapabilities{
		LoadSession:         true,
		SessionCapabilities: acp.SessionCapabilities{List: &acp.SessionListCapabilities{}},
	}
	return resp, nil
}

// handleAuthenticate is a no-op success: the WebSocket bearer token (see
// [Daemon.authorized]) is the daemon's only auth boundary in M2, checked
// before the connection is even upgraded.
func handleAuthenticate(_ *Daemon, _ context.Context, _ *peer, _ json.RawMessage) (any, *rpcError) {
	return struct{}{}, nil
}

// cwdErrRef formats a cwd for an invalid-params error: always the raw string
// the client sent (so they can match the error to what they typed), plus the
// resolved path when a "~" expansion or filepath.Clean changed it (so they
// also see where it actually pointed — the exact ambiguity behind the literal
// "~/orchestration" bug). Used for every resolveSessionCwd rejection so the
// messages stay consistent.
func cwdErrRef(raw, resolved string) string {
	if resolved == raw {
		return fmt.Sprintf("%q", raw)
	}
	return fmt.Sprintf("%q (resolved to %q)", raw, resolved)
}

// normalizeCwd is the ONE normalization rule every client-supplied cwd is put
// through before it is stored (session creation/load, via [resolveSessionCwd]):
// a leading "~" or "~/" is expanded against the daemon's own home, then the
// result is filepath.Clean'd. It is the inner, validation-free step
// resolveSessionCwd layers its absolute/exists/is-a-directory checks and
// empty-cwd default on top of for the create/load paths.
//
// If the daemon's home directory can't be resolved (os.UserHomeDir failing —
// vanishingly rare in practice, since it just reads $HOME/USERPROFILE), a
// "~"-prefixed raw is left unexpanded rather than erroring: normalizeCwd has
// no error return, so downstream callers just see it fail their own
// validation (resolveSessionCwd's IsAbs check rejects it as a real problem) —
// an acceptable outcome for a condition this unlikely.
func normalizeCwd(raw string) string {
	cwd := raw
	if raw == "~" || strings.HasPrefix(raw, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			if raw == "~" {
				cwd = home
			} else {
				cwd = filepath.Join(home, raw[2:])
			}
		}
	}
	return filepath.Clean(cwd)
}

// resolveSessionCwd validates and normalizes an ACP session cwd. ACP v1
// requires cwd to be an absolute path (both NewSessionRequest.cwd and
// LoadSessionRequest.cwd — src/v1/agent.rs); as a DX nicety for phone clients
// that let a user type a path, a leading "~" or "~/" is expanded against the
// daemon's own home (see [normalizeCwd]). An empty cwd defaults to the
// daemon's own working directory (os.Getwd) — the same effective root a
// zero-value [supervisor.CreateOptions]/[supervisor.ResumeOptions] has always
// resolved to (see their doc comments), now explicit and validated here
// rather than left to flow down unchecked. The result must be an existing
// directory; otherwise a clear invalid-params error naming the path (raw,
// plus the resolved form when they differ — see [cwdErrRef]) is returned
// instead of creating a session whose every tool call silently fails (the
// live bug this guards: an ACP client sending the literal, unexpanded string
// "~/orchestration" as cwd).
func resolveSessionCwd(raw string) (string, *rpcError) {
	if strings.TrimSpace(raw) == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", internalErr(fmt.Errorf("session cwd: resolve daemon working directory: %w", err))
		}
		return cwd, nil
	}

	cwd := normalizeCwd(raw)

	if !filepath.IsAbs(cwd) {
		return "", invalidParamsMsg(fmt.Sprintf("session cwd %s must be an absolute path (a leading ~ is expanded to the daemon's home)", cwdErrRef(raw, cwd)))
	}

	fi, err := os.Stat(cwd)
	if err != nil {
		return "", invalidParamsMsg(fmt.Sprintf("session cwd %s does not exist: %v", cwdErrRef(raw, cwd), err))
	}
	if !fi.IsDir() {
		return "", invalidParamsMsg(fmt.Sprintf("session cwd %s is not a directory", cwdErrRef(raw, cwd)))
	}
	return cwd, nil
}

// handleSessionNew creates an idle session (no first turn — the prompt
// arrives via a subsequent session/prompt) and replies its id.
func handleSessionNew(d *Daemon, ctx context.Context, _ *peer, params json.RawMessage) (any, *rpcError) {
	op, rerr := decodeOp[event.SessionNew](acp.MethodSessionNew, params)
	if rerr != nil {
		return nil, rerr
	}
	cwd, rerr := resolveSessionCwd(op.Cwd)
	if rerr != nil {
		return nil, rerr
	}
	info, err := d.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: cwd, Model: d.cfg.DefaultModel})
	if err != nil {
		return nil, appError(err)
	}
	d.log.Info("session created", "session", info.ID)
	return acp.NewSessionResponse{SessionID: info.ID}, nil
}

// handleSessionLoad reopens a persisted session and replays its folded
// conversation history as session/update notifications before returning the
// session/load response, per ACP v1's "the Agent MUST replay the entire
// conversation to the Client in the form of session/update notifications"
// requirement. As of the M3 lossless-attach work, it ALSO replays the same
// folded history as gofer/event notifications (see historyEvents and
// methodGoferEvent's doc) — the ACP replay serves an ACP client, the
// gofer/event replay serves a gofer client (internal/daemonbridge), which
// ignores session/update entirely; both go out to this peer before the
// response, per the SAME ordering contract.
//
// Ordering contract (spec-critical): every replay notification is written
// strictly before this handler's response reaches the wire. This holds
// without extra synchronization here because [peer.handleFrame] sends the
// handler's returned result as the response only AFTER the handler itself
// returns, and every frame — this handler's notify calls and handleFrame's
// eventual reply — goes through the same p.writeMu-serialized [peer.writeJSON]
// path, so they can never interleave or reorder on the wire.
//
// Concurrency note: replay reads Fold(), a snapshot of the settled journal
// taken at call time. A concurrent session/prompt on the same session (a
// client is not expected to do this mid-load, but nothing here forbids it)
// streams NEW notifications from its own goroutine onto the same peer; both
// paths only ever reach the wire through p.notify -> p.writeJSON, whose
// writeMu already serializes frame-by-frame, so no additional locking is
// needed to keep replay and live streaming from corrupting each other's
// frames — only their relative ordering (which is unspecified once a prompt
// races a load) is left unguaranteed, and ACP does not require otherwise.
//
// Cwd precedence: ACP v1's LoadSessionRequest.cwd is REQUIRED and is the
// working directory to reload the session into (src/v1/agent.rs), so the
// client-supplied cwd is authoritative here — via resolveSessionCwd, same as
// session/new — even though [supervisor.Supervisor.List] can now also read a
// session's persisted cwd back from its journal (see [handleSessionList]).
// The persisted cwd is for LISTING (so a client can discover/filter sessions
// before it has picked one to load), not for overriding what the client
// explicitly asks to load into; a client is expected to always send a cwd on
// load per spec. When it sends none, resolveSessionCwd's existing empty-cwd
// fallback (the daemon's own working directory) applies unchanged — that
// fallback predates this change and is not replaced by the persisted cwd.
func handleSessionLoad(d *Daemon, ctx context.Context, p *peer, params json.RawMessage) (any, *rpcError) {
	op, rerr := decodeOp[event.SessionResume](acp.MethodSessionLoad, params)
	if rerr != nil {
		return nil, rerr
	}
	// event.SessionResume carries only the session id (see acp.FromLoadSession);
	// Cwd, which Resume needs, lives only on the raw request, so it is decoded
	// again here.
	var req acp.LoadSessionRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, invalidParams(fmt.Errorf("acp: decode %s params: %w", acp.MethodSessionLoad, err))
	}
	cwd, rerr := resolveSessionCwd(req.Cwd)
	if rerr != nil {
		return nil, rerr
	}
	if _, err := d.sup.Resume(ctx, op.SessionID, supervisor.ResumeOptions{Cwd: cwd, Model: d.cfg.DefaultModel}); err != nil {
		return nil, appError(err)
	}
	d.log.Info("session resumed", "session", op.SessionID)

	// This peer is now attached to the session: register it in the fan-out
	// registry so it receives live session/update notifications for any turn
	// ANY peer drives, not just its own. Deregistered on connection close (see
	// [Daemon.detachPeer]). Registering after Resume succeeds — a load that
	// failed above never reaches here — means the registry only ever holds
	// peers attached to a session the daemon actually resumed.
	d.attachPeer(op.SessionID, p)

	msgs, err := d.sup.History(ctx, op.SessionID)
	if err != nil {
		return nil, appError(err)
	}
	notifs := acp.ReplayNotifications(op.SessionID, msgs)
	// This handler never subscribes to the session's event broker — it
	// replays the folded journal directly — so it never triggers the
	// broker's retained-backlog replay handleSessionPrompt guards against.
	// The subsequent session/prompt on this now-loaded session uses
	// SubscribeLive, so it won't re-deliver this history as a duplicate
	// broker-replayed session/update either; the two replay paths are
	// disjoint by construction.
	d.log.Debug("session load replay", "session", op.SessionID, "notifications", len(notifs))
	for _, n := range notifs {
		if werr := p.notify(ctx, acp.MethodSessionUpdate, n); werr != nil {
			return nil, internalErr(fmt.Errorf("session/load %s: write replay session/update: %w", op.SessionID, werr))
		}
	}

	// ALSO replay the same folded history as gofer/event frames (see
	// historyEvents and methodGoferEvent's doc), so a gofer client — which
	// ignores session/update entirely — still gets a full history replay on
	// attach, not just an ACP one. Both replays go out to THIS peer before
	// this handler returns its response, preserving the same
	// notifications-before-response wire-order guarantee documented above.
	events := historyEvents(op.SessionID, msgs)
	d.log.Debug("session load gofer/event replay", "session", op.SessionID, "events", len(events))
	for _, ev := range events {
		raw, merr := json.Marshal(ev)
		if merr != nil {
			return nil, internalErr(fmt.Errorf("session/load %s: marshal replay gofer/event: %w", op.SessionID, merr))
		}
		if werr := p.notify(ctx, methodGoferEvent, json.RawMessage(raw)); werr != nil {
			return nil, internalErr(fmt.Errorf("session/load %s: write replay gofer/event: %w", op.SessionID, werr))
		}
	}
	return acp.LoadSessionResponse{}, nil
}

// handleSessionCancel interrupts id's in-flight turn. It is normally sent as
// a notification (no id, no reply); the router replies only if the client
// happened to send one with an id anyway.
func handleSessionCancel(d *Daemon, ctx context.Context, _ *peer, params json.RawMessage) (any, *rpcError) {
	op, rerr := decodeOp[event.TurnInterrupt](acp.MethodSessionCancel, params)
	if rerr != nil {
		return nil, rerr
	}
	if err := d.sup.Interrupt(ctx, op.SessionID); err != nil {
		return nil, appError(err)
	}
	return struct{}{}, nil
}

// sessionListPageSize bounds a single session/list response's Sessions slice.
const sessionListPageSize = 50

// handleSessionList answers session/list: every on-disk session (live and
// archived — see [supervisor.Supervisor.List]), newest-first, opaquely
// paginated at [sessionListPageSize] entries per page.
//
// Listing is FLEET-GLOBAL: a session is returned regardless of its cwd, so a
// client on any device discovers every session the daemon supervises — the
// whole point of a shared daemon several clients attach to (a turn driven from
// a phone must be visible to a laptop's roster). req.Cwd is accepted for wire
// compatibility but IGNORED — it is a label on each returned session, not a
// filter. (It was historically a hiding filter; that made a shared daemon's
// sessions invisible to a client sitting in a different directory, so it was
// dropped.) Every returned [acp.SessionInfo] still carries its Cwd so a client
// can group or label sessions by directory itself.
//
// A disk-only (archived, or simply not yet resumed since the daemon last
// started) [supervisor.SessionInfo] carries its Cwd, Title, and Updated read
// back from the journal — see [supervisor.Supervisor.List]'s doc — so those
// sessions survive a daemon restart with a real title instead of a bare id.
// The only entries still missing Cwd are legacy journals written before the
// SDK started persisting it (no session_meta entry); those fall back to the
// daemon's own working directory below.
func handleSessionList(d *Daemon, ctx context.Context, _ *peer, params json.RawMessage) (any, *rpcError) {
	var req acp.ListSessionsRequest
	if len(params) > 0 {
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, invalidParams(fmt.Errorf("acp: decode %s params: %w", acp.MethodSessionList, err))
		}
	}

	offset, err := decodeSessionCursor(req.Cursor)
	if err != nil {
		return nil, invalidParamsMsg(err.Error())
	}

	infos, err := d.sup.List(ctx)
	if err != nil {
		return nil, appError(err)
	}

	sort.Slice(infos, func(i, j int) bool {
		if !infos[i].Updated.Equal(infos[j].Updated) {
			return infos[i].Updated.After(infos[j].Updated)
		}
		return infos[i].ID > infos[j].ID
	})

	if offset > len(infos) {
		offset = len(infos)
	}
	end := offset + sessionListPageSize
	if end > len(infos) {
		end = len(infos)
	}
	page := infos[offset:end]

	// daemonCwd is the fallback for a legacy disk-only entry whose journal
	// predates the SDK's session_meta cwd persistence (see the doc above);
	// resolved once, best-effort — an error here just leaves the fallback ""
	// rather than failing the whole request.
	daemonCwd, _ := os.Getwd()

	sessions := make([]acp.SessionInfo, 0, len(page))
	for _, info := range page {
		cwd := info.Cwd
		if cwd == "" {
			cwd = daemonCwd
		}
		sessions = append(sessions, acp.SessionInfo{
			SessionID: info.ID,
			Cwd:       cwd,
			Title:     info.Title,
			UpdatedAt: info.Updated.UTC().Format(time.RFC3339),
		})
	}

	resp := acp.ListSessionsResponse{Sessions: sessions}
	if end < len(infos) {
		resp.NextCursor = encodeSessionCursor(end)
	}
	return resp, nil
}

// handleSessionPrompt is the streaming heart of the daemon. It subscribes to
// the session's event stream BEFORE dispatching the prompt (so no event
// between dispatch and subscribe is missed), sends the prompt, and then
// drains the subscription: every event that projects to a session/update (see
// [acp.ToSessionUpdate]) is pushed to the client as a notification, and the
// handler returns as soon as it observes a terminal turn.finished (one whose
// stop reason [acp.ToPromptResponse] projects — end_turn, max_tokens,
// refusal, or cancelled).
//
// It subscribes via [supervisor.Supervisor.SubscribeLive], not Subscribe: the
// broker replays its retained must-deliver backlog (lifecycle + terminal
// events) into every new Subscribe-based subscription, a feature meant for
// mid-session attach/peek recovering events it missed. A plain Subscribe here
// would instead hand THIS prompt a PRIOR turn's retained turn.finished, which
// the wait loop below would consume immediately and return as this prompt's
// own response in ~0ms — the second prompt on a connection would appear to
// resolve instantly with no session/update at all, while the actual turn
// streamed into a subscription nobody was reading (torn down by the deferred
// sub.Close() before it produced anything). SubscribeLive delivers only
// events published after the call, so combined with subscribing BEFORE
// sending the prompt, this handler observes exactly this turn's events and
// nothing from any earlier one.
//
// M2 contract: one outstanding session/prompt per session. A turn.finished
// with stop reason "tool_use" is mid-turn (the loop is about to run tool
// calls and make another model call within this same dispatch) and does not
// end the wait. Because [supervisor.Supervisor.Subscribe] fans the session's
// whole stream out to every subscriber, a second concurrent session/prompt
// for the same session would race this one for whichever turn.finished comes
// next — callers must wait for a response (or the connection closing) before
// sending another session/prompt for the same session id.
//
// A known M2 limitation: [event.SessionError] with Fatal=true almost always
// precedes a same-turn error/cancelled turn.finished (see the SDK's loop
// package), which this handler resolves on normally. The one path that does
// not — an Interrupt landing in the narrow window between two model-call
// iterations of a tool-using turn — emits only the fatal session.error with
// no turn.finished at all, which would leave this request pending until the
// connection closes. It is not special-cased here because doing so would
// require guessing whether a given fatal session.error is terminal, and a
// wrong guess would end the wait early on the far more common path where
// StopReasonCancelled still projects to a normal PromptResponse.
func handleSessionPrompt(d *Daemon, ctx context.Context, p *peer, params json.RawMessage) (any, *rpcError) {
	op, rerr := decodeOp[event.PromptSend](acp.MethodSessionPrompt, params)
	if rerr != nil {
		return nil, rerr
	}

	sub, err := d.sup.SubscribeLive(ctx, op.SessionID)
	if err != nil {
		return nil, appError(fmt.Errorf("session/prompt %s: %w", op.SessionID, err))
	}
	defer sub.Close()

	// Attach the originator so it is part of this session's fan-out set for
	// subsequent turns other peers drive too, even if it never called
	// session/load (a one-shot `run` that only ever prompts). Deregistered on
	// connection close.
	d.attachPeer(op.SessionID, p)

	if err := d.sup.Send(ctx, op.SessionID, op.Text); err != nil {
		return nil, appError(err)
	}

	var lastFatal string
	var updates int
	for {
		select {
		case e, ok := <-sub.C:
			if !ok {
				return nil, appError(fmt.Errorf("session/prompt %s: session ended before the turn finished", op.SessionID))
			}

			if se, isErr := e.(event.SessionError); isErr && se.Fatal {
				lastFatal = se.Err
			}

			// Permission events are outside ACP's session/update surface
			// (acp.ToSessionUpdate returns ok=false for them), so they would be
			// silently dropped by the projection below. Fan them out explicitly
			// as gofer-native notifications — a must-deliver path reaching EVERY
			// attached peer, so a phone can approve a turn a laptop drives (and
			// vice versa). Recording the call->session route here (where the
			// session is known) is what lets a later permission.reply, which
			// carries only the call id, find this session's gate.
			switch pe := e.(type) {
			case event.PermissionRequested:
				d.recordPermRoute(pe.ID, op.SessionID)
				d.broadcastPermission(ctx, op.SessionID, methodGoferPermissionRequested, permissionRequestedParams{
					SessionID: op.SessionID,
					ID:        pe.ID,
					Tool:      pe.Tool,
					Spec:      pe.Spec,
					Trace:     pe.Trace,
				})
				continue
			case event.PermissionResolved:
				d.clearPermRoute(pe.ID)
				d.broadcastPermission(ctx, op.SessionID, methodGoferPermissionResolved, permissionResolvedParams{
					SessionID: op.SessionID,
					ID:        pe.ID,
					Verdict:   string(pe.Verdict),
					Rule:      pe.Rule,
				})
				continue
			}

			// Every non-permission event reaches a gofer client's reconstruction
			// verbatim via gofer/event — see methodGoferEvent's doc — BEFORE the
			// lossy acp.ToSessionUpdate projectability check below, so a kind
			// ToSessionUpdate drops entirely (turn.started, session.error,
			// message.started, tool.call.delta, turn.finished, ...) still
			// crosses the wire for this path. This runs for turn.finished too
			// (see below): every event that reaches this point is fanned,
			// unconditionally, before any of this loop's return/continue
			// branches.
			d.broadcastGoferEvent(ctx, op.SessionID, e)

			if notif, ok := acp.ToSessionUpdate(op.SessionID, e); ok {
				// The user-message echo (a settled event.MessageUser projects
				// to user_message_chunk) is suppressed to the ORIGINATOR — the
				// peer that typed this prompt already knows what it sent, and a
				// daemonbridge client renders its own prompt from a local
				// injection instead (see internal/daemonbridge Send). Every
				// OTHER attached peer still receives it, so a phone or second
				// TUI sees what was typed on the driving client.
				isUserEcho := false
				if mf, isMF := e.(event.MessageFinished); isMF && mf.MessageKind == event.MessageUser {
					isUserEcho = true
				}
				if rerr := d.broadcastUpdate(ctx, op.SessionID, p, notif, isUserEcho); rerr != nil {
					return nil, rerr
				}
				updates++
			}

			tf, isTurnFinished := e.(event.TurnFinished)
			if !isTurnFinished {
				continue
			}
			if resp, ok := acp.ToPromptResponse(tf); ok {
				d.log.Debug("session prompt updates", "session", op.SessionID, "notifications", updates)
				return resp, nil
			}
			if tf.StopReason == "tool_use" {
				continue
			}
			msg := fmt.Sprintf("session/prompt %s: turn ended with stop reason %q", op.SessionID, tf.StopReason)
			if lastFatal != "" {
				msg = fmt.Sprintf("%s: %s", msg, lastFatal)
			}
			return nil, appError(errors.New(msg))

		case <-ctx.Done():
			return nil, appError(fmt.Errorf("session/prompt %s: %w", op.SessionID, ctx.Err()))
		}
	}
}

// broadcastUpdate fans one projected session/update out to every peer attached
// to sessionID (see [Daemon.peersForSession]) — the union of the registered
// set and origin, the peer driving this turn (origin is in the set: it was
// attached at the top of handleSessionPrompt). The peer set is snapshotted
// under the registry RLock and released before any notify (a socket write)
// runs, so a wedged client never stalls the registry.
//
// Two delivery rules differ by peer and by update:
//   - origin: the user-message echo (isUserEcho) is suppressed — origin already
//     knows its own prompt. Every other update is delivered, and a write
//     failure to origin is FATAL: the RPC's own response rides that same
//     connection, so a broken origin connection aborts the turn (returned as
//     an rpcError, matching the pre-fan-out single-peer behavior).
//   - every other attached peer: receives every update including the echo. A
//     write failure is NON-fatal — it is logged and skipped, and that peer's
//     own connection-close defer (see [Daemon.detachPeer]) removes it from the
//     registry. One disconnected observer must never abort a turn the origin
//     is still driving.
//
// origin is matched by pointer identity and handled exactly once, so it is
// never double-delivered even though it is also in the snapshot.
func (d *Daemon) broadcastUpdate(ctx context.Context, sessionID string, origin *peer, notif any, isUserEcho bool) *rpcError {
	for _, pr := range d.peersForSession(sessionID) {
		if pr == origin {
			continue // origin handled below with its distinct echo/error rules
		}
		if werr := pr.notify(ctx, acp.MethodSessionUpdate, notif); werr != nil {
			// DEBUG, not WARN: a peer disconnecting mid-turn is routine, not a
			// daemon fault. Session id only — never the notif (prompt/message
			// content); see handleFrame's redaction rule.
			d.log.Debug("session/update broadcast: peer notify failed", "session", sessionID, "err", werr)
		}
	}
	if isUserEcho {
		return nil // suppressed to the originator
	}
	if werr := origin.notify(ctx, acp.MethodSessionUpdate, notif); werr != nil {
		return internalErr(fmt.Errorf("session/prompt %s: write session/update: %w", sessionID, werr))
	}
	return nil
}

// broadcastPermission fans a permission notification out to EVERY peer attached
// to sessionID — including the origin peer driving the turn (a permission
// prompt is not an echo the origin already knows; it must see it to answer, and
// so must every other attached device). Unlike broadcastUpdate, a write failure
// to any single peer (origin included) is non-fatal and only logged: the turn
// is blocked in the loop awaiting the gate regardless of any one peer's socket,
// and a wedged observer must never abort a turn nor stop delivery to the other
// peers. The peer set is snapshotted under the registry RLock and released
// before any notify runs (see peersForSession).
func (d *Daemon) broadcastPermission(ctx context.Context, sessionID, method string, params any) {
	for _, pr := range d.peersForSession(sessionID) {
		if werr := pr.notify(ctx, method, params); werr != nil {
			// Session id + method only — never the params (tool input/spec);
			// see handleFrame's redaction rule.
			d.log.Debug("permission broadcast: peer notify failed", "session", sessionID, "method", method, "err", werr)
		}
	}
}

// broadcastGoferEvent fans e out to EVERY peer attached to sessionID as a
// gofer/event notification (see methodGoferEvent's doc) — e's own MarshalJSON
// envelope, verbatim, marshaled ONCE and reused for every peer. Modeled on
// broadcastPermission, not broadcastUpdate: there is no user-echo suppression
// or origin special-casing (the daemon ACP surface stays spec-general — every
// peer, including the one driving the turn, gets the identical frame, same as
// a permission broadcast), and a write failure to any single peer — origin
// included — is non-fatal and only logged, never aborting the turn a
// possibly-unrelated peer's wedged socket has nothing to do with. A marshal
// failure (e's own MarshalJSON erroring) is likewise non-fatal: the frame is
// simply skipped rather than aborting the turn over a client-visibility
// concern. The peer set is snapshotted under the registry RLock and released
// before any notify runs (see peersForSession).
func (d *Daemon) broadcastGoferEvent(ctx context.Context, sessionID string, e event.Event) {
	raw, err := json.Marshal(e)
	if err != nil {
		d.log.Debug("gofer/event broadcast: marshal failed", "session", sessionID, "kind", e.Kind(), "err", err)
		return
	}
	for _, pr := range d.peersForSession(sessionID) {
		if werr := pr.notify(ctx, methodGoferEvent, json.RawMessage(raw)); werr != nil {
			// Session id + kind only — never the marshaled event (may carry
			// prompt/message content); see handleFrame's redaction rule.
			d.log.Debug("gofer/event broadcast: peer notify failed", "session", sessionID, "kind", e.Kind(), "err", werr)
		}
	}
}

// handlePermissionReply answers a client's "permission.reply" op: it routes the
// verdict to the awaiting session's gate. The op carries only the call id (no
// session id — see event.PermissionReply), so the session is resolved from the
// call->session route the daemon recorded when it broadcast the request (see
// [Daemon.recordPermRoute]). It is a notification (no result); an unknown id or
// an already-resolved/gone session surfaces as an error the router logs but
// sends nowhere.
func handlePermissionReply(d *Daemon, _ context.Context, _ *peer, params json.RawMessage) (any, *rpcError) {
	var req permissionReplyParams
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, invalidParams(err)
	}
	if req.ID == "" {
		return nil, invalidParamsMsg(methodPermissionReply + ": id is required")
	}
	sessionID, ok := d.lookupPermRoute(req.ID)
	if !ok {
		return nil, invalidParamsMsg(fmt.Sprintf("%s: no outstanding permission request with id %q", methodPermissionReply, req.ID))
	}
	if err := d.sup.Reply(sessionID, event.PermissionReply{ID: req.ID, Verdict: req.Verdict, Remember: req.Remember}); err != nil {
		return nil, appError(err)
	}
	d.log.Debug("permission reply routed", "session", sessionID, "verdict", string(req.Verdict))
	return struct{}{}, nil
}

// handleGoferRoster answers gofer/roster: the live roster, newest-first.
func handleGoferRoster(d *Daemon, ctx context.Context, _ *peer, _ json.RawMessage) (any, *rpcError) {
	infos, err := d.sup.Roster(ctx)
	if err != nil {
		return nil, appError(err)
	}
	return toSessionInfoDTOs(infos), nil
}

// handleGoferPS answers gofer/ps: every session on disk, live or archived
// (Live distinguishes them).
func handleGoferPS(d *Daemon, ctx context.Context, _ *peer, _ json.RawMessage) (any, *rpcError) {
	infos, err := d.sup.List(ctx)
	if err != nil {
		return nil, appError(err)
	}
	return toSessionInfoDTOs(infos), nil
}

// handleGoferKill answers gofer/kill {sessionId}.
func handleGoferKill(d *Daemon, ctx context.Context, _ *peer, params json.RawMessage) (any, *rpcError) {
	req, rerr := decodeSessionIDParams(methodGoferKill, params)
	if rerr != nil {
		return nil, rerr
	}
	if err := d.sup.Kill(ctx, req.SessionID); err != nil {
		return nil, appError(err)
	}
	d.log.Info("session killed", "session", req.SessionID)
	return struct{}{}, nil
}

// handleGoferArchive answers gofer/archive {sessionId}, surfacing
// [supervisor.ErrRunning] (still active — kill or interrupt it first) and
// [supervisor.ErrNotLive] (unknown or already archived) as clear application
// errors.
func handleGoferArchive(d *Daemon, ctx context.Context, _ *peer, params json.RawMessage) (any, *rpcError) {
	req, rerr := decodeSessionIDParams(methodGoferArchive, params)
	if rerr != nil {
		return nil, rerr
	}
	if err := d.sup.Archive(ctx, req.SessionID); err != nil {
		return nil, appError(err)
	}
	d.log.Info("session archived", "session", req.SessionID)
	return struct{}{}, nil
}
