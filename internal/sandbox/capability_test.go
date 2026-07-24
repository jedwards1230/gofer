package sandbox

import (
	"context"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/loop"
	"github.com/jedwards1230/agent-sdk-go/permission"
)

func TestContainableTool(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"bash", true},
		{"read", true},
		{"edit", true},
		{"write", true},
		{"ls", true},
		{"glob", true},
		{"grep", true},
		{"update_plan", true},
		{"ask_user", true},
		{"unknown_tool", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containableTool(tt.name); got != tt.want {
				t.Errorf("containableTool(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// availableContainer is both OS backends' CanContain rule with availability
// pinned on: seatbelt and bwrap each answer `c.available && containableTool(…)`,
// and whether a runtime is installed varies by host (CI's linux runner ships no
// bwrap, so the real sandbox.New() fails closed for every tool there). Pinning
// availability is what makes the guard's verdict below deterministic everywhere
// while still routing through the real capability table.
type availableContainer struct{}

func (availableContainer) CanContain(_ context.Context, call loop.ToolCall) (bool, error) {
	return containableTool(call.Name), nil
}

// TestRuleGuardDoesNotEscalateContainableTools is the guard-level consequence of
// the table above, which the table alone cannot show: loop.RuleGuard resolves an
// allow-matched call to DecisionRunContained only when the Container can hold
// it, and to DecisionAsk otherwise. That escalation is invisible to a test that
// runs a tool straight off the registry, which is how ask_user shipped omitted
// from the table in the first place.
//
// ask_user is the case that matters most: escalating it means the user gets a
// permission prompt asking whether the agent may ask them a question — before
// the decision widget can render — and a deny answers the agent's question with
// "permission denied".
func TestRuleGuardDoesNotEscalateContainableTools(t *testing.T) {
	guard := loop.RuleGuard{
		Engine:    permission.New(permission.Rule{Verdict: event.VerdictAllow, Tool: "*", Specifier: "*", Source: "test"}),
		Container: availableContainer{},
		Target:    ToolTarget,
	}
	tests := []struct {
		name string
		call loop.ToolCall
		want loop.Decision
	}{
		{"ask_user", loop.ToolCall{Name: "ask_user", Input: []byte(`{"questions":[]}`)}, loop.DecisionRunContained},
		{"update_plan", loop.ToolCall{Name: "update_plan"}, loop.DecisionRunContained},
		{"bash", loop.ToolCall{Name: "bash", Input: []byte(`{"command":"echo hi"}`)}, loop.DecisionRunContained},
		{"unknown tool", loop.ToolCall{Name: "unknown_tool"}, loop.DecisionAsk},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := guard.Evaluate(context.Background(), tt.call)
			if got.Decision != tt.want {
				t.Errorf("Evaluate(%q).Decision = %v, want %v (trace %v)", tt.call.Name, got.Decision, tt.want, got.Trace)
			}
		})
	}
}
