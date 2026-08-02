package supervisor

import (
	"context"
	"fmt"
	"strings"

	"github.com/jedwards1230/agent-sdk-go/provider"
)

// Subagents is the seam through which a RUNNING session creates child sessions
// and a FINISHED child reports its result back to its parent. It is the one
// abstraction agent-initiated spawning needs, because the two deployments
// answer it completely differently:
//
//   - The plain in-process daemon answers it with [Supervisor.Create] and
//     [Supervisor.Send] on the very same supervisor — see [localSubagents],
//     which [New] installs when handed [LocalSubagents] as its factory.
//   - A `--workers` session answers it over the wire: a worker's embedded
//     daemon is built with MaxSessions: 1 (a worker IS a single-session daemon
//     — see internal/worker's package doc), so it structurally cannot host a
//     sibling session. It dials the ROUTER instead, which already owns
//     one-worker-per-session bring-up. See [internal/worker.NewRouterSubagents].
//
// Both halves ride ONE seam on purpose: the same dial-back channel that carries
// a spawn carries the report, so there is exactly one place per deployment that
// knows how to reach the session tree.
//
// # This is not a second source of truth for parentage
//
// The seam CREATES the link; it never derives it. Depth is derived and the cap
// enforced by [Supervisor.resolveParent] against the on-disk sidecar, which
// stays the single authority for who a session's parent is (see the package
// doc's "Subagent sessions"). Nothing here subscribes to the SDK's
// event.SessionSpawned, deliberately: gofer implements parentage through its own
// sidecar, and a handler for that event would create a second, divergent record
// of the same fact.
type Subagents interface {
	// Spawn creates a child session of parentID running agent, seeded with
	// prompt as its first turn, and returns the child's id. The child inherits
	// the parent's model and working directory. It is ASYNCHRONOUS: it returns
	// once the child exists, never once the child has finished.
	Spawn(ctx context.Context, parentID, agent, prompt string) (childID string, err error)
	// Report delivers a finished child's result to parentID as its next prompt.
	// It queues rather than interrupting: a parent mid-turn (or mid-compaction)
	// runs the report the moment it is free, which is what makes a child's
	// completion invisible to the parent's own turn boundaries.
	Report(ctx context.Context, parentID, text string) error
}

// LocalSubagents is [Config.Subagents] for a deployment that can host sibling
// sessions itself: the in-process daemon and the fallback in-process supervisor
// the bare `gofer` TUI builds. It is the ONLY way to obtain the session-creating
// implementation, and every caller of it is a process that owns a supervisor
// with no MaxSessions cap.
//
// A `--workers` session worker must never pass this — see [Config.Subagents].
func LocalSubagents(s *Supervisor) Subagents { return localSubagents{sup: s} }

// localSubagents is the in-process [Subagents]: a spawn is a plain
// [Supervisor.Create] and a report a plain [Supervisor.Send], both on the SAME
// supervisor that hosts the parent. Reachable only through [LocalSubagents].
//
// # Why a reentrant Create from a running session's pump is safe
//
// The call arrives on the PARENT session's loop goroutine (a tool call inside
// its turn), so it is reentrant into the supervisor that is already running that
// session. It cannot deadlock, for three independent reasons:
//
//   - Create holds s.mu only inside [Supervisor.register], which releases it
//     BEFORE starting the child's pump — no lock is held across any call into a
//     session.
//   - Tool execution never runs inside Create's call stack: Create returns as
//     soon as the child is registered and its first prompt queued.
//   - The parent's own m.mu is not held during a tool call — the pump releases
//     it before m.sess.Prompt (see managed.pump's lock discipline).
//
// So this is concurrent-but-disjoint locking (parent goroutine takes s.mu, then
// the child's m.mu), never nested locking on one goroutine.
type localSubagents struct{ sup *Supervisor }

// localSubagents satisfies the seam — a signature drift fails the build here.
var _ Subagents = localSubagents{}

// Spawn creates the child, inheriting the parent's model and cwd.
//
// Inheritance rather than configuration is deliberate: a subagent is a
// delegation of THIS session's work, so running it against a different model or
// in a different directory than the session that asked for it would be a
// surprise the model has no way to anticipate. It also keeps the seam's
// signature to the three things the model actually chooses.
func (l localSubagents) Spawn(ctx context.Context, parentID, agent, prompt string) (string, error) {
	m, ok := l.sup.get(parentID)
	if !ok {
		// The caller IS the parent, so this only fires if the parent was killed
		// between issuing the tool call and it running.
		return "", fmt.Errorf("supervisor: spawn subagent of %s: %w", parentID, ErrNotLive)
	}
	info := m.info()
	child, err := l.sup.Create(ctx, prompt, CreateOptions{
		Model:    info.Model,
		Cwd:      info.Cwd,
		ParentID: parentID,
		Agent:    agent,
	})
	if err != nil {
		return "", err
	}
	return child.ID, nil
}

// Report enqueues text as parentID's next prompt.
func (l localSubagents) Report(ctx context.Context, parentID, text string) error {
	return l.sup.Send(ctx, parentID, text)
}

// subagentReportPrefix opens every child→parent report. It is a constant so the
// parent-side rendering, the tests, and anyone grepping a journal agree on one
// spelling.
const subagentReportPrefix = "subagent "

// formatSubagentReport renders a finished child's report as the prompt its
// parent receives. It names the child's id and agent up front so a parent whose
// several children report out of order can tell them apart, then carries the
// child's own final answer verbatim.
//
// A child that produced no assistant text still reports — silence from a
// delegated task is a result the parent needs, and a dropped report would leave
// the parent waiting forever on a child that already finished.
func formatSubagentReport(childID, agent, result string) string {
	var b strings.Builder
	b.WriteString(subagentReportPrefix)
	if agent != "" {
		b.WriteString(agent + " ")
	}
	b.WriteString("(session " + childID + ") finished")
	result = strings.TrimSpace(result)
	if result == "" {
		b.WriteString(" with no textual result.")
		return b.String()
	}
	b.WriteString(":\n\n")
	b.WriteString(result)
	return b.String()
}

// lastAssistantText returns the text of the LAST assistant message in msgs, or
// "" when there is none. It reads the session's own folded context (see
// [Session.Fold]) rather than tailing its event stream: the fold is what the
// session durably believes it said, so a report built from it survives a
// subscriber that missed a delta.
func lastAssistantText(msgs []provider.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != provider.RoleAssistant {
			continue
		}
		if text := strings.TrimSpace(msgs[i].Text()); text != "" {
			return text
		}
	}
	return ""
}
