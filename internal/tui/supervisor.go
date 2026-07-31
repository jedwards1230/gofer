package tui

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/decision"
)

// The TUI is a client of the daemon's supervisor, never a privileged peer: it
// reads the roster, subscribes to the same per-session Event stream every ACP
// client sees, and submits the same Ops (create/send/interrupt/kill/archive).
// This file is the consumer-side contract — the narrow slice of the supervisor
// the TUI depends on.
//
// The supervisor itself is package #2's (gofer-daemon's) to build in
// internal/supervisor. Until that package lands, SessionInfo, SessionStatus,
// and Supervisor live here so the TUI and its golden tests are unblocked; a
// reconciliation PR moves the shared value types (SessionInfo/SessionStatus)
// into the supervisor package and reduces this file to the consumer interface
// alone, once the shapes have converged with gofer-daemon.

// SessionStatus is the coarse roster grouping a session falls into. It drives
// both the grouped-view sections (Working / Needs input / Idle / Finished) and the
// header status counts.
type SessionStatus int

const (
	// StatusWorking is a session with a turn in flight.
	StatusWorking SessionStatus = iota
	// StatusNeedsInput is a session at rest awaiting the user — either it
	// finished its turn and is waiting for the next prompt, or a permission
	// request is pending.
	StatusNeedsInput
	// StatusFinished is a terminal session (completed, killed, or archived);
	// its journal is retained (repo invariant #4) and it remains listable.
	StatusFinished
	// StatusIdle is a session at rest that is NOT awaiting the user: a reloaded
	// offline row, or one just resumed from disk and not yet prompted. It is
	// deliberately distinct from StatusNeedsInput so browsing/opening a reloaded
	// session does not label it "Needs input" or move the header's awaiting-input
	// count (see counts / effectiveStatus). A pending request on such a row still
	// reads as StatusNeedsInput via effectiveStatus, so a real prompt is never
	// hidden. Ordinal-aligned with [supervisor.StatusIdle] because the in-process
	// path casts one enum to the other (see internal/tuibridge).
	StatusIdle
)

// String returns the roster section label for a status.
func (s SessionStatus) String() string {
	switch s {
	case StatusWorking:
		return "Working"
	case StatusNeedsInput:
		return "Needs input"
	case StatusFinished:
		return "Finished"
	case StatusIdle:
		return "Idle"
	default:
		return "Unknown"
	}
}

// SessionInfo is one roster row: everything the overview needs to render a
// session without subscribing to its event stream. The supervisor derives it
// from the session's journal and live turn state.
type SessionInfo struct {
	ID      string        // stable session id (a UUID)
	Title   string        // task title, seeded from the first prompt
	Summary string        // one-line latest-activity summary
	Status  SessionStatus // coarse grouping / status count bucket
	Model   string        // model id driving the session
	Effort  string        // reasoning effort: "" (provider default), "low", "medium", "high"
	Cwd     string        // session working directory — the roster's cwd group key

	Cost  provider.Cost  // accumulated cost, from the SDK's usage accounting
	Usage provider.Usage // accumulated token usage

	Pending   int // pending permission requests; >0 reclassifies the row to "Needs input"
	Artifacts int // artifact/PR count; best-effort, 0 until later milestones

	Created time.Time // session start
	Updated time.Time // last activity — the recency sort key

	// BinaryVersion is the gofer build running this session's process. Under M6
	// process isolation each session runs in its own worker, so a daemon upgrade
	// leaves already-running sessions finishing on the OLD build while new ones
	// start on the new one. The roster renders it only when it DIFFERS from the
	// app's own version (see [Overview.binaryMark]) — that skew is the whole
	// signal, and stamping an identical version on every row would be noise.
	// Empty for an offline row (no process) and from any pre-M6 daemon.
	BinaryVersion string

	// ParentID is the id of the session that spawned this one — "" for a root
	// session. A subagent is a real session with its own journal, cost and
	// transcript, so a child row is an ordinary roster row plus this link; the
	// link is what lets the overview render children indented beneath their
	// parent instead of as unrelated siblings.
	ParentID string
	// Agent is the session's agent identity (e.g. "go-developer"), the same id
	// its tool-call events are stamped with. "" is un-attributed.
	Agent string
	// Depth is the row's depth in the subagent tree: 0 for a root session,
	// parent+1 for a child — the indent level a tree render uses.
	Depth int

	// LastUsage is the MOST RECENTLY COMPLETED turn's token usage in the
	// session's current folded context — the measured proxy for how full the
	// context window is right now, as opposed to Usage above (the session's
	// ACCUMULATED total, which only grows). It is what the /context panel
	// (context.go) reads as its numerator, and what drops back down the turn
	// after a compaction. Zero when no turn has settled yet.
	LastUsage provider.Usage
	// ContextWindow is the active model's total context-window size in
	// tokens, resolved server-side — 0 when the model is unregistered
	// (unknown, never "no window"; a renderer must not divide by it). Pairs
	// with LastUsage as /context's denominator.
	ContextWindow int
}

