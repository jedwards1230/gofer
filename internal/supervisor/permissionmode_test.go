package supervisor

// permissionmode_test.go covers the load-bearing half of /yolo: that
// `session.permission_mode` actually GOVERNS the guard a new session is created
// with. A toggle that flips a setting nothing reads is theater, so these tests
// assert on the guard injected into runner.Options, not on the config value.
//
// White-box (package supervisor) because the yolo posture's guard is
// [yoloGuard], an unexported type — "which guard did this session get" is not
// observable from outside the package, and asserting it through a proxy (a
// permission event that never fires) would be a weaker test of the same thing.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/loop"
	"github.com/jedwards1230/agent-sdk-go/permission"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/config"
)

// stubSession is the minimal [Session] the NewSession seam can hand back: the
// tests here never drive a turn, they only inspect the runner.Options the
// supervisor built.
type stubSession struct {
	id     string
	broker *event.Broker
}

func newStubSession(id string) *stubSession {
	return &stubSession{id: id, broker: event.NewBroker(event.WithReplay(8))}
}

func (s *stubSession) ID() string                  { return s.id }
func (s *stubSession) JournalPath() string         { return "/dev/null" }
func (s *stubSession) Fold() []provider.Message    { return nil }
func (s *stubSession) Events() *event.Subscription { return s.broker.Subscribe(event.FilterAll, 64) }
func (s *stubSession) EventsLive() *event.Subscription {
	return s.broker.SubscribeLive(event.FilterAll, 64)
}
func (s *stubSession) Prompt(context.Context, string) error { return nil }
func (s *stubSession) Emit(e event.Event)                   { s.broker.Publish(e) }
func (s *stubSession) Cost() session.CostReport             { return session.CostReport{} }
func (s *stubSession) SetModel(string) error                { return nil }
func (s *stubSession) SetEffort(string) error               { return nil }
func (s *stubSession) Close() error                         { s.broker.Close(); return nil }

// guardCapture builds a supervisor whose NewSession seam records the guard the
// supervisor injected for each created session, with mode resolved through the
// caller's pointer so a test can change the posture BETWEEN creates — which is
// the behavior that matters: the mode is read per session, not sampled once at
// construction.
func guardCapture(t *testing.T, mode *config.PermissionMode) (*Supervisor, *[]loop.Guard) {
	t.Helper()
	sup, guards, _ := guardAndToolCapture(t, mode)
	return sup, guards
}

// guardAndToolCapture is guardCapture also recording the tool registry, for the
// assertions about what yolo must NOT take away.
func guardAndToolCapture(t *testing.T, mode *config.PermissionMode) (*Supervisor, *[]loop.Guard, *[]loop.ToolRegistry) {
	t.Helper()
	var guards []loop.Guard
	var tools []loop.ToolRegistry
	sup, err := New(Config{
		Root: t.TempDir(),
		PermissionMode: func() config.PermissionMode {
			return *mode
		},
		NewSession: func(_ context.Context, opts runner.Options) (Session, error) {
			guards = append(guards, opts.Guard)
			tools = append(tools, opts.Tools)
			return newStubSession("sess-1"), nil
		},
	})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })
	return sup, &guards, &tools
}

// TestYoloKeepsTheAskUserTool pins the line between "turn off the guardrail" and
// "take a capability away". ask_user (internal/decision) is the AGENT asking a
// question of its own accord — a tool, not a gate — so a yolo session must keep
// it. It is easy to lose by accident: yolo swaps the tool registry (to drop
// sandbox-wrapping), and the ask_user tool rides on that same registry.
func TestYoloKeepsTheAskUserTool(t *testing.T) {
	for _, mode := range []config.PermissionMode{config.PermissionModeAsk, config.PermissionModeYolo} {
		t.Run(string(mode), func(t *testing.T) {
			m := mode
			sup, _, tools := guardAndToolCapture(t, &m)
			if _, err := sup.Create(context.Background(), "", CreateOptions{Model: "claude-sonnet-5", Cwd: t.TempDir()}); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if len(*tools) != 1 {
				t.Fatalf("captured %d registries, want 1", len(*tools))
			}
			if _, ok := (*tools)[0].Get("ask_user"); !ok {
				t.Errorf("a %q session has no ask_user tool — yolo turns off the guardrail, not the agent's ability to ask", mode)
			}
			// The builtins must survive the swap too, or yolo would quietly ship
			// a session that cannot do anything at all.
			if _, ok := (*tools)[0].Get("bash"); !ok {
				t.Errorf("a %q session has no bash tool", mode)
			}
		})
	}
}

