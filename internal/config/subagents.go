package config

import "strings"

// Subagents configures gofer's AGENT-INITIATED subagent spawning: whether a
// running session may create child sessions of its own, and which agent
// identities it is told about.
//
// The zero value is DISABLED, and that polarity is the whole point. Subagents
// are an extension feature gofer layers on top of the SDK, not part of the
// baseline product: a user who never opts in must never see the spawn tool in a
// session's tool surface, pay its context cost, or reason about a session tree.
// So this section gates the tool's EXISTENCE — an unconfigured gofer builds a
// session's registry exactly as it did before agent-initiated spawning existed
// (proven by internal/supervisor's TestSessionGuard_PreloadModeByteIdentical).
//
// It is deliberately NOT the depth cap. [Session.MaxSubagentDepth] already
// governs how deep a tree may nest and stays where it is: that knob answers "how
// far", this one answers "at all". The two are independent because the
// OPERATOR-driven path (`gofer run --parent <id> --agent <name>`) has always
// been available and is unaffected by this section — an operator explicitly
// asking for a child session gets one whether or not the model may ask for its
// own.
type Subagents struct {
	// Enabled turns agent-initiated spawning on: the `spawn_subagent` tool is
	// registered into every new session's tool surface, and a child session
	// reports its result back to its parent when its first turn settles.
	//
	// A plain bool, not the *bool this package uses elsewhere, because there is
	// no third state to distinguish: the default is OFF, so "unset" and
	// "explicitly false" mean the same thing and want the same behavior. (The
	// *bool spelling exists for knobs whose default is ON, where unset and false
	// genuinely differ.)
	Enabled bool `json:"enabled,omitempty"`

	// Agents optionally names the agent identities the spawn tool advertises to
	// the model, in the order to show them. When non-empty the tool's schema
	// constrains its `agent` argument to this list; when empty the argument is
	// free-form and forwarded verbatim (see [supervisor.CreateOptions.Agent]),
	// which is what the operator-driven `gofer run --agent` path has always done.
	//
	// gofer attaches no behavior to any value: an agent id is an attribution
	// label stamped onto the child's tool-call events, not a lookup key into a
	// registry of prompts. Naming the ones a project actually uses just spares
	// the model from guessing.
	Agents []string `json:"agents,omitempty"`
}

// IsEnabled reports whether agent-initiated spawning is on. The zero value is
// false — see the type doc for why that polarity is load-bearing.
func (s Subagents) IsEnabled() bool { return s.Enabled }

// AgentNames returns the advertised agent identities with blank and duplicate
// entries dropped, preserving the configured order. It returns nil (not an
// empty slice) when nothing survives, which is the caller's cue to leave the
// tool's `agent` argument free-form rather than constrain it to an empty set —
// a schema enum of zero values would make the argument unsatisfiable.
func (s Subagents) AgentNames() []string {
	var out []string
	seen := make(map[string]bool, len(s.Agents))
	for _, name := range s.Agents {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}