// SessionRef is one entry in the /resume picker's list: a session that exists
// on disk and can therefore be brought back under live supervision. It is
// deliberately NOT a [SessionInfo] — a session that is merely on disk has no
// status, no cost, and no usage to report, and reusing the roster row would
// present those zero values as fact (the same "omit what you can't answer
// honestly" rule status.go states). These four fields are exactly what BOTH
// backends can answer for an offline session: the in-process supervisor reads
// them back off the journal ([supervisor.Supervisor.List]), and the daemon
// path gets them off the ACP session/list response.
type SessionRef struct {
	ID      string    // stable session id (a UUID) — what Resume addresses
	Title   string    // task title, seeded from the first prompt; may be empty
	Cwd     string    // the directory the session was created in; may be empty
	Updated time.Time // last activity — the newest-first sort key; may be zero
}

// CreateOptions configures [Supervisor.Create]. The zero value is the
// daemon's default: a credential-driven model in the daemon's working
// directory, as a ROOT session. The daemon supervisor's CreateOptions carries
// more fields (System, Params, MaxIters); the TUI only sets these, so this local
// copy mirrors just them until the reconciliation PR imports the daemon type.
type CreateOptions struct {
	Model string
	Cwd   string
	// ParentID, when set, creates the session as a SUBAGENT of that session
	// rather than as a root one (see [SessionInfo.ParentID]). An unknown parent,
	// or one already at the daemon's depth cap, fails the create.
	ParentID string
	// Agent is the new session's agent identity, stamped onto its tool-call
	// events (see [SessionInfo.Agent]).
	Agent string
}

// PermissionDecision is a human's answer to a pending permission request:
// the verdict, whether to remember it, and — for an amend-before-approve —
// the replacement tool input the call runs with instead of the model's
// original arguments. It mirrors the SDK's event.PermissionReply.
//
// It is a struct rather than a third positional argument on [Supervisor.Reply]
// because the three fields are one decision: a six-parameter, two-bool
// signature reads the same at the call site whichever way the bools are
// ordered, and this one is answered by a human under time pressure.
type PermissionDecision struct {
	Allow    bool
	Remember bool
	// Input, when non-nil, is the replacement tool input for an amended
	// allow. It is honored only with Allow; a nil Input is the plain
	// allow/deny path, byte-identical to before amend existed.
	//
	// The SDK does NOT re-run the permission guard over it — see
	// loop.awaitApproval, which substitutes it into the call after the guard
	// already evaluated the model's original arguments, and substitutes it
	// BEFORE calling Grant, so a remembered amend pins the amended call. The
	// approval prompt says both out loud (see approval.go's warning lines);
	// nothing on this path may imply otherwise.
	Input json.RawMessage
}

