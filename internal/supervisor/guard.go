package supervisor

// guard.go holds the "yolo" permission mode's [loop.Guard] — the non-default
// half of what [Supervisor.sessionGuard] builds per session. The default half
// stays the SDK's [loop.RuleGuard] (contain-or-ask); see supervisor.go.

import (
	"context"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/loop"
	"github.com/jedwards1230/agent-sdk-go/permission"

	"github.com/jedwards1230/gofer/internal/sandbox"
)

// yoloGuard is the guard a session created under
// [config.PermissionModeYolo] runs with: every tool call that no DENY rule
// blocks runs, with no human in the loop.
//
// It is deliberately not "no guard at all" (the SDK's documented nil-Guard
// behavior, loop/guard.go). A deny rule is the one thing in the ruleset that a
// user wrote down as "never, under any circumstances"; dropping it because a
// second knob was flipped would turn an explicit prohibition into a silent
// no-op. Everything softer than deny — an ask rule, an unmatched call, the
// contain-or-ask default — is exactly what yolo exists to skip, so it resolves
// to run.
//
// [loop.DecisionRunContained] is the SDK's "run it" decision; whether the call
// is ACTUALLY contained is decided by the tool registry, not by this value, and
// sessionGuard pairs this guard with the UNWRAPPED builtin registry. That is
// the second half of the mode: a contained bash refuses outright on a host it
// can't wrap (sandbox.containedBash), which under yolo would be a confusing
// tool error where the user asked for no gate at all.
type yoloGuard struct {
	engine *permission.Engine
}

// Evaluate implements [loop.Guard].
func (g yoloGuard) Evaluate(_ context.Context, call loop.ToolCall) loop.Guarding {
	target := sandbox.ToolTarget(call)
	verdict, rule, matched := g.engine.Evaluate(permission.Request{Tool: call.Name, Target: target})
	if verdict == event.VerdictDeny {
		return loop.Guarding{
			Decision: loop.DecisionDeny,
			Rule:     yoloRuleLabel(rule, matched),
			Trace:    []string{"rule: " + yoloRuleLabel(rule, matched), "mode: yolo (deny rules still block)"},
		}
	}
	return loop.Guarding{
		Decision: loop.DecisionRunContained,
		Rule:     "yolo",
		Trace:    []string{"mode: yolo (no ask, no containment)"},
	}
}

// Grant implements [loop.Granter]. A remember-grant under yolo is a no-op in
// effect — nothing asks — but recording it keeps the engine's history the same
// shape it would have had under ask, so a session that started in yolo carries
// no surprise when its grants are read back.
func (g yoloGuard) Grant(call loop.ToolCall) {
	g.engine.Grant(permission.Rule{
		Verdict:   event.VerdictAllow,
		Tool:      call.Name,
		Specifier: sandbox.ToolTarget(call),
		Source:    "session",
	})
}

// yoloRuleLabel mirrors the SDK's own (unexported) rule labeling for the deny
// path: the matched rule's Source when it has one, else a readable summary.
func yoloRuleLabel(rule permission.Rule, matched bool) string {
	switch {
	case !matched:
		return "unmatched"
	case rule.Source != "":
		return rule.Source
	default:
		return string(rule.Verdict) + " " + rule.Tool + "(" + rule.Specifier + ")"
	}
}
