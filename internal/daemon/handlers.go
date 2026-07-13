package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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

	methodGoferRoster:  handleGoferRoster,
	methodGoferPS:      handleGoferPS,
	methodGoferKill:    handleGoferKill,
	methodGoferArchive: handleGoferArchive,
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
// disconnect.
func handleInitialize(_ *Daemon, _ context.Context, _ *peer, params json.RawMessage) (any, *rpcError) {
	if _, err := acp.DecodeInitialize(params); err != nil {
		return nil, invalidParams(err)
	}
	return acp.NewInitializeResponse(), nil
}

// handleAuthenticate is a no-op success: the WebSocket bearer token (see
// [Daemon.authorized]) is the daemon's only auth boundary in M2, checked
// before the connection is even upgraded.
func handleAuthenticate(_ *Daemon, _ context.Context, _ *peer, _ json.RawMessage) (any, *rpcError) {
	return struct{}{}, nil
}

// handleSessionNew creates an idle session (no first turn — the prompt
// arrives via a subsequent session/prompt) and replies its id.
func handleSessionNew(d *Daemon, ctx context.Context, _ *peer, params json.RawMessage) (any, *rpcError) {
	op, rerr := decodeOp[event.SessionNew](acp.MethodSessionNew, params)
	if rerr != nil {
		return nil, rerr
	}
	info, err := d.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: op.Cwd, Model: d.cfg.DefaultModel})
	if err != nil {
		return nil, appError(err)
	}
	d.log.Info("session created", "session", info.ID)
	return acp.NewSessionResponse{SessionID: info.ID}, nil
}

// handleSessionLoad reopens a persisted session (best-effort for M2: no
// resumption of ACP-side client state beyond the supervisor's own session).
func handleSessionLoad(d *Daemon, ctx context.Context, _ *peer, params json.RawMessage) (any, *rpcError) {
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
	if _, err := d.sup.Resume(ctx, op.SessionID, supervisor.ResumeOptions{Cwd: req.Cwd, Model: d.cfg.DefaultModel}); err != nil {
		return nil, appError(err)
	}
	d.log.Info("session resumed", "session", op.SessionID)
	return loadSessionResult{}, nil
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

// handleSessionPrompt is the streaming heart of the daemon. It subscribes to
// the session's event stream BEFORE dispatching the prompt (so no event
// between dispatch and subscribe is missed), sends the prompt, and then
// drains the subscription: every event that projects to a session/update (see
// [acp.ToSessionUpdate]) is pushed to the client as a notification, and the
// handler returns as soon as it observes a terminal turn.finished (one whose
// stop reason [acp.ToPromptResponse] projects — end_turn, max_tokens,
// refusal, or cancelled).
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

	sub, err := d.sup.Subscribe(ctx, op.SessionID)
	if err != nil {
		return nil, appError(fmt.Errorf("session/prompt %s: %w", op.SessionID, err))
	}
	defer sub.Close()

	if err := d.sup.Send(ctx, op.SessionID, op.Text); err != nil {
		return nil, appError(err)
	}

	var lastFatal string
	for {
		select {
		case e, ok := <-sub.C:
			if !ok {
				return nil, appError(fmt.Errorf("session/prompt %s: session ended before the turn finished", op.SessionID))
			}

			if se, isErr := e.(event.SessionError); isErr && se.Fatal {
				lastFatal = se.Err
			}

			if notif, ok := acp.ToSessionUpdate(op.SessionID, e); ok {
				if werr := p.notify(ctx, acp.MethodSessionUpdate, notif); werr != nil {
					return nil, internalErr(fmt.Errorf("session/prompt %s: write session/update: %w", op.SessionID, werr))
				}
			}

			tf, isTurnFinished := e.(event.TurnFinished)
			if !isTurnFinished {
				continue
			}
			if resp, ok := acp.ToPromptResponse(tf); ok {
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
