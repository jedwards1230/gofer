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

// resolveSessionCwd validates and normalizes an ACP session cwd. ACP v1
// requires cwd to be an absolute path (both NewSessionRequest.cwd and
// LoadSessionRequest.cwd — src/v1/agent.rs); as a DX nicety for phone clients
// that let a user type a path, a leading "~" or "~/" is expanded against the
// daemon's own home. An empty cwd defaults to the daemon's own working
// directory (os.Getwd) — the same effective root a zero-value
// [supervisor.CreateOptions]/[supervisor.ResumeOptions] has always resolved
// to (see their doc comments), now explicit and validated here rather than
// left to flow down unchecked. The result must be an existing directory;
// otherwise a clear invalid-params error naming raw is returned instead of
// creating a session whose every tool call silently fails (the live bug this
// guards: an ACP client sending the literal, unexpanded string
// "~/orchestration" as cwd).
func resolveSessionCwd(raw string) (string, *rpcError) {
	if strings.TrimSpace(raw) == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", internalErr(fmt.Errorf("session cwd: resolve daemon working directory: %w", err))
		}
		return cwd, nil
	}

	cwd := raw
	if raw == "~" || strings.HasPrefix(raw, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", internalErr(fmt.Errorf("session cwd: resolve daemon home directory: %w", err))
		}
		if raw == "~" {
			cwd = home
		} else {
			cwd = filepath.Join(home, raw[2:])
		}
	}
	cwd = filepath.Clean(cwd)

	if !filepath.IsAbs(cwd) {
		return "", invalidParamsMsg(fmt.Sprintf("session cwd %q must be an absolute path (a leading ~ is expanded to the daemon's home)", raw))
	}

	fi, err := os.Stat(cwd)
	if err != nil {
		return "", invalidParamsMsg(fmt.Sprintf("session cwd %q does not exist: %v", raw, err))
	}
	if !fi.IsDir() {
		return "", invalidParamsMsg(fmt.Sprintf("session cwd %q is not a directory", cwd))
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
// requirement.
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
// archived — see [supervisor.Supervisor.List]), optionally filtered by cwd,
// newest-first, opaquely paginated at [sessionListPageSize] entries per page.
//
// A disk-only (archived) [supervisor.SessionInfo] carries no cwd — the
// journal does not persist it — so Cwd falls back to the daemon's own
// working directory for those entries; a cwd filter therefore only ever
// matches live sessions in M2, which is an accepted limitation (see the
// task brief).
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

	if req.Cwd != "" {
		filtered := infos[:0:0]
		for _, info := range infos {
			if info.Cwd == req.Cwd {
				filtered = append(filtered, info)
			}
		}
		infos = filtered
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

	// daemonCwd is the fallback for a disk-only entry's empty Cwd (see the
	// doc above); resolved once, best-effort — an error here just leaves the
	// fallback "" rather than failing the whole request.
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

			if notif, ok := acp.ToSessionUpdate(op.SessionID, e); ok {
				if werr := p.notify(ctx, acp.MethodSessionUpdate, notif); werr != nil {
					return nil, internalErr(fmt.Errorf("session/prompt %s: write session/update: %w", op.SessionID, werr))
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
