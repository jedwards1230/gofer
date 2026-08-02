// Package subagent is gofer's `spawn_subagent` [tool.Tool]: the model-facing
// surface of agent-initiated subagent spawning. Until it existed, only an
// OPERATOR could start a child session (`gofer run --parent <id> --agent
// <name>`); a running agent had no way to delegate work of its own.
//
// # Registration is conditional — the opt-in is the whole design
//
// [NewTool] returns ok=false when [config.Subagents] is disabled (its zero
// value) or when the [Spawner] seam is nil, and the caller
// (internal/supervisor's sessionGuard) must skip registration entirely rather
// than register a tool that can only ever error. This mirrors
// internal/skillset's NewTool exactly, and for a stronger reason: a user who
// never opts into subagents must get a session tool surface BYTE-IDENTICAL to
// one built before this feature existed — no extra schema in the request, no
// extra concept for the model to reason about. That property is pinned by
// internal/supervisor's TestSessionGuard_PreloadModeByteIdentical and
// TestSessionGuard_SpawnRegisteredOnlyWhenConfigured.
//
// # The tool learns its own session id after it is built
//
// sessionGuard runs BEFORE runner.New mints the session id, so a tool built
// there cannot know the id that will become the parent of the children it
// spawns. [Tool.Bind] closes that gap, on exactly the pattern
// internal/decision's Gate already uses: the supervisor binds the id in
// register, before the session is reachable and before any turn can run. An
// unbound tool refuses to spawn rather than guessing.
//
// # A refused spawn is a tool result, not a failed turn
//
// The depth cap ([supervisor.ErrDepthExceeded]) and every other refusal come
// back as tool.Result{IsError: true} carrying the actionable message, never as
// a Go error. A model that asks for one child too many should be told so and
// keep working — killing the parent's turn over it would turn a recoverable
// planning mistake into a lost turn. See [tool.Tool.Run]'s (Result, error)
// split: only ctx cancellation and undecodable input are Go errors here.
package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/jedwards1230/agent-sdk-go/tool"

	"github.com/jedwards1230/gofer/internal/config"
)

// ToolName is the name the model calls the spawn tool by.
const ToolName = "spawn_subagent"

// Spawner is the narrow seam [Tool] drives: create a child session of parentID
// running agent, seeded with prompt as its first turn, and return the child's
// id. It is the Spawn half of internal/supervisor's Subagents seam, restated
// here so this package stays a leaf over the SDK and internal/config — the
// supervisor imports THIS package, so it must not import the supervisor.
//
// The implementation owns every policy decision: which model and working
// directory the child inherits, the parent lookup, and the depth cap. The tool
// owns only the schema, the model-facing wording, and the (Result, error)
// split.
type Spawner interface {
	Spawn(ctx context.Context, parentID, agent, prompt string) (childID string, err error)
}

// Tool is the `spawn_subagent` [tool.Tool].
type Tool struct {
	spawner Spawner
	// agents is the advertised agent-identity list ([config.Subagents.AgentNames]),
	// nil when the operator named none — in which case the `agent` argument is
	// free-form. Fixed at construction: the config that produced it was already
	// re-read for THIS session (see sessionGuard), and a live session's tool
	// surface never changes.
	agents []string

	// mu guards sessionID, which is written once by [Tool.Bind] (from the
	// supervisor's register, before the session is reachable) and read by every
	// Run (from the session's own loop goroutine). Those are different
	// goroutines, so the write needs publishing even though it happens-before
	// every read in practice.
	mu        sync.Mutex
	sessionID string
}

// Tool is a tool the SDK's registry accepts. Failing this assertion means the
// tool contract drifted from what gofer implements.
var _ tool.Tool = (*Tool)(nil)

// NewTool returns the spawn tool for cfg and whether it should be registered at
// all. ok is false when cfg disables subagents (the zero value) or spawner is
// nil — see the package doc. Callers MUST branch on ok rather than nil-checking
// the returned pointer: a *Tool stored into a tool.Tool interface is never a nil
// interface, so a nil return assigned into a registry would panic on first use
// instead of being skipped.
func NewTool(spawner Spawner, cfg config.Subagents) (t *Tool, ok bool) {
	if spawner == nil || !cfg.IsEnabled() {
		return nil, false
	}
	return &Tool{spawner: spawner, agents: cfg.AgentNames()}, true
}

// Bind stamps the tool's OWN session id — the parent of every child it spawns —
// the moment it is knowable. See the package doc for why construction cannot do
// it. Calling it twice is legal (the last write wins); never calling it leaves
// the tool refusing to spawn.
func (t *Tool) Bind(sessionID string) {
	t.mu.Lock()
	t.sessionID = sessionID
	t.mu.Unlock()
}

// boundID returns the bound session id, or "" when Bind was never called.
func (t *Tool) boundID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID
}

// Name returns "spawn_subagent".
func (*Tool) Name() string { return ToolName }

