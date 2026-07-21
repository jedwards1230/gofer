// Package decision carries gofer's structured-decision round trip: an agent
// asks the human one or more titled, multiple-choice questions through the
// `ask_user` tool and blocks until a client answers them.
//
// # Why this originates in gofer
//
// The SDK ships the ACP wire vocabulary for structured decisions — the acp
// package's DecisionQuestion/DecisionOption/DecisionAnswer types and the
// session/request_decision method — but no event kind, no op, and no loop or
// runner seam to carry one. event.Event is a closed union (its withMeta
// method is unexported), so gofer cannot publish a decision onto the SDK's
// broker even if it wanted to. The request therefore originates HERE, in a
// gofer-registered tool, and travels a gofer-native transport that reuses the
// acp types as its payload vocabulary — exactly the precedent the
// gofer/permission_requested + permission.reply pair already set (ACP has no
// session/update variant for permissions either).
//
// This keeps CLAUDE.md invariant #1 (contract-only consumption) intact: the
// only SDK surfaces touched are exported ones — runner.Options.Tools,
// loop.ToolRegistry, tool.Tool, and the acp types.
//
// # Shape
//
// [Gate] is one session's registry of outstanding requests: the decision-side
// analogue of the SDK's loop.Gate for permissions. The tool calls
// [Gate.Request] (which blocks the agent turn); a client subscribes with
// [Gate.Subscribe] and resolves with [Gate.Answer]. Domain types are
// deliberately thin over acp — this package adds only what the acp types do
// not carry: a gofer-assigned request id, the open/resolved stream, and the
// id-assignment scheme (see [AssignIDs]).
//
// # Escape-hatch defaults
//
// [DefaultAllowFreeText] and [DefaultAllowChat] are both true, and the tool's
// input decodes those two fields as *bool so an omitted field is
// distinguishable from an explicit false. A forced-choice prompt with no way
// out is a trap, so the agent must opt OUT of the escape hatches rather than
// opt in. They live here as consts rather than in internal/config: making them
// session-scoped config would mean threading a config value through the
// supervisor into the tool for a knob no user has asked for. When one does,
// this is the value config overrides.
package decision

import (
	"errors"
	"fmt"

	"github.com/jedwards1230/agent-sdk-go/acp"
)

// ErrNoClient reports that a decision was requested while no client was
// subscribed to see it. It is returned by [Gate.Request] immediately, without
// opening a request: a question nobody can answer must not hang the agent's
// turn until the ctx expires. The tool turns it into a model-facing "continue
// in conversation instead" result rather than an error.
var ErrNoClient = errors.New("no client attached to answer decisions")

// ErrUnknownRequest reports that [Gate.Answer] named a request id that is not
// open — it was never opened, was already answered (possibly by another peer),
// or was dropped when its turn was interrupted.
var ErrUnknownRequest = errors.New("unknown decision request")

// Escape-hatch defaults for a question the model did not explicitly configure.
// Both are true; see the package doc for why the agent must opt out rather
// than in.
const (
	// DefaultAllowFreeText is the value of a question's AllowFreeText when the
	// model omits the field.
	DefaultAllowFreeText = true
	// DefaultAllowChat is the value of a question's AllowChat when the model
	// omits the field.
	DefaultAllowChat = true
)

// Request is one outstanding structured-decision request: a batch of questions
// the agent is blocked on. Its Questions carry gofer-assigned ids (see
// [AssignIDs]) and are treated as read-only once the request is open — a
// snapshot from [Gate.Open] or an [Update] shares them.
type Request struct {
	// ID identifies the request within its session. It is gofer-assigned,
	// monotonic, and formatted "dec-N" — deterministic on purpose, so goldens
	// and the model-facing result text stay stable.
	ID string
	// SessionID is the session whose turn is blocked on this request.
	SessionID string
	// Questions are the questions to answer, in the order the model asked
	// them and in the order answers are returned.
	Questions []acp.DecisionQuestion
}

// UpdateKind discriminates an [Update].
type UpdateKind int

const (
	// UpdateRequested reports that a request opened — or, on a fresh
	// [Gate.Subscribe], replays one that is already open.
	UpdateRequested UpdateKind = iota
	// UpdateResolved reports that a request left the open set: it was
	// answered (possibly by another peer) or its turn was interrupted. Only
	// Request.ID and Request.SessionID are meaningful on it.
	UpdateResolved
)

// String returns the kind's name, for test failure messages and logs.
func (k UpdateKind) String() string {
	switch k {
	case UpdateRequested:
		return "requested"
	case UpdateResolved:
		return "resolved"
	default:
		return "unknown"
	}
}

// Update is one change to a session's open decision set, delivered on a
// [Subscription]. On [UpdateResolved] only Request.ID/Request.SessionID are
// meaningful — a client uses them to clear a prompt it is still rendering.
type Update struct {
	Kind    UpdateKind
	Request Request
}

// AssignIDs returns a copy of questions with gofer-assigned ids stamped on:
// "q1", "q2", … for questions and "q1o1", "q1o2", … for each question's
// options. The MODEL never supplies these ids. Two reasons: an answer can then
// never reference an id the model hallucinated (every id in play was minted
// here from a position), and the id space is stable across runs, which is what
// makes the tool's result text and the TUI goldens deterministic.
//
// It is position-derived and therefore idempotent: re-stamping an
// already-stamped batch yields byte-identical ids. [Gate.Request] relies on
// that, re-stamping whatever it is handed so a caller cannot smuggle in ids of
// its own, while the tool keeps its own stamped copy to render labels against.
func AssignIDs(questions []acp.DecisionQuestion) []acp.DecisionQuestion {
	if questions == nil {
		return nil
	}
	out := make([]acp.DecisionQuestion, len(questions))
	for i, q := range questions {
		q.QuestionID = fmt.Sprintf("q%d", i+1)
		if q.Options != nil {
			// Copy rather than mutate in place: the caller's slice is its
			// own, and a shared backing array would let a later stamp of a
			// different batch rewrite ids out from under an open request.
			opts := make([]acp.DecisionOption, len(q.Options))
			for j, o := range q.Options {
				o.OptionID = fmt.Sprintf("%so%d", q.QuestionID, j+1)
				opts[j] = o
			}
			q.Options = opts
		}
		out[i] = q
	}
	return out
}