// Supervisor is the client-side view of the daemon the TUI drives. Every
// method is an Op or a read a remote ACP client could equally issue: the TUI
// holds no back channel the protocol doesn't expose.
type Supervisor interface {
	// Roster returns a snapshot of every live (and, per the daemon's policy,
	// recently finished) session. The supervisor's roster is pull-based, so
	// the app root polls this on a timer and re-renders on each snapshot.
	Roster(ctx context.Context) ([]SessionInfo, error)

	// Subscribe returns the event stream for one session — the same
	// *event.Subscription an attach or peek renders, and the same bytes an
	// ACP client would receive.
	Subscribe(ctx context.Context, sessionID string) (*event.Subscription, error)

	// Create starts a new session seeded with prompt and returns its roster
	// row. The dispatch bar calls this, then attaches into the returned id. A
	// zero-value opts gives the daemon's default behavior (credential-driven
	// model, daemon working directory); an ACP client or a `-m` invocation
	// sets Model/Cwd at create time. An empty prompt creates an idle session
	// with no first turn (the ACP path).
	Create(ctx context.Context, prompt string, opts CreateOptions) (SessionInfo, error)

	// ListSessions enumerates every session on disk — live and offline alike —
	// as the /resume picker's source list. It is a strictly WIDER read than
	// Roster: Roster answers "what is under live supervision right now", while
	// this answers "what could be brought back", which is the only question a
	// resume picker can be built from. Ordering is the backend's; the caller
	// sorts. A backend with no store to walk returns an empty list, not an
	// error.
	ListSessions(ctx context.Context) ([]SessionRef, error)

	// Resume reopens a persisted session as a live one and leaves it addressable
	// by every other method here — the client-side half of ACP's session/load.
	// It is idempotent: resuming a session that is already live is a no-op that
	// succeeds, so the caller may always follow it with a Subscribe.
	//
	// cwd is the working directory to reload the session INTO, and per ACP v1 it
	// is the client's call, not the daemon's (LoadSessionRequest.cwd is
	// required). Callers pass the session's own persisted directory when they
	// know it (the picker reads it off [SessionRef.Cwd]) and their own working
	// directory otherwise — the same value `gofer resume` sends from os.Getwd.
	Resume(ctx context.Context, sessionID, cwd string) error

	// Send submits prompt as the next turn on an existing session — the
	// multi-turn attach loop's send-when-idle path.
	Send(ctx context.Context, sessionID, prompt string) error

	// Interrupt stops the in-flight turn of a session without terminating it
	// (esc on the active session). A subsequent Send resumes the same
	// journaled session.
	Interrupt(ctx context.Context, sessionID string) error

	// Kill interrupts and terminates a running session. The journal is kept.
	Kill(ctx context.Context, sessionID string) error

	// Archive drops a finished session from the roster. The journal is kept.
	Archive(ctx context.Context, sessionID string) error

	// SetModel changes the model a session uses for its next turn. It is
	// valid to call while the session is running — the swap takes effect on
	// the NEXT turn, not the one in flight. It returns an error for an
	// unknown model id or a cross-provider target (a session's provider is
	// fixed at creation; switching providers requires a new session). A
	// caller wanting to branch on the cross-provider case specifically
	// should compare provider families itself before calling — the concrete
	// error type does not cross the daemon wire (see internal/daemonbridge).
	SetModel(ctx context.Context, sessionID, model string) error

	// SetEffort changes the reasoning effort a session uses for its next
	// turn — the effort-axis twin of SetModel, valid to call while the
	// session is running for the same reason (the swap lands on the NEXT
	// turn). An empty effort clears the level back to the provider's default
	// and is always legal; any other value outside "low"/"medium"/"high" is
	// rejected, as is a non-empty level on a model the SDK registry KNOWS
	// cannot reason. There is NO cross-provider constraint here — effort is
	// provider-agnostic vocabulary. A caller wanting to refuse the
	// non-reasoning case BEFORE calling should read [provider.Lookup]'s
	// Reasoning bit itself: like SetModel's, the concrete error type does not
	// cross the daemon wire.
	SetEffort(ctx context.Context, sessionID, effort string) error

	// Reply answers a pending permission request identified by id with d:
	// allow or deny it, and — when d.Remember is true — persist the verdict
	// as a standing grant for future matching calls (the SDK's
	// loop.RuleGuard/Grant path). A non-nil d.Input is an amend: the call
	// runs with that input instead of the model's, and a remembered amend
	// grants the AMENDED call (see [PermissionDecision.Input]). The inline
	// approval prompt's key handling (see app.go/dialog.go) is the sole
	// caller. sessionID scopes the reply to the session the prompt was
	// raised for; a daemon-backed Supervisor need not put it on the wire
	// itself (see internal/daemonbridge's contract: the daemon resolves a
	// permission request by id alone), but an in-process one routes through
	// it directly.
	Reply(ctx context.Context, sessionID, id string, d PermissionDecision) error

	// ExplainPermission asks why the identified still-pending tool call was
	// gated. It is READ-ONLY: it never resolves the request, so the prompt
	// stays open across an explain and the human still answers it.
	//
	// The returned [acp.PermissionRationale] is the AGENT's own answer — the
	// gating decision as the side that made it describes it, as opposed to
	// the approximation this client derives from the trace riding on the
	// permission request (see internal/permrationale, which both sides share
	// so the two are comparable). An unknown or already-resolved call id, or
	// one belonging to another session, is an error rather than an empty
	// rationale: "no longer pending" and "gated for no stated reason" are
	// different answers and a client must be able to tell them apart.
	ExplainPermission(ctx context.Context, sessionID, callID string) (acp.PermissionRationale, error)

	// Decisions subscribes to sessionID's open structured-decision requests —
	// the questions an agent asked with the ask_user tool and is blocked on
	// (see internal/decision). It is a SECOND stream alongside Subscribe, not
	// part of the event stream: the SDK's Event union carries no decision
	// kind, so the request travels gofer's own transport (the package doc in
	// internal/decision has the why). Every request already open is replayed
	// first, so attaching mid-turn still shows the prompt.
	//
	// The caller owns the returned subscription and must Close it — the app
	// tears it down with the session subscription it sits beside.
	Decisions(ctx context.Context, sessionID string) (*decision.Subscription, error)

	// AnswerDecision resolves the outstanding decision request identified by
	// requestID with one answer per question. Unanswered questions may be
	// omitted — the gate fills them in as cancelled — but an answer naming a
	// question or option the request does not carry is rejected, as is a
	// request that is no longer open (another peer answered it, or the turn
	// was interrupted). Unlike a permission call id, requestID is unique only
	// within its session, so sessionID is what disambiguates it.
	AnswerDecision(ctx context.Context, sessionID, requestID string, answers []acp.DecisionAnswer) error

	// RestartDaemon restarts the daemon this client is talking to — the
	// stale-daemon banner's one-key action — bringing it back on the CLIENT's
	// build (the honest self-update the banner offers) and reconnecting to the
	// replacement. On success the overview's next roster poll lands on the new
	// daemon, which rebuilds the roster from the on-disk journals so the sessions
	// that were showing return. A backend with no daemon to restart (the local
	// in-process supervisor — which never shows the banner) returns an error; the
	// key is only reachable while the banner is up, so that path is unreached.
	RestartDaemon(ctx context.Context) error

	// DaemonVersion reports the build version of the daemon this Supervisor is
	// currently connected to — the value a fresh gofer/hello handshake would
	// answer, best-effort and non-blocking (it never dials on its own). "" means
	// unknown, including on a backend with no separate daemon to ask (the local
	// in-process supervisor, which never shows the banner this exists for).
	//
	// It exists so [App]'s daemonRestartMsg handler can refresh
	// OverviewMeta.DaemonVersion after RestartDaemon swaps in a fresh
	// connection: that seed value is otherwise a one-shot snapshot taken before
	// [NewApp] and never updated, so the stale-daemon banner (skewSeparator)
	// would keep warning about a daemon the restart already replaced.
	DaemonVersion() string

	// Compact replaces sessionID's history up to HEAD with a summary — the
	// backend for the explicit `/compact` command (command.go). instructions
	// is forwarded verbatim; "" uses the SDK's own default. It is idle-only
	// (see [supervisor.Supervisor.Compact]'s doc): a running session, or one
	// with queued work, is refused. Success is observable on the session's own
	// event stream as a session.compacted event — this method does not itself
	// return a summary, matching Reply/AnswerDecision's fire-and-observe shape.
	Compact(ctx context.Context, sessionID, instructions string) error
}
