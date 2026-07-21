package tui

import (
	"context"
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
// both the grouped-view sections (Working / Needs input / Finished) and the
// header status counts.
type SessionStatus int

const (
	// StatusWorking is a session with a turn in flight.
	StatusWorking SessionStatus = iota
	// StatusNeedsInput is an idle session awaiting the user — either it
	// finished its turn and is waiting for the next prompt, or a permission
	// request is pending.
	StatusNeedsInput
	// StatusFinished is a terminal session (completed, killed, or archived);
	// its journal is retained (repo invariant #4) and it remains listable.
	StatusFinished
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
}

// CreateOptions configures [Supervisor.Create]. The zero value is the
// daemon's default: a credential-driven model in the daemon's working
// directory. The daemon supervisor's CreateOptions carries more fields
// (System, Params, MaxIters); the TUI only sets these two, so this local copy
// mirrors just them until the reconciliation PR imports the daemon type.
type CreateOptions struct {
	Model string
	Cwd   string
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

	// Reply answers a pending permission request identified by id: allow or
	// deny it, and — when remember is true — persist the verdict as a
	// standing grant for future matching calls (the SDK's
	// loop.RuleGuard/Grant path). The inline approval prompt's key handling
	// (see app.go/dialog.go) is the sole caller. sessionID scopes the reply
	// to the session the prompt was raised for; a daemon-backed Supervisor
	// need not put it on the wire itself (see internal/daemonbridge's
	// contract: the daemon resolves a permission request by id alone), but
	// an in-process one routes through it directly.
	Reply(ctx context.Context, sessionID, id string, allow, remember bool) error

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
}
