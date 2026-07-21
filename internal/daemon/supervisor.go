package daemon

import (
	"context"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// Supervisor is the exact set of methods [Daemon] drives on the session
// registry it hosts (every call site is in handlers.go). It was extracted from
// the concrete *[supervisor.Supervisor] the daemon took directly so the same
// ACP-over-WebSocket surface can front an alternate implementation — chiefly a
// remote proxy that forwards these calls over the daemon wire to another
// process, the router→worker relationship M6 introduces (see
// docs/milestones/M6-process-isolation.md). The in-process
// *[supervisor.Supervisor] satisfies it unchanged.
//
// The signatures are quoted verbatim from the supervisor's own methods,
// including its DTO types ([supervisor.SessionInfo], [supervisor.CreateOptions],
// [supervisor.ResumeOptions]): this is a seam for *hosting* the registry behind
// the daemon, not a reshaping of it. A proxy implementation reconstitutes those
// DTOs from the wire.
type Supervisor interface {
	// Create starts a new session. handleSessionNew uses only the returned
	// SessionInfo.ID.
	Create(ctx context.Context, prompt string, opts supervisor.CreateOptions) (supervisor.SessionInfo, error)
	// Resume reopens a persisted session as a live one.
	Resume(ctx context.Context, id string, opts supervisor.ResumeOptions) (supervisor.SessionInfo, error)
	// History returns a session's folded conversation for replay on attach.
	History(ctx context.Context, id string) ([]provider.Message, error)
	// AwaitSettled blocks until id's journal is safe to fold completely, then
	// returns nil: a live session reaching supervisor.StatusNeedsInput (the
	// observable signal that its runner's async journaling barrier has passed),
	// or a session with no live writer (offline / not hosted here — its on-disk
	// journal is already durable). It returns the ctx error (never a fold error)
	// when ctx is cancelled or its deadline fires first. The session/load caller
	// treats it as a best-effort wait: handleSessionLoad bounds it and folds
	// whatever is durable regardless of the outcome, which is what keeps a load
	// of a session genuinely mid-turn — one that never reaches needs-input, e.g.
	// an adopted worker blocked on a permission (design §7) — from deadlocking.
	// It closes the journaling-flush window in which a load would otherwise read
	// a SHORT history (issue #137). Distinct from History, which stays a
	// non-blocking accessor for List/diagnostics.
	AwaitSettled(ctx context.Context, id string) error
	// Interrupt cancels a session's in-flight turn.
	Interrupt(ctx context.Context, sessionID string) error
	// List enumerates every session, live and disk-only.
	List(ctx context.Context) ([]supervisor.SessionInfo, error)
	// Roster returns just the live sessions' snapshots.
	Roster(ctx context.Context) ([]supervisor.SessionInfo, error)
	// SetModel changes a session's model for its next turn.
	SetModel(ctx context.Context, sessionID, model string) error
	// SetEffort changes a session's reasoning effort for its next turn. An
	// empty effort clears the level back to the provider's default.
	SetEffort(ctx context.Context, sessionID, effort string) error
	// SubscribeLive returns a session's event stream without the retained
	// must-deliver backlog, for a caller about to drive a fresh turn.
	SubscribeLive(ctx context.Context, sessionID string) (*event.Subscription, error)
	// Send drives one turn (enqueuing if a turn is already in flight).
	Send(ctx context.Context, sessionID, prompt string) error
	// Reply answers a pending permission request by call id.
	Reply(sessionID string, op event.PermissionReply) error
	// EmitConfigOptions publishes a session's available config options.
	EmitConfigOptions(sessionID string, options []event.ConfigOption) error
	// Kill interrupts and terminates a session (keeping its journal).
	Kill(ctx context.Context, sessionID string) error
	// Archive drops a session from the roster (keeping its journal).
	Archive(ctx context.Context, sessionID string) error
}

// The in-process supervisor satisfies the daemon's hosting interface unchanged
// — a signature drift in either package fails the build here rather than at a
// call site.
var _ Supervisor = (*supervisor.Supervisor)(nil)

// FleetUsager is an OPTIONAL [Supervisor] capability: a fleet-wide Cost/Usage
// total across every live session, aggregated by the supervisor itself. The M6
// router implements it off its pushed roster cache (zero worker RPCs); the
// in-process supervisor deliberately does NOT — a single-process daemon has no
// per-worker fan-out to aggregate, and a client can sum the roster it already
// receives.
//
// It is a separate interface rather than a method on [Supervisor] precisely so
// the in-process supervisor stays untouched: [handleGoferFleet] type-asserts for
// it and reports the total as unsupported when the hosted supervisor does not
// provide one, so an in-process — or an older — daemon simply omits the
// fleet-total line instead of failing the call.
type FleetUsager interface {
	// FleetUsage returns the summed Cost and Usage of every live session.
	FleetUsage() (provider.Cost, provider.Usage)
}
