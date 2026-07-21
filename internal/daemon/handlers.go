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
// mirror the lifecycle operations, and set_model / set_effort mirror
// [supervisor.Supervisor.SetModel] / [supervisor.Supervisor.SetEffort].
const (
	methodGoferRoster   = "gofer/roster"
	methodGoferPS       = "gofer/ps"
	methodGoferKill     = "gofer/kill"
	methodGoferArchive  = "gofer/archive"
	methodGoferSetModel = "gofer/set_model"

	// methodGoferSetEffort is the effort-axis twin of gofer/set_model: it
	// changes a session's reasoning effort for its next turn. Deliberately
	// gofer-native (not the SDK's session.set_effort op) so it mirrors
	// set_model's shape hop for hop — model- and effort-setting are the same
	// kind of control-plane mutation and travel the same road.
	methodGoferSetEffort = "gofer/set_effort"

	// methodGoferFleet is gofer-native fleet-wide usage: the summed Cost/Usage
	// across every LIVE session, aggregated by the hosted supervisor (see
	// [FleetUsager]). With sessions in separate worker processes under M6, no
	// single row carries the fleet total, so a client that wants "total cost
	// across all sessions" — `gofer ps`' fleet footer — reads it here rather than
	// re-summing the roster. Read-only; takes no params. A daemon whose supervisor
	// does not aggregate (the in-process one) answers {supported:false}, and an
	// older daemon returns method-not-found; a client treats both as "no fleet
	// total to show".
	methodGoferFleet = "gofer/fleet"

	// methodGoferHello is the gofer-native version handshake (design §6): the
	// authoritative, connection-scoped version exchange a router calls first on
	// every worker connection to route around binary/wire skew. Returns
	// HelloResult. Read-only; takes no params.
	methodGoferHello = "gofer/hello"

	// methodGoferModels is gofer-native model discovery over the SDK provider
	// registry: the full registered-model catalog, each entry stamped with the
	// host's per-provider availability (see handleGoferModels). Deliberately
	// gofer-namespaced, NOT the unstable ACP providers/* surface — a remote
	// client (e.g. an iOS ACP client populating a model picker) reads it for the
	// real context windows and pricing behind each id. Read-only.
	//
	// It is a metadata CATALOG, not the admission list, and deliberately
	// under-reports what the daemon will run: this list is built with
	// provider.Lookup (registry membership), while every admission gate —
	// session/new, gofer/set_model, `gofer daemon install -m` — uses
	// provider.Resolve, which infers the provider from the id's shape. So a
	// model newer than this binary's registry is absent here yet perfectly
	// runnable, and a client may validly pass an id this method never listed
	// (PRD "Model admission vs metadata"). Only an id matching no provider
	// family at all is rejected.
	methodGoferModels = "gofer/models"

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

	// methodGoferDecisionRequested / methodGoferDecisionResolved are the
	// structured-decision twins of the permission pair: the gofer-native
	// notifications the daemon fans a session's OPEN and RESOLVED ask_user
	// requests out to every attached peer with. They exist for the same reason —
	// ACP models a decision as a client-answered REQUEST
	// (acp.MethodSessionRequestDecision, which the daemon also sends, see
	// [Daemon.requestDecisionFromPeers]), and a request does not fit a
	// must-deliver fan-out to N peers where the first answer wins.
	//
	// Their source is different, though, and that is the whole shape of this
	// relay: a permission is an event.Event the prompt handler observes on the
	// session's stream, while a decision rides no stream at all (the SDK's Event
	// union is closed and carries no decision kind — see internal/decision), so
	// the ONLY observer is the supervisor's standing per-session gate watcher.
	// See decision_relay.go.
	methodGoferDecisionRequested = "gofer/decision_requested"
	methodGoferDecisionResolved  = "gofer/decision_resolved"

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

	// methodDecisionAnswer is the inbound op a client sends to answer a
	// structured-decision request: JSON-RPC method "decision.answer", params
	// {sessionId, id, answers}. Named for the permission.reply convention (a
	// bare, dotted, gofer-native op rather than a gofer/ notification), and a
	// notification too — no result.
	//
	// Unlike permission.reply it carries a sessionId, because a decision request
	// id is unique only within its session (see [decisionKey]).
	methodDecisionAnswer = "decision.answer"
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

	acp.MethodSessionSetConfigOption: handleSessionSetConfigOption,

	methodGoferRoster:   handleGoferRoster,
	methodGoferPS:       handleGoferPS,
	methodGoferFleet:    handleGoferFleet,
	methodGoferKill:     handleGoferKill,
	methodGoferArchive:  handleGoferArchive,
	methodGoferSetModel: handleGoferSetModel,
	methodGoferModels:   handleGoferModels,
	methodGoferHello:    handleGoferHello,

	methodGoferSetEffort: handleGoferSetEffort,

	methodPermissionReply: handlePermissionReply,
	methodDecisionAnswer:  handleDecisionAnswer,
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
//
// Model resolution: event.SessionNew carries no model field — per
// [acp.FromNewSession]'s doc, the ACP projection deliberately drops
// NewSessionRequest.Model, leaving a consuming application to read it off the
// decoded request directly. So params is decoded a second time here, the same
// way handleSessionLoad recovers Cwd from acp.LoadSessionRequest. An empty
// (or absent) model falls back to the daemon's configured default.
func handleSessionNew(d *Daemon, ctx context.Context, _ *peer, params json.RawMessage) (any, *rpcError) {
	op, rerr := decodeOp[event.SessionNew](acp.MethodSessionNew, params)
	if rerr != nil {
		return nil, rerr
	}
	cwd, rerr := resolveSessionCwd(op.Cwd)
	if rerr != nil {
		return nil, rerr
	}
	var req acp.NewSessionRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, invalidParams(fmt.Errorf("acp: decode %s params: %w", acp.MethodSessionNew, err))
	}
	// d.defaultModel, not d.cfg.DefaultModel: with a resolver configured this
	// re-reads the daemon's default per request, so a `session.model` config
	// write lands on the NEXT session rather than requiring a daemon restart
	// (issue #156). A client-supplied model still wins outright.
	model := d.defaultModel(ctx)
	if req.Model != "" {
		model = req.Model
	}
	// Enforce the optional live-session cap (0 = unlimited, the default — so
	// this whole block is skipped for an ordinary `gofer daemon` and every
	// existing test). A worker (M6) runs MaxSessions=1 to be a single-session
	// daemon. The count is the live roster length.
	//
	// Benign TOCTOU: two concurrent session/new calls could each read a count
	// below the cap and both Create, briefly exceeding it. Acceptable for the
	// single-worker use — a worker is driven by one router serially and never
	// races its own session/new — so no lock is taken across the check+Create.
	if d.cfg.MaxSessions > 0 {
		live, err := d.sup.Roster(ctx)
		if err != nil {
			return nil, appError(err)
		}
		if len(live) >= d.cfg.MaxSessions {
			return nil, appError(fmt.Errorf("session limit reached (max %d)", d.cfg.MaxSessions))
		}
	}
	info, err := d.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: cwd, Model: model})
	if err != nil {
		return nil, appError(err)
	}
	d.log.Info("session created", "session", info.ID)
	return newSessionResult{
		NewSessionResponse: acp.NewSessionResponse{SessionID: info.ID},
		Meta:               &newSessionMeta{Model: info.Model},
	}, nil
}