// Description returns the model-facing description. It names both what a
// subagent IS in gofer (a real, separate session — not a black box inside this
// turn) and the asynchronous contract, because both change how a model should
// use it: the call returns immediately, and the child's answer arrives later as
// a new message in this conversation.
func (t *Tool) Description() string {
	var b strings.Builder
	b.WriteString("Delegate a self-contained piece of work to a subagent: a real, separate " +
		"session with its own transcript and cost, linked to this one as its child. " +
		"Use it for work that is independent enough to describe once and hand off — a " +
		"focused investigation, a parallel review — rather than for anything you need " +
		"to steer turn by turn.\n\n" +
		"This call returns as soon as the child session exists; it does NOT wait for " +
		"the child to finish. Keep working. When the child's first turn settles, its " +
		"result arrives here as a new message naming the child's id. Write the prompt " +
		"as a complete, standalone brief — the child does not see this conversation.")
	if len(t.agents) > 0 {
		b.WriteString("\n\nAvailable agents: " + strings.Join(t.agents, ", ") + ".")
	}
	return b.String()
}

// Spec returns the JSON Schema for the tool's input: a required "prompt" and an
// "agent" identity. agent is constrained to the configured list when the
// operator named one (see [config.Subagents.Agents]) and free-form otherwise.
func (t *Tool) Spec() tool.Schema {
	agent := tool.Property{
		Type: "string",
		Description: "The agent identity to run the child as, e.g. \"go-developer\". " +
			"It labels every tool call the child makes so an interleaved transcript " +
			"can be attributed.",
	}
	if len(t.agents) > 0 {
		agent.Enum = t.agents
		agent.Description += " Choose one of the configured agents."
	}
	return tool.ObjectSchema([]string{"prompt"}, map[string]tool.Property{
		"prompt": {
			Type: "string",
			Description: "The complete, standalone brief for the child session: what to do, " +
				"what context it needs, and what to report back. The child does not " +
				"see this conversation.",
		},
		"agent": agent,
	})
}

// input is the decoded shape of Run's argument, matching Spec exactly.
type input struct {
	Prompt string `json:"prompt"`
	Agent  string `json:"agent"`
}

// Run spawns the child and returns its id.
//
// Per [tool.Tool.Run]'s contract only two things are Go errors: input that
// cannot be decoded at all, and ctx cancellation (an interrupted turn, which
// the loop aborts rather than feeding back to the model). Everything the model
// could correct or react to — an empty prompt, an agent outside the configured
// set, an unbound tool, and every refusal the [Spawner] returns (depth cap,
// capacity, a closed supervisor) — comes back as an IsError [tool.Result]. See
// the package doc for why the depth cap in particular must not kill the turn.
func (t *Tool) Run(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	if err := ctx.Err(); err != nil {
		return tool.Result{}, err
	}
	var in input
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &in); err != nil {
			return tool.Result{}, fmt.Errorf("%s: decode input: %w", ToolName, err)
		}
	}
	if problem := t.validate(in); problem != "" {
		return tool.Result{IsError: true, Content: problem}, nil
	}

	parentID := t.boundID()
	if parentID == "" {
		// Unreachable while the supervisor binds in register (see the package
		// doc). Kept because the alternative — spawning with an empty parent —
		// would silently mint a ROOT session that dodges the depth cap, which is
		// exactly the failure this tool must never have.
		return tool.Result{IsError: true, Content: ToolName + ": this session cannot spawn subagents (no session id bound)"}, nil
	}

	childID, err := t.spawner.Spawn(ctx, parentID, strings.TrimSpace(in.Agent), in.Prompt)
	if err != nil {
		// A cancelled turn is the one refusal that is NOT the model's to react
		// to: the turn is being torn down, so there is nothing to report into.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return tool.Result{}, err
		}
		return tool.Result{IsError: true, Content: fmt.Sprintf("%s: %v", ToolName, err)}, nil
	}
	return tool.Result{
		Content: fmt.Sprintf("spawned subagent session %s%s — it is running now; its result will arrive as a message in this conversation when its first turn settles.",
			childID, agentSuffix(in.Agent)),
		Metadata: tool.Metadata{Extra: map[string]any{"session_id": childID, "agent": strings.TrimSpace(in.Agent)}},
	}, nil
}

// validate reports the first thing wrong with a decoded call as a model-facing
// sentence naming the fix, or "" when the call is well formed.
func (t *Tool) validate(in input) string {
	if strings.TrimSpace(in.Prompt) == "" {
		return ToolName + ": prompt must not be empty — describe the work the subagent should do"
	}
	agent := strings.TrimSpace(in.Agent)
	if len(t.agents) == 0 || agent == "" {
		// Free-form (or unset) agent ids are legal — the operator-driven
		// `gofer run --agent` path has always accepted any label, and an empty
		// one is simply un-attributed.
		return ""
	}
	for _, name := range t.agents {
		if name == agent {
			return ""
		}
	}
	return fmt.Sprintf("%s: unknown agent %q — configured agents are: %s", ToolName, agent, strings.Join(t.agents, ", "))
}

// agentSuffix renders the " running <agent>" clause, or "" for an
// un-attributed child.
func agentSuffix(agent string) string {
	if agent = strings.TrimSpace(agent); agent != "" {
		return " running " + agent
	}
	return ""
}
