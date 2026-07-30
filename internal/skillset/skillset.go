// Package skillset wires the SDK's skill package (SKILL.md discovery with
// progressive disclosure — [github.com/jedwards1230/agent-sdk-go/skill])
// into a gofer session: resolving [config.Skills] into a [sdkskill.Load]
// call, applying [config.Skills.Disabled] (a gofer-native config concept the
// SDK's skill.Set has no notion of), and exposing the single
// [tool.Tool] a session's registry is wired with — the invocation surface
// internal/supervisor.sessionGuard registers alongside internal/decision's
// ask_user, matching the "gofer owns config and invocation, the SDK owns
// discovery" split the SDK's skill package doc calls out explicitly.
//
// Progressive disclosure is preserved end to end: [Load] never reads a
// skill's body (sdkskill.Set.Index carries no body field, so nothing here
// can reintroduce one), and [NewTool]'s Description only ever projects the
// truncated index — the body is read from disk in Run, once per invocation,
// via sdkskill.Set.Body.
package skillset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	sdkskill "github.com/jedwards1230/agent-sdk-go/skill"
	"github.com/jedwards1230/agent-sdk-go/tool"

	"github.com/jedwards1230/gofer/internal/config"
)

// Load resolves cfg's directories against root/cwd and its size caps, then
// discovers skills via the SDK's [sdkskill.Load]. The returned Diagnostics
// are exactly what the SDK reported — a skipped oversized/malformed/
// duplicate/symlinked candidate — never filtered or summarized; that is
// [Summarize]'s job. cfg.Disabled plays no part here: a disabled skill is
// still loaded (so re-enabling it needs no directory rescan) and is only
// excluded at the [NewTool] projection.
func Load(cfg config.Skills, root, cwd string) (*sdkskill.Set, []sdkskill.Diagnostic) {
	dirs := cfg.Directories(root, cwd)
	return sdkskill.Load(dirs, sdkskill.Options{
		MaxBodyBytes:      cfg.FileLimitBytes(),
		DescriptionBudget: cfg.DescriptionLimitBytes(),
	})
}

// Summarize renders diags as one operator-facing line, naming the first
// diagnostic and collapsing any others into a "+N more" count — the same
// shape [applyUserCommands] (internal/tui) reports a [usercmd.Warning] with,
// so a skipped skill and a skipped command file read the same way. Empty
// diags returns "".
func Summarize(diags []sdkskill.Diagnostic) string {
	if len(diags) == 0 {
		return ""
	}
	msg := "skills: skipped " + diags[0].Error()
	if n := len(diags) - 1; n > 0 {
		msg += fmt.Sprintf(" (+%d more)", n)
	}
	return msg
}

// ToolName is the name a [NewTool]-built tool registers under; it mirrors
// the SDK's own [sdkskill.ToolName] so a transcript or log naming "skill"
// means the same thing regardless of which layer emitted it.
const ToolName = sdkskill.ToolName

// filteredIndex returns set's discovery index with every name in
// cfg.Disabled removed. Computed fresh on every call (matching
// [sdkskill.Set.Index]'s own "no caching, Set is immutable, call again"
// contract) so a config edit that flips Disabled reaches the NEXT session
// this cfg resolver feeds — sessionGuard already re-resolves config.Skills
// per session for that reason.
func filteredIndex(set *sdkskill.Set, cfg config.Skills) []sdkskill.Meta {
	all := set.Index()
	out := make([]sdkskill.Meta, 0, len(all))
	for _, m := range all {
		if cfg.IsDisabled(m.Name) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// NewTool returns the session's skill-invocation tool and whether it has
// anything to offer. ok is false when every loaded skill is disabled (or
// none loaded at all) — the caller's cue to skip registering it entirely
// rather than adding a tool whose description is permanently "no skills are
// currently available", which is pure context-cost with no payoff (gofer's
// context-cost discipline: don't preload what nothing will use).
func NewTool(set *sdkskill.Set, cfg config.Skills) (t tool.Tool, ok bool) {
	if len(filteredIndex(set, cfg)) == 0 {
		return nil, false
	}
	return skillTool{set: set, cfg: cfg}, true
}

// skillTool is [NewTool]'s [tool.Tool]: a disabled-aware projection over one
// [sdkskill.Set], patterned directly on the SDK's own (unexported) skill
// tool — the same Description/Spec/Run shape — with cfg.Disabled applied at
// every read so a disabled skill is invisible to the model, not merely
// unlisted-but-still-runnable.
type skillTool struct {
	set *sdkskill.Set
	cfg config.Skills
}

var _ tool.Tool = skillTool{}

func (t skillTool) Name() string { return ToolName }

// Description lists the currently available (non-disabled) skills — name
// and truncated description — evaluated fresh on every call so a Set or
// Disabled list that changes between turns is reflected without extra
// wiring, matching the SDK skill tool's own freshness contract.
func (t skillTool) Description() string {
	idx := filteredIndex(t.set, t.cfg)
	var b strings.Builder
	b.WriteString("Loads the full instructions for a named skill. Available skills:\n")
	for _, m := range idx {
		fmt.Fprintf(&b, "- %s: %s\n", m.Name, m.Description)
	}
	return b.String()
}

func (t skillTool) Spec() tool.Schema {
	idx := filteredIndex(t.set, t.cfg)
	names := make([]string, 0, len(idx))
	for _, m := range idx {
		names = append(names, m.Name)
	}
	return tool.ObjectSchema([]string{"name"}, map[string]tool.Property{
		"name": {
			Type:        "string",
			Description: "The skill to load, from the tool description's list.",
			Enum:        names,
		},
	})
}

func (t skillTool) Run(_ context.Context, input json.RawMessage) (tool.Result, error) {
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return tool.Result{}, fmt.Errorf("skillset: invalid input: %w", err)
	}
	if strings.TrimSpace(args.Name) == "" {
		return tool.Result{}, errors.New("skillset: name is required")
	}
	// A disabled name is refused the same way an unknown one is (model-
	// correctable, not a malformed call) — see tool.Tool.Run's (Result,
	// error) split — even though it never appeared in Description/Spec: a
	// model can still type a name it saw in an earlier turn, before the
	// skill was disabled.
	if t.cfg.IsDisabled(args.Name) {
		return tool.Result{Content: fmt.Sprintf("skill %q is disabled", args.Name), IsError: true}, nil
	}
	body, err := t.set.Body(args.Name)
	if err != nil {
		return tool.Result{Content: err.Error(), IsError: true}, nil
	}
	// FullResult: the model explicitly asked to load this skill by name; the
	// body is already capped at cfg.FileLimitBytes() and must not be
	// re-truncated by the loop's spill excerpt behavior.
	return tool.Result{Content: body, FullResult: true}, nil
}