// TestPermissionModeGovernsTheGuardOfNewSessions is the mutation-checked core:
// neutralize sessionGuard's mode branch (always build the RuleGuard) and the
// yolo case below fails.
func TestPermissionModeGovernsTheGuardOfNewSessions(t *testing.T) {
	tests := []struct {
		name string
		mode config.PermissionMode
		want string // "rule" | "yolo"
	}{
		{"ask builds the contain-or-ask RuleGuard", config.PermissionModeAsk, "rule"},
		{"yolo builds the no-ask guard", config.PermissionModeYolo, "yolo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mode := tt.mode
			sup, guards := guardCapture(t, &mode)
			if _, err := sup.Create(context.Background(), "", CreateOptions{Model: "claude-sonnet-5", Cwd: t.TempDir()}); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if len(*guards) != 1 {
				t.Fatalf("captured %d guards, want 1", len(*guards))
			}
			switch got := (*guards)[0].(type) {
			case yoloGuard:
				if tt.want != "yolo" {
					t.Fatalf("mode %q built a yoloGuard; want the contain-or-ask RuleGuard", tt.mode)
				}
			case loop.RuleGuard:
				if tt.want != "rule" {
					t.Fatalf("mode %q built a RuleGuard; want the yolo guard", tt.mode)
				}
			default:
				t.Fatalf("mode %q built an unexpected guard %T", tt.mode, got)
			}
		})
	}
}

// TestPermissionModeIsResolvedPerSession is what makes the /yolo toggle reach a
// RUNNING gofer: the mode closure is consulted at each create, so a session
// started after the toggle gets the new posture without a restart.
func TestPermissionModeIsResolvedPerSession(t *testing.T) {
	mode := config.PermissionModeAsk
	sup, guards := guardCapture(t, &mode)

	if _, err := sup.Create(context.Background(), "", CreateOptions{Model: "claude-sonnet-5", Cwd: t.TempDir()}); err != nil {
		t.Fatalf("Create (ask): %v", err)
	}
	mode = config.PermissionModeYolo
	if _, err := sup.Create(context.Background(), "", CreateOptions{Model: "claude-sonnet-5", Cwd: t.TempDir()}); err != nil {
		t.Fatalf("Create (yolo): %v", err)
	}

	if len(*guards) != 2 {
		t.Fatalf("captured %d guards, want 2", len(*guards))
	}
	if _, ok := (*guards)[0].(loop.RuleGuard); !ok {
		t.Fatalf("the first session (created under ask) got %T, want loop.RuleGuard", (*guards)[0])
	}
	if _, ok := (*guards)[1].(yoloGuard); !ok {
		t.Fatalf("the second session (created after the toggle) got %T, want yoloGuard — "+
			"the mode is being sampled once instead of resolved per session", (*guards)[1])
	}
}

// TestNilPermissionModeDefaultsToAsk pins the seam's fail-safe default: a
// supervisor built without the knob (every test harness in this package, and
// any future embedder) behaves exactly as gofer did before it existed.
func TestNilPermissionModeDefaultsToAsk(t *testing.T) {
	var guards []loop.Guard
	sup, err := New(Config{
		Root: t.TempDir(),
		NewSession: func(_ context.Context, opts runner.Options) (Session, error) {
			guards = append(guards, opts.Guard)
			return newStubSession("sess-1"), nil
		},
	})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	if _, err := sup.Create(context.Background(), "", CreateOptions{Model: "claude-sonnet-5", Cwd: t.TempDir()}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(guards) != 1 {
		t.Fatalf("captured %d guards, want 1", len(guards))
	}
	if _, ok := guards[0].(loop.RuleGuard); !ok {
		t.Fatalf("a supervisor with no PermissionMode got %T, want loop.RuleGuard (contain-or-ask)", guards[0])
	}
}

// TestYoloGuardRunsEverythingExceptDenies pins what yolo actually means. The
// deny half is the safety-relevant one: a rule the user wrote as "never" must
// survive the toggle, or /yolo silently repeals the ruleset.
func TestYoloGuardRunsEverythingExceptDenies(t *testing.T) {
	g := yoloGuard{engine: permission.New(
		permission.Rule{Verdict: event.VerdictDeny, Tool: "bash", Specifier: "rm -rf /*", Source: "config"},
		permission.Rule{Verdict: event.VerdictAsk, Tool: "bash", Specifier: "*", Source: "config"},
	)}

	denied := g.Evaluate(context.Background(), bashCall(t, "rm -rf /tmp"))
	if denied.Decision != loop.DecisionDeny {
		t.Errorf("a deny-matched call resolved to %v, want DecisionDeny — /yolo must not repeal deny rules", denied.Decision)
	}

	// An ASK rule is exactly what yolo exists to skip.
	asked := g.Evaluate(context.Background(), bashCall(t, "ls -la"))
	if asked.Decision != loop.DecisionRunContained {
		t.Errorf("an ask-matched call resolved to %v, want it to run — yolo means no human in the loop", asked.Decision)
	}

	// An unmatched call is "ask" under the engine's fail-safe; yolo runs it.
	unmatched := yoloGuard{engine: permission.New()}.Evaluate(context.Background(), bashCall(t, "go test ./..."))
	if unmatched.Decision != loop.DecisionRunContained {
		t.Errorf("an unmatched call resolved to %v, want it to run", unmatched.Decision)
	}
}

// bashCall builds a bash [loop.ToolCall] with the given command, the shape
// [sandbox.ToolTarget] extracts a specifier from.
func bashCall(t *testing.T, command string) loop.ToolCall {
	t.Helper()
	input, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		t.Fatalf("marshal tool input: %v", err)
	}
	return loop.ToolCall{Name: "bash", Input: input}
}
