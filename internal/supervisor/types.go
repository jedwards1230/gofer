package supervisor

import (
	"context"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"
)

// runState is a live session's internal pump run-state. It is deliberately
// unexported: clients observe the derived [SessionStatus] on a [SessionInfo]
// snapshot, never this. The pump drives idle⇄running; [SessionInfo.Status]
// is computed from it (plus queue depth) at snapshot time.
type runState string

const (
	stateIdle    runState = "idle"
	stateRunning runState = "running"
)

// SessionStatus is a live session's client-facing status, derived from its
// pump run-state and queue depth at snapshot time.
type SessionStatus int

const (
	// StatusWorking means a turn is in flight or a prompt is queued to run.
	StatusWorking SessionStatus = iota
	// StatusNeedsInput means the session is idle with an empty queue —
	// ready for the next prompt.
	StatusNeedsInput
	// StatusFinished is RESERVED and never emitted in M2: a gofer coding
	// session is never intrinsically finished — an idle session is
	// NeedsInput, i.e. ready for another prompt. It exists so the enum's
	// wire values are stable when a later milestone defines "finished".
	StatusFinished
)

// String renders a SessionStatus for logs and debugging.
func (s SessionStatus) String() string {
	switch s {
	case StatusWorking:
		return "working"
	case StatusNeedsInput:
		return "needs-input"
	case StatusFinished:
		return "finished"
	default:
		return "unknown"
	}
}

// Session is the subset of [runner.Runner] the supervisor drives. It exists
// so tests can inject a scripted fake in place of a real provider-backed
// runner; [runner.Runner] satisfies it unchanged (the SDK-promotion
// invariant this package's doc describes).
type Session interface {
	// ID returns the session's journal id.
	ID() string
	// JournalPath returns the session journal's JSONL file path.
	JournalPath() string
	// Fold returns the session's current folded context as provider messages.
	Fold() []provider.Message
	// Events returns a subscription to every event the session emits.
	Events() *event.Subscription
	// EventsLive returns a subscription to events emitted AFTER the call,
	// without the retained must-deliver backlog Events replays (see
	// [runner.Runner.EventsLive]). It is for a caller driving a new turn that
	// must not observe a prior turn's retained terminal event.
	EventsLive() *event.Subscription
	// Prompt drives one turn; a cancelled ctx interrupts it, leaving whatever
	// prefix had already settled durable on disk.
	Prompt(ctx context.Context, text string) error
	// Emit publishes a lifecycle event onto the session's own stream.
	Emit(e event.Event)
	// Cost returns the session's token/cost tally across every journaled turn.
	Cost() session.CostReport
	// Close shuts the session down, releasing its broker and journal.
	Close() error
}

// var _ Session asserts *runner.Runner satisfies Session unchanged, so a
// signature drift in either package fails the build, not a runtime surprise.
var _ Session = (*runner.Runner)(nil)

// SessionInfo is the single exported snapshot type for both live roster rows
// ([Supervisor.Roster], [Supervisor.WatchRoster]) and on-disk enumeration
// ([Supervisor.List]). Status is meaningful only when Live is true; a
// disk-only (archived/offline) entry from List carries Live=false and a
// zero-value Status.
type SessionInfo struct {
	ID        string
	Title     string // M2: first-prompt snippet, else project slug; may be ""
	Summary   string // M2: "" (reserved)
	Status    SessionStatus
	Model     string
	Cost      provider.Cost
	Usage     provider.Usage
	Pending   int // approvals; 0 in M2
	Artifacts int // 0 in M2
	Created   time.Time
	Updated   time.Time

	// Operational extras beyond the TUI's narrow interface (additive).
	Project     string
	JournalPath string
	Queued      int
	Live        bool // false for disk-only archived entries from List

	// Cwd is the working directory the session was created/resumed into.
	// Live sessions carry it from their [managed] bookkeeping; a disk-only
	// entry from [Supervisor.List] reads it back from the journal's
	// [session.EntryMeta] root entry (see [diskSessionInfo]) and leaves it ""
	// only for a legacy journal written before the SDK persisted it.
	Cwd string
}