// newSessionResult is the session/new response: ACP's own
// [acp.NewSessionResponse] plus gofer's `_meta` extension. ACP reserves `_meta`
// for exactly this — implementation-specific data an unaware client ignores —
// so carrying the assigned model there keeps the response conformant, needs no
// change to the SDK's shared wire types, and honors the repo's contract-only
// consumption invariant.
//
// It exists because the ACP response carries only the session id. A client that
// let the daemon choose the model — the NORMAL path, session/new with no model
// — therefore had no way to learn what it actually got, so
// internal/daemonbridge's Create could only echo back the model it had
// REQUESTED (the empty string), and the roster row it returned could never
// carry the real one (issue #162, defect 2).
type newSessionResult struct {
	acp.NewSessionResponse
	Meta *newSessionMeta `json:"_meta,omitempty"`
}

// newSessionMeta is [newSessionResult]'s `_meta` payload. The key is namespaced
// the same way gofer's own methods are (gofer/*), so it cannot collide with
// another ACP implementation's extension data.
type newSessionMeta struct {
	// Model is the model the daemon ASSIGNED to the new session: the client's
	// requested model when it sent one, else the daemon's own resolved default
	// (see the resolution above). Empty only when the daemon could not resolve
	// one at all. A client that sees no `_meta` at all is talking to a daemon
	// predating this field and falls back to whatever it requested.
	Model string `json:"gofer/model,omitempty"`
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
// History completeness: before folding, it waits — bounded and best-effort — for
// the session to finish journaling its in-flight turn (see [Supervisor.AwaitSettled]
// and [Config.LoadSettleTimeout]). A turn's assistant/tool entries are journaled
// ASYNCHRONOUSLY after the turn.finished a client observes, so a load landing in
// that window would otherwise read and silently replay a SHORT history (issue
// #137). The wait cannot deadlock a load of a session genuinely mid-turn: on
// timeout it folds whatever is durable, and the still-open permission that mid-turn
// state carries is replayed separately below (§7).
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
	if _, err := d.sup.Resume(ctx, op.SessionID, supervisor.ResumeOptions{Cwd: cwd, Model: d.defaultModel(ctx)}); err != nil {
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

	// Wait — bounded, best-effort — for the session's in-flight turn to finish
	// journaling before folding, so a load landing in the async-journaling window
	// does not read and silently replay a SHORT history (issue #137). The bound
	// is [Config.LoadSettleTimeout]; on timeout (or any early return) the load
	// proceeds to fold whatever is durable, so a session genuinely mid-turn — one
	// that never reaches needs-input, e.g. an adopted worker blocked on a
	// permission (design §7), whose still-open gate this handler replays AFTER
	// History below — is never deadlocked here. See [Supervisor.AwaitSettled].
	settleCtx, cancelSettle := context.WithTimeout(ctx, d.cfg.LoadSettleTimeout)
	if serr := d.sup.AwaitSettled(settleCtx, op.SessionID); serr != nil {
		d.log.Debug("session load settle wait ended early", "session", op.SessionID, "err", serr)
	}
	cancelSettle()

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

	// Re-surface any STILL-OPEN permission request for the M6 router adopting a
	// worker mid-approval (design §7): the folded history above is journaled
	// conversation only — an outstanding gate is live in-flight state, not in the
	// journal — so a turn blocked awaiting a decision reaches this newly attached
	// peer only if the worker re-emits its pending requests here. Gated to worker
	// mode so the default in-process daemon's session/load is unchanged. Sent as
	// gofer/permission_requested (the same notification handleSessionPrompt fans a
	// live ask out on), before the response, so it rides the same
	// notifications-before-response ordering the replays above rely on and a
	// gofer client reconstructs it exactly as a live one.
	if d.cfg.ReplayPendingPermissionsOnAttach {
		pending := d.pendingPermsForSession(op.SessionID)
		d.log.Debug("session load pending-permission replay", "session", op.SessionID, "requests", len(pending))
		for _, req := range pending {
			if werr := p.notify(ctx, methodGoferPermissionRequested, req); werr != nil {
				return nil, internalErr(fmt.Errorf("session/load %s: replay pending permission: %w", op.SessionID, werr))
			}
		}
	}

	// Re-surface any STILL-OPEN structured-decision request, UNCONDITIONALLY —
	// not behind [Config.ReplayPendingPermissionsOnAttach], and this is the one
	// deliberate asymmetry between the two relays.
	//
	// A permission has two other ways to reach a late peer: it is an event, so it
	// sits in the session broker's retained must-deliver backlog and rides the
	// gofer/event replay above, and it leaves a transcript badge a client renders
	// from history. A decision has NEITHER — it is not an event, and nothing about
	// it is journaled — so this notification is the only path by which a peer that
	// attaches after the question was asked can ever learn a turn is blocked on
	// one. Gating it would mean an ordinary `gofer daemon` shows a TUI attaching
	// mid-question an idle-looking session whose agent waits forever. That is not
	// a knob anyone would want to turn off, so it is not one.
	//
	// Sent as gofer/decision_requested — the same notification a live ask fans out
	// on — before the response, so it rides the same notifications-before-response
	// ordering the replays above rely on and a client reconstructs it exactly as a
	// live one. A peer that already saw this request live and re-loads receives it
	// twice; the client-side stream folds a duplicate id in place (see
	// decision.Stream.Apply), so it re-renders the prompt it is already showing
	// rather than stacking a second.
	openDecisions := d.pendingDecisionsForSession(op.SessionID)
	if len(openDecisions) > 0 {
		d.log.Debug("session load open-decision replay", "session", op.SessionID, "requests", len(openDecisions))
	}
	for _, req := range openDecisions {
		if werr := p.notify(ctx, methodGoferDecisionRequested, req); werr != nil {
			return nil, internalErr(fmt.Errorf("session/load %s: replay open decision: %w", op.SessionID, werr))
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

// handleSessionSetConfigOption answers the ACP session/set_config_option
// request — the stable spec surface for a client's model/mode selectors. gofer
// exposes exactly one config option today, "model", and maps it to its
// gofer-native model swap ([supervisor.Supervisor.SetModel], the same path
// gofer/set_model drives): model-setting stays gofer-native per the PRD, and
// this is only the ACP entry point to it. The reply lists every config option
// gofer supports, each with its post-change current value (see
// modelConfigOption), read back from the live roster so it reflects real state.
// A wrong value type, an empty value, or an unknown configId is a clear
// invalid-params error; an unknown model / unknown session / cross-provider
// swap surface as [supervisor.Supervisor.SetModel]'s application errors.
func handleSessionSetConfigOption(d *Daemon, ctx context.Context, _ *peer, params json.RawMessage) (any, *rpcError) {
	req, err := acp.DecodeSetConfigOption(params)
	if err != nil {
		return nil, invalidParams(err)
	}
	var prevModel string
	switch req.ConfigID {
	case configIDModel:
		sel, ok := req.Value.(acp.SelectValue)
		if !ok {
			return nil, invalidParamsMsg(fmt.Sprintf("%s: config option %q takes a select value id, got %T", acp.MethodSessionSetConfigOption, req.ConfigID, req.Value))
		}
		if sel.Value == "" {
			return nil, invalidParamsMsg(fmt.Sprintf("%s: config option %q value is required", acp.MethodSessionSetConfigOption, req.ConfigID))
		}
		// Snapshot the pre-change model (best-effort — an unknown session yields
		// "" and SetModel below surfaces the real error) so advertiseModelChange
		// can suppress an advertisement when the set is a no-op.
		prevModel, _ = d.sessionModel(ctx, req.SessionID)
		if err := d.sup.SetModel(ctx, req.SessionID, sel.Value); err != nil {
			return nil, appError(err)
		}
		d.log.Info("session config option set", "session", req.SessionID, "config", req.ConfigID, "model", sel.Value)
	default:
		return nil, invalidParamsMsg(fmt.Sprintf("%s: unknown configId %q (gofer supports %q)", acp.MethodSessionSetConfigOption, req.ConfigID, configIDModel))
	}

	current, rerr := d.sessionModel(ctx, req.SessionID)
	if rerr != nil {
		return nil, rerr
	}
	// Advertise the change to attached peers (config_option_update) when the
	// model actually changed — the same snapshot this response returns to the
	// caller, so a second attached client (a phone changing what a laptop sees)
	// tracks the new model live.
	d.advertiseModelChange(req.SessionID, prevModel, current)
	return acp.SetConfigOptionResponse{
		ConfigOptions: []acp.ConfigOption{modelConfigOption(current, d.authedProviders())},
	}, nil
}

// sessionModel returns sessionID's current model, read from the live roster —
// the authoritative post-change value a session/set_config_option response
// reports. A session absent from the roster (never created, killed, or
// archived) is an application error rather than a silent empty value.
func (d *Daemon) sessionModel(ctx context.Context, sessionID string) (string, *rpcError) {
	infos, err := d.sup.Roster(ctx)
	if err != nil {
		return "", appError(err)
	}
	for _, info := range infos {
		if info.ID == sessionID {
			return info.Model, nil
		}
	}
	return "", appError(fmt.Errorf("session %s: not found in roster", sessionID))
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

	// Mark this session as prompt-driven for as long as this handler lives, so
	// the M6 event relay (see event_relay.go) stands down and never
	// double-delivers the events this handler is about to fan out itself. Taken
	// at ENTRY — before the subscription below and before Send — and released by
	// a defer that necessarily runs AFTER this handler's final broadcast, since
	// every broadcast is inline in the loop below. See
	// [Daemon.BroadcastRawEvent]'s guard doc for the one-event turn-boundary
	// window this ordering accepts (and why the other ordering is worse).
	d.beginPromptHandler(op.SessionID)
	defer d.endPromptHandler(op.SessionID)

	sub, err := d.sup.SubscribeLive(ctx, op.SessionID)
	if err != nil {
		return nil, appError(fmt.Errorf("session/prompt %s: %w", op.SessionID, err))
	}
	defer sub.Close()

	// handlerCtx scopes the spec-ACP session/request_permission fan-out (see
	// requestPermissionFromPeers) to this turn: its cancellation — this handler
	// returning, the origin disconnecting, or the daemon shutting down — cancels
	// every permission request this turn still has outstanding at other peers, so
	// none dangles past the turn. Per-request cancellation on the ordinary
	// resolve path is layered on top, in the PermissionResolved branch below.
	handlerCtx, handlerCancel := context.WithCancel(ctx)
	defer handlerCancel()

	// Attach the originator so it is part of this session's fan-out set for
	// subsequent turns other peers drive too, even if it never called
	// session/load (a one-shot `run` that only ever prompts). Deregistered on
	// connection close.
	d.attachPeer(op.SessionID, p)

	if err := d.sup.Send(ctx, op.SessionID, op.Text); err != nil {
		return nil, appError(err)
	}

	// pendingPermIDs collects every permission call id this turn fanned a
	// session/request_permission out for, so the deferred sweep forgets any that
	// never saw a matching permission.resolved (the rare interrupt path that
	// emits no resolved event) — their ctxs are already cancelled by
	// handlerCancel; this just keeps permReqCancels from lingering. Ids that DID
	// resolve were already forgotten in the PermissionResolved branch, so
	// re-cancelling them here is a no-op.
	var pendingPermIDs []string
	defer func() {
		for _, id := range pendingPermIDs {
			d.cancelPermRequest(id)
		}
	}()

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
				reqParams := permissionRequestedParams{
					SessionID: op.SessionID,
					ID:        pe.ID,
					Tool:      pe.Tool,
					Spec:      pe.Spec,
					Trace:     pe.Trace,
				}
				// Record the route + retain the full request, and fan the
				// gofer-native notification out, exactly once — recordPermRoute
				// reports whether this observer is the FIRST to see the request, so
				// an adopted session's standing permission watcher (see
				// [Daemon.RequestPermission]) and this handler never double-fan when
				// both observe the same event. For every non-adopted turn this
				// handler is the sole observer, so first is always true and the
				// behavior is unchanged. Retaining the request lets it re-broadcast
				// to a peer that attaches while this gate is still held — the M6
				// adoption re-surface (see [Daemon.pendingPermsForSession] and
				// handleSessionLoad's replay).
				if d.recordPermRoute(pe.ID, op.SessionID) {
					d.recordPendingPerm(pe.ID, reqParams)
					d.broadcastPermission(op.SessionID, methodGoferPermissionRequested, reqParams)
				}
				// ALSO ask every attached ACP peer via the spec-ACP
				// session/request_permission REQUEST, so a pure ACP client (a
				// phone) can answer — the gofer-native notification above only
				// serves gofer clients (the TUI/daemonbridge). First answer from
				// EITHER surface wins at the session's gate. Ungated: only this
				// handler ever issues ACP requests (the standing watcher does not),
				// so there is nothing to double.
				pendingPermIDs = append(pendingPermIDs, pe.ID)
				d.requestPermissionFromPeers(handlerCtx, op.SessionID, pe)
				continue
			case event.PermissionResolved:
				d.clearPermRoute(pe.ID)
				// Cancel the outstanding session/request_permission requests at
				// every other peer now that the gate is resolved (by whichever
				// surface answered first), so no daemon-side waiter dangles —
				// mirroring this gofer/permission_resolved fanout's timing.
				d.cancelPermRequest(pe.ID)
				// Fan the resolution out exactly once — clearPendingPerm reports
				// whether the retained request was still present (the route is
				// cleared eagerly in handlePermissionReply, so it is not a reliable
				// dedup signal; the pending entry is dropped only here). For a
				// single-observer turn this is always true, so behavior is unchanged.
				if d.clearPendingPerm(pe.ID) {
					d.broadcastPermission(op.SessionID, methodGoferPermissionResolved, permissionResolvedParams{
						SessionID: op.SessionID,
						ID:        pe.ID,
						Verdict:   string(pe.Verdict),
						Rule:      pe.Rule,
					})
				}
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
			d.broadcastGoferEvent(op.SessionID, e)

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
//
// The two rules also differ in the CONTEXT they write under, and that split is
// the point of ctx being origin-only:
//   - origin's write uses ctx, this turn's request context. Its cancellation
//     (origin disconnecting, the turn ending) closing origin's own connection is
//     harmless — that connection is already going away, and the fatal-write rule
//     above aborts the turn on it regardless.
//   - every other peer's write uses a fresh bound off the daemon's base context
//     (see [relayWriteTimeout]). coder/websocket closes the whole connection when
//     a write's context is cancelled, so writing to peer B under peer A's request
//     context would let A disconnecting mid-turn tear down B's healthy
//     connection — for an M6 router that peer is frequently the link to a live
//     worker, marking a running session offline.
//
// # Teardown-latency delta of owning the fan-out context
//
// Borrowing ctx used to make the non-origin loop collapse instantly once the
// origin cancelled: every write inherited an already-cancelled context and
// returned at once. Owning fanCtx removes that shortcut, so a fan-out racing an
// origin teardown now runs to completion instead of short-circuiting. The delta
// is BOUNDED and small: fanCtx is one shared relayWriteTimeout budget for the
// whole fan-out (not per peer — see [relayWriteTimeout]), so the worst case is
// 5s of one demuxer goroutine, and only when a peer is stalled-but-open.
// Healthy peers drain in microseconds, which is the ordinary case. That trade is
// the point: the old shortcut was the same mechanism that let one peer's
// cancellation close every other peer's connection.
func (d *Daemon) broadcastUpdate(ctx context.Context, sessionID string, origin *peer, notif any, isUserEcho bool) *rpcError {
	fanCtx, cancel := context.WithTimeout(d.ctx, relayWriteTimeout)
	defer cancel()
	for _, pr := range d.peersForSession(sessionID) {
		if pr == origin {
			continue // origin handled below with its distinct echo/error rules
		}
		if werr := pr.notify(fanCtx, acp.MethodSessionUpdate, notif); werr != nil {
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
//
// It OWNS the context every write runs under (see [relayWriteTimeout]) rather
// than taking one from its caller: no peer here has origin semantics, and a
// caller's context is a peer's request context — writing under it would let that
// peer's disconnect close a DIFFERENT peer's connection.
func (d *Daemon) broadcastPermission(sessionID, method string, params any) {
	ctx, cancel := context.WithTimeout(d.ctx, relayWriteTimeout)
	defer cancel()
	for _, pr := range d.peersForSession(sessionID) {
		if werr := pr.notify(ctx, method, params); werr != nil {
			// Session id + method only — never the params (tool input/spec);
			// see handleFrame's redaction rule.
			d.log.Debug("permission broadcast: peer notify failed", "session", sessionID, "method", method, "err", werr)
		}
	}
}

// broadcastDecision fans a structured-decision notification out to EVERY peer
// attached to sessionID. It is [Daemon.broadcastPermission]'s exact twin — same
// context ownership (see [relayWriteTimeout]), same snapshot-then-notify, same
// rule that a write failure to any single peer is non-fatal and only logged,
// since a turn blocked on a question must not be affected by one observer's
// wedged socket.
//
// It is a separate function rather than a shared generic so each keeps the
// redaction note for what it actually sends: a permission's params carry tool
// input, a decision's carry the model's own question and option text. Both are
// content — never logged (see handleFrame's redaction rule).
func (d *Daemon) broadcastDecision(sessionID, method string, params any) {
	ctx, cancel := context.WithTimeout(d.ctx, relayWriteTimeout)
	defer cancel()
	for _, pr := range d.peersForSession(sessionID) {
		if werr := pr.notify(ctx, method, params); werr != nil {
			// Session id + method only — never the params (the question text).
			d.log.Debug("decision broadcast: peer notify failed", "session", sessionID, "method", method, "err", werr)
		}
	}
}

// requestDecisionFromPeers fans the spec-ACP session/request_decision REQUEST
// out to every attached ACP peer, alongside the gofer-native
// gofer/decision_requested notification the caller already broadcast. Each
// request runs on its own goroutine — a request BLOCKS awaiting the client's
// answer, and the caller here is the supervisor's decision watcher, which must
// keep draining its gate subscription (chiefly for the eventual resolution that
// cancels the losers). reqCtx is keyed in decisionReqCancels so
// [Daemon.cancelDecisionRequest] retracts them all at once when the request
// resolves by any path.
//
// It derives from the daemon's base context, not a request context: there is no
// per-turn handler owning this fan-out's lifetime (unlike
// [Daemon.requestPermissionFromPeers], scoped to the driving session/prompt).
// Nothing dangles as a result — the gate publishes a resolution for EVERY exit
// path (answered, interrupted, session closed), and each one lands here as
// [Daemon.ResolveDecision], which cancels.
//
// gofer-native peers are skipped for the same reason the permission fan-out
// skips them: they answer via the decision.answer notification and consume
// gofer/decision_requested, so a request would only ever time out at them. A
// peer not yet classified defaults to ACP, the safe direction — see
// [peer.goferNative].
func (d *Daemon) requestDecisionFromPeers(key decisionKey, questions []acp.DecisionQuestion) {
	reqCtx, cancel := context.WithCancel(d.ctx)
	d.registerDecisionCancel(key, cancel)

	req := acp.ToRequestDecision(key.session, questions)
	for _, pr := range d.peersForSession(key.session) {
		if pr.goferNative.Load() {
			continue
		}
		go d.askPeerDecision(reqCtx, pr, key, req)
	}
}

// askPeerDecision sends one session/request_decision request to pr and routes
// the answers it returns into the session's decision gate — the same gate the
// gofer-native decision.answer path resolves (see handleDecisionAnswer), so the
// first answer from EITHER surface wins and the gate makes any later one a
// no-op (it rejects an id that is no longer open).
//
// The response's answers are passed through VERBATIM, including each answer's
// free-text Notes: the gate validates them against the question set and
// normalizes what the client left out, and the tool renders the notes into the
// model-facing result. Dropping or reshaping them here would silently discard
// what the human actually said.
//
// A transport error, a ctx cancellation (the request resolved elsewhere), an
// undecodable response, or a gate rejection are all no-ops — logged at DEBUG,
// session id only, never the payload.
func (d *Daemon) askPeerDecision(ctx context.Context, pr *peer, key decisionKey, req acp.RequestDecisionRequest) {
	raw, err := pr.request(ctx, acp.MethodSessionRequestDecision, req)
	if err != nil {
		// Routine: resolved elsewhere (ctx cancelled), the client disconnected,
		// or it cannot answer this request.
		d.log.Debug("session/request_decision: no answer from peer", "session", key.session, "err", err)
		return
	}
	var resp acp.RequestDecisionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		d.log.Debug("session/request_decision: decode response failed", "session", key.session, "err", err)
		return
	}
	// Skip an answer that lost the race — the request already resolved and its
	// route is gone. The gate is first-wins regardless, so this only avoids a
	// pointless late Answer; it is not required for correctness.
	if !d.decisionRouteOpen(key) {
		return
	}
	if err := d.answerDecision(key, resp.Answers); err != nil {
		d.log.Debug("session/request_decision: routing answer to gate failed", "session", key.session, "err", err)
	}
}

// answerDecision routes answers into key's session gate through the hosted
// supervisor's optional [DecisionAnswerer] capability, and performs the eager
// cleanup a successful answer implies: drop the route (closing the window
// before the gate's resolution reaches the standing watcher) and retract the
// outstanding ACP requests at every other peer. Both are idempotent, so the
// resolution repeating them is a no-op.
//
// A supervisor that does not implement the capability — the M6 router, whose
// sessions live in worker processes — is reported as a plain, actionable error
// rather than a silent success, so a client is never told a turn was unblocked
// that is in fact still waiting. See [DecisionAnswerer].
func (d *Daemon) answerDecision(key decisionKey, answers []acp.DecisionAnswer) error {
	answerer, ok := d.sup.(DecisionAnswerer)
	if !ok {
		return fmt.Errorf("%s: this daemon's supervisor does not carry structured decisions", methodDecisionAnswer)
	}
	if err := answerer.AnswerDecision(key.session, key.request, answers); err != nil {
		return err
	}
	d.clearDecisionRoute(key)
	d.cancelDecisionRequest(key)
	return nil
}

// handleDecisionAnswer answers a client's "decision.answer" op: it routes the
// answers to the session's decision gate, unblocking the ask_user tool call
// waiting on them. It is a notification (no result); every rejection below
// surfaces as an error the router logs but sends nowhere.
//
// Unlike handlePermissionReply it takes the session id from the params rather
// than from a route table, because a decision request id does not identify a
// request on its own (see [decisionKey]). The route table is still consulted —
// as the "is this request actually open?" check — so an unknown, stale, or
// already-answered id becomes a descriptive error instead of a silent drop or a
// confusing gate-level one.
func handleDecisionAnswer(d *Daemon, _ context.Context, _ *peer, params json.RawMessage) (any, *rpcError) {
	var req decisionAnswerParams
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, invalidParams(err)
	}
	if req.SessionID == "" {
		return nil, invalidParamsMsg(methodDecisionAnswer + ": sessionId is required")
	}
	if req.ID == "" {
		return nil, invalidParamsMsg(methodDecisionAnswer + ": id is required")
	}
	key := decisionKey{session: req.SessionID, request: req.ID}
	if !d.decisionRouteOpen(key) {
		return nil, invalidParamsMsg(fmt.Sprintf("%s: session %q has no outstanding decision request with id %q",
			methodDecisionAnswer, req.SessionID, req.ID))
	}
	if err := d.answerDecision(key, req.Answers); err != nil {
		return nil, appError(err)
	}
	d.log.Debug("decision answer routed", "session", req.SessionID, "request", req.ID)
	return struct{}{}, nil
}

// permissionOptions is the fixed option set every session/request_permission
// request offers: the four ACP permission-option kinds. Their ids are the kind
// strings, which is how askPeerPermission maps a client's chosen optionId back
// to its kind (allow/deny + remember) via [acp.ToPermissionReply]. The
// remember-verdict distinction the gofer-native surface expresses is expressible
// here too: allow_always/reject_always carry Remember=true, allow_once/reject_once
// do not — so ACP clients get the same allow/deny(+remember) granularity as the
// TUI, with no wire extension.
func permissionOptions() []acp.PermissionOption {
	return []acp.PermissionOption{
		{OptionID: string(acp.PermissionAllowOnce), Name: "Allow once", Kind: acp.PermissionAllowOnce},
		{OptionID: string(acp.PermissionAllowAlways), Name: "Allow always", Kind: acp.PermissionAllowAlways},
		{OptionID: string(acp.PermissionRejectOnce), Name: "Reject once", Kind: acp.PermissionRejectOnce},
		{OptionID: string(acp.PermissionRejectAlways), Name: "Reject always", Kind: acp.PermissionRejectAlways},
	}
}

// findPermissionOption returns the option in options whose OptionID matches id.
func findPermissionOption(options []acp.PermissionOption, id string) (acp.PermissionOption, bool) {
	for _, o := range options {
		if o.OptionID == id {
			return o, true
		}
	}
	return acp.PermissionOption{}, false
}

// requestPermissionFromPeers fans the spec-ACP session/request_permission
// REQUEST out to every attached ACP peer for the permission pe, alongside the
// gofer-native gofer/permission_requested notification the caller already
// broadcast. Each request runs on its own goroutine (a request BLOCKS awaiting
// the client's answer, and the drain loop must keep processing events — chiefly
// the eventual permission.resolved that cancels the losers), keyed off reqCtx so
// [Daemon.cancelPermRequest] can retract them all at once when the gate resolves
// by any path.
//
// gofer-native peers (the TUI/daemonbridge) are skipped: they do not answer a
// session/request_permission request (they answer via the permission.reply
// notification and consume gofer/permission_requested), so sending them one
// would only ever time out. A peer not yet classified defaults to ACP, the safe
// direction — see [peer.goferNative].
func (d *Daemon) requestPermissionFromPeers(ctx context.Context, sessionID string, pe event.PermissionRequested) {
	reqCtx, cancel := context.WithCancel(ctx)
	d.registerPermCancel(pe.ID, cancel)

	options := permissionOptions()
	req := acp.ToRequestPermission(sessionID, pe.ID, pe.Tool, options)

	for _, pr := range d.peersForSession(sessionID) {
		if pr.goferNative.Load() {
			continue
		}
		go d.askPeerPermission(reqCtx, pr, sessionID, pe.ID, req, options)
	}
}

// askPeerPermission sends one session/request_permission request to pr and, on
// a "selected" answer, routes it into the session's gate — the same call-id
// routing the gofer-native permission.reply path uses (see handlePermissionReply),
// so the first answer from EITHER surface wins and the gate makes any later one a
// no-op. A "cancelled" outcome is a non-answer (the client declined to decide,
// e.g. its own session/cancel raced) and is ignored rather than routed as a deny,
// so one observer dismissing its dialog never denies a turn another peer is still
// deciding. A transport error, a ctx cancellation (the permission resolved
// elsewhere), or an unparseable/unknown answer are all likewise no-ops.
func (d *Daemon) askPeerPermission(ctx context.Context, pr *peer, sessionID, callID string, req acp.RequestPermissionRequest, options []acp.PermissionOption) {
	raw, err := pr.request(ctx, acp.MethodSessionRequestPermission, req)
	if err != nil {
		// Routine: resolved elsewhere (ctx cancelled), the client disconnected,
		// or it cannot answer this request. Session id only — never the payload.
		d.log.Debug("session/request_permission: no answer from peer", "session", sessionID, "err", err)
		return
	}
	var resp acp.RequestPermissionResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		d.log.Debug("session/request_permission: decode response failed", "session", sessionID, "err", err)
		return
	}
	sel, ok := resp.Outcome.(acp.PermissionOutcomeSelected)
	if !ok {
		return // a cancelled outcome is a non-answer — ignore it
	}
	chosen, ok := findPermissionOption(options, sel.OptionID)
	if !ok {
		d.log.Debug("session/request_permission: unknown optionId in answer", "session", sessionID)
		return
	}
	// Skip an answer that lost the race — the gate already resolved and cleared
	// the route. The gate is first-wins regardless, so this is only to avoid a
	// pointless late Reply; it is not required for correctness.
	if _, live := d.lookupPermRoute(callID); !live {
		return
	}
	if err := d.sup.Reply(sessionID, acp.ToPermissionReply(callID, resp, chosen)); err != nil {
		d.log.Debug("session/request_permission: routing answer to gate failed", "session", sessionID, "err", err)
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
//
// Like [Daemon.broadcastPermission] it owns its write context rather than
// borrowing a peer's (see [relayWriteTimeout]).
//
// It deliberately does NOT delegate to [Daemon.BroadcastRawEvent] even though
// the two loops are otherwise identical: that method is the M6 relay, and its
// double-delivery guard suppresses exactly the case this function IS — a live
// prompt handler fanning out its own turn — so delegating would silence every
// turn a client drives.
func (d *Daemon) broadcastGoferEvent(sessionID string, e event.Event) {
	raw, err := json.Marshal(e)
	if err != nil {
		d.log.Debug("gofer/event broadcast: marshal failed", "session", sessionID, "kind", e.Kind(), "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(d.ctx, relayWriteTimeout)
	defer cancel()
	for _, pr := range d.peersForSession(sessionID) {
		if werr := pr.notify(ctx, methodGoferEvent, json.RawMessage(raw)); werr != nil {
			// Session id + kind only — never the marshaled event (may carry
			// prompt/message content); see handleFrame's redaction rule.
			d.log.Debug("gofer/event broadcast: peer notify failed", "session", sessionID, "kind", e.Kind(), "err", werr)
		}
	}
}

// advertiseModelChange emits + fans out a config_option_update snapshot when
// sessionID's model actually changed from prev to current — the live-config
// counterpart to the model value a session/set_config_option response already
// returns to its caller. It is the shared tail of both model-swap routes
// (session/set_config_option and gofer/set_model), so both advertise the change
// identically.
//
// Emit-on-change guard: a no-op set (current == prev) or an unreadable
// post-change model (current == "", e.g. the session was killed in the race
// window) advertises nothing — a client must not be stormed with a
// config_option_update that changed nothing.
//
// Delivery is two-pronged, mirroring handleSessionPrompt's per-event fan-out:
//   - EmitConfigOptions publishes the event.ConfigOptionsUpdated onto the
//     session's own stream via the runner Emit seam (journals/retains it, so a
//     later peek's retained-backlog replay and any concurrent-turn drain observe
//     it too).
//   - broadcastGoferEvent + the acp.ToSessionUpdate projection fan it out LIVE to
//     every currently-attached peer. This direct fan-out is necessary because
//     there is no continuous broker drain outside a session/prompt (see doc.go):
//     a model change between turns reaches clients ONLY via this broadcast. The
//     projection is acp.ToSessionUpdate's pass-through (config_option_update);
//     gofer clients get the verbatim event on gofer/event.
//
// If a set_config_option ever races a live turn on the same session (the M2
// one-prompt-per-session contract makes this rare), the turn's drain and this
// direct fan-out both deliver the snapshot; a config_option_update is an
// authoritative, idempotent full snapshot (not a delta), so a duplicate is a
// harmless re-render.
func (d *Daemon) advertiseModelChange(sessionID, prev, current string) {
	if current == "" || current == prev {
		return
	}
	opts := []event.ConfigOption{modelConfigOptionEvent(current, d.authedProviders())}
	if err := d.sup.EmitConfigOptions(sessionID, opts); err != nil {
		// DEBUG, not WARN: the only expected error is the session going away
		// between the model set and here — routine, not a daemon fault.
		d.log.Debug("config option update: emit failed", "session", sessionID, "err", err)
	}
	ev := event.NewConfigOptionsUpdated(sessionID, opts)
	d.broadcastGoferEvent(sessionID, ev)
	if notif, ok := acp.ToSessionUpdate(sessionID, ev); ok {
		d.broadcastConfigOptionUpdate(sessionID, notif)
	}
}

// broadcastConfigOptionUpdate fans a projected config_option_update session/update
// out to EVERY peer attached to sessionID (including the peer that drove the
// model change — a config snapshot is not an echo it already has on the
// session/update surface; its set_config_option response carries the value, but
// a gofer client tracks its own model off the event stream). Modeled on
// broadcastGoferEvent, not broadcastUpdate: no origin special-casing or
// user-echo suppression, and a write failure to any single peer is non-fatal and
// only logged — a wedged observer must never fail the model-change RPC. The peer
// set is snapshotted under the registry RLock and released before any notify runs
// (see peersForSession).
//
// Like [Daemon.broadcastPermission] it owns its write context rather than
// borrowing the model-change RPC caller's (see [relayWriteTimeout]).
func (d *Daemon) broadcastConfigOptionUpdate(sessionID string, notif any) {
	ctx, cancel := context.WithTimeout(d.ctx, relayWriteTimeout)
	defer cancel()
	for _, pr := range d.peersForSession(sessionID) {
		if werr := pr.notify(ctx, acp.MethodSessionUpdate, notif); werr != nil {
			// Session id only — never the notif (carries no message content here,
			// but the redaction rule is uniform); see handleFrame's redaction rule.
			d.log.Debug("config option update broadcast: peer notify failed", "session", sessionID, "err", werr)
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
	// Clean up eagerly, mirroring the PermissionResolved event-stream path
	// (the resolved case above): drop the call->session route and cancel any
	// outstanding ACP session/request_permission requests at other peers. Both
	// are idempotent, so the PermissionResolved this reply triggers no-ops when
	// it repeats them — this just closes the one-hop window before it arrives.
	d.clearPermRoute(req.ID)
	d.cancelPermRequest(req.ID)
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

// handleGoferFleet answers gofer/fleet: the fleet-wide Cost/Usage total across
// every live session. It type-asserts the hosted supervisor for the optional
// [FleetUsager] capability — the M6 router aggregates off its roster cache; the
// in-process supervisor does not implement it — and reports {supported:false}
// when the supervisor does not aggregate, so a client omits its fleet footer
// rather than showing a misleading $0. Read-only; never fails.
func handleGoferFleet(d *Daemon, _ context.Context, _ *peer, _ json.RawMessage) (any, *rpcError) {
	fu, ok := d.sup.(FleetUsager)
	if !ok {
		return fleetUsageDTO{Supported: false}, nil
	}
	cost, usage := fu.FleetUsage()
	return fleetUsageDTO{Supported: true, Cost: cost, Usage: usage}, nil
}

// handleGoferModels answers gofer/models: the full SDK provider registry
// projected to the wire, sorted (provider, id), each entry stamped Available
// per the daemon host's current auth (see Config.AuthedProviders). Read-only.
func handleGoferModels(d *Daemon, _ context.Context, _ *peer, _ json.RawMessage) (any, *rpcError) {
	return toModelInfoDTOs(d.authedProviders()), nil
}

// handleGoferHello answers gofer/hello: the daemon's identity across the three
// version axes (design §6). binaryVersion is the daemon's build version
// (Config.Version), wireVersion the router↔worker wire contract version
// (WireVersion), acpProtocolVersion the ACP version this daemon speaks
// (acp.ProtocolVersion), plus defaultModel — the model this daemon would use
// RIGHT NOW for a session/new carrying none, which a client cannot reproduce
// locally (see HelloResult.DefaultModel). Takes no params; never fails.
//
// defaultModel comes from the same [Daemon.defaultModel] accessor session/new
// and session/load use, so what a daemon advertises and what it acts on cannot
// drift apart (issue #156).
func handleGoferHello(d *Daemon, ctx context.Context, _ *peer, _ json.RawMessage) (any, *rpcError) {
	return HelloResult{
		BinaryVersion:      d.cfg.Version,
		WireVersion:        WireVersion,
		ACPProtocolVersion: acp.ProtocolVersion,
		DefaultModel:       d.defaultModel(ctx),
	}, nil
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

// handleGoferSetModel answers gofer/set_model {sessionId, model}, changing
// the session's model for its next turn (see [supervisor.Supervisor.SetModel]).
// [supervisor.ErrNotLive] (unknown session), an unknown model id, and
// [supervisor.ErrCrossProvider] (a same-session cross-provider swap) all
// surface as clear application errors — the wrapped error's message names
// both models and providers, so a client sees exactly why it was rejected
// even though the concrete ErrCrossProvider type itself does not cross the
// wire (see internal/daemonbridge's SetModel doc).
func handleGoferSetModel(d *Daemon, ctx context.Context, _ *peer, params json.RawMessage) (any, *rpcError) {
	req, rerr := decodeSetModelParams(params)
	if rerr != nil {
		return nil, rerr
	}
	prevModel, _ := d.sessionModel(ctx, req.SessionID)
	if err := d.sup.SetModel(ctx, req.SessionID, req.Model); err != nil {
		return nil, appError(err)
	}
	d.log.Info("session model set", "session", req.SessionID, "model", req.Model)
	// Advertise the change to attached peers (config_option_update) on an actual
	// change, so the gofer/set_model route surfaces it identically to the ACP
	// session/set_config_option route. Best-effort post-change read: an
	// unreadable model just suppresses the advertisement (see advertiseModelChange).
	current, _ := d.sessionModel(ctx, req.SessionID)
	d.advertiseModelChange(req.SessionID, prevModel, current)
	return struct{}{}, nil
}

// handleGoferSetEffort answers gofer/set_effort {sessionId, effort}, changing
// the session's reasoning effort for its next turn (see
// [supervisor.Supervisor.SetEffort]). [supervisor.ErrNotLive] (unknown session),
// [supervisor.ErrInvalidEffort] (a level outside the unified vocabulary), and
// the SDK's own non-reasoning-model rejection all surface as clear application
// errors naming the offending value — the concrete sentinel types do not cross
// the wire (see internal/daemonbridge's SetEffort doc), the messages do.
//
// Unlike [handleGoferSetModel] it advertises nothing afterwards: effort is not
// one of the ACP config options this daemon publishes (see
// [handleSessionSetConfigOption], which offers "model" only), so there is no
// config_option_update for a client to reconcile against.
func handleGoferSetEffort(d *Daemon, ctx context.Context, _ *peer, params json.RawMessage) (any, *rpcError) {
	req, rerr := decodeSetEffortParams(params)
	if rerr != nil {
		return nil, rerr
	}
	if err := d.sup.SetEffort(ctx, req.SessionID, req.Effort); err != nil {
		return nil, appError(err)
	}
	d.log.Info("session effort set", "session", req.SessionID, "effort", req.Effort)
	return struct{}{}, nil
}
