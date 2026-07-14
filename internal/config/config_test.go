package config_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/loop"
	"github.com/jedwards1230/agent-sdk-go/permission"

	"github.com/jedwards1230/gofer/internal/config"
)

// fakeContainer is a loop.Container that reports a fixed containability, so the
// guard-policy test can pin the allow→contain-or-ask branch deterministically
// regardless of the host's real sandbox runtime.
type fakeContainer struct{ can bool }

func (f fakeContainer) CanContain(context.Context, loop.ToolCall) (bool, error) { return f.can, nil }

func target(call loop.ToolCall) string {
	var in struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(call.Input, &in)
	return in.Command
}

func TestEngineDefaultPolicy(t *testing.T) {
	// An empty config compiles to a catch-all allow → contain-or-ask: every
	// request the engine sees resolves to allow, which the guard then routes
	// through the sandbox Container. It must never leave a call unmatched
	// (unmatched ⇒ ask), because that would escalate a plain read to a human.
	eng := config.Config{}.Engine()

	for _, tc := range []struct {
		name string
		req  permission.Request
	}{
		{"bash", permission.Request{Tool: "bash", Target: "ls -la"}},
		{"read", permission.Request{Tool: "read", Target: "/etc/hosts"}},
		{"unknown tool", permission.Request{Tool: "mcp__whatever", Target: "x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict, _, matched := eng.Evaluate(tc.req)
			if !matched {
				t.Fatalf("Evaluate(%+v): matched=false, want the catch-all allow to match", tc.req)
			}
			if verdict != event.VerdictAllow {
				t.Fatalf("Evaluate(%+v): verdict=%q, want allow", tc.req, verdict)
			}
		})
	}
}

func TestEngineConfigRulesOverrideDefault(t *testing.T) {
	// Command specifiers use the SDK's "prefix:*" grammar — a plain glob's `*`
	// stops at a "/" separator (path.Match), so a bare "rm *" would not match a
	// command containing a path.
	cfg := config.Config{Permissions: []config.Rule{
		{Verdict: "deny", Tool: "bash", Specifier: "rm:*"},
		{Verdict: "ask", Tool: "bash", Specifier: "curl:*"},
	}}
	eng := cfg.Engine()

	for _, tc := range []struct {
		name    string
		req     permission.Request
		want    event.Verdict
		wantSrc string
	}{
		{
			name:    "deny rule blocks",
			req:     permission.Request{Tool: "bash", Target: "rm -rf /"},
			want:    event.VerdictDeny,
			wantSrc: "config",
		},
		{
			name:    "ask rule asks",
			req:     permission.Request{Tool: "bash", Target: "curl example.com"},
			want:    event.VerdictAsk,
			wantSrc: "config",
		},
		{
			name:    "unmatched falls through to the default allow",
			req:     permission.Request{Tool: "bash", Target: "ls"},
			want:    event.VerdictAllow,
			wantSrc: "default",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict, rule, matched := eng.Evaluate(tc.req)
			if !matched {
				t.Fatalf("Evaluate(%+v): matched=false", tc.req)
			}
			if verdict != tc.want {
				t.Fatalf("Evaluate(%+v): verdict=%q, want %q", tc.req, verdict, tc.want)
			}
			if rule.Source != tc.wantSrc {
				t.Fatalf("Evaluate(%+v): rule.Source=%q, want %q", tc.req, rule.Source, tc.wantSrc)
			}
		})
	}
}

// TestGuardPolicyFromConfig drives the compiled engine through the SDK's
// RuleGuard to prove the end-to-end M3 policy: a config deny rule yields a Deny
// decision (the loop emits permission.resolved(deny) and blocks — no ask), an
// unmatched call falls through the catch-all allow to contain-or-ask, and an
// allow the container can hold runs contained.
func TestGuardPolicyFromConfig(t *testing.T) {
	cfg := config.Config{Permissions: []config.Rule{
		{Verdict: "deny", Tool: "bash", Specifier: "rm:*"},
	}}
	eng := cfg.Engine()

	call := func(cmd string) loop.ToolCall {
		return loop.ToolCall{ID: "c1", Name: "bash", Input: json.RawMessage(`{"command":"` + cmd + `"}`)}
	}

	t.Run("deny rule → Deny, no ask", func(t *testing.T) {
		g := loop.RuleGuard{Engine: eng, Container: fakeContainer{can: true}, Target: target}
		got := g.Evaluate(context.Background(), call("rm -rf /"))
		if got.Decision != loop.DecisionDeny {
			t.Fatalf("Decision = %v, want DecisionDeny", got.Decision)
		}
	})

	t.Run("unmatched allow, containable → RunContained", func(t *testing.T) {
		g := loop.RuleGuard{Engine: eng, Container: fakeContainer{can: true}, Target: target}
		got := g.Evaluate(context.Background(), call("ls -la"))
		if got.Decision != loop.DecisionRunContained {
			t.Fatalf("Decision = %v, want DecisionRunContained", got.Decision)
		}
	})

	t.Run("unmatched allow, not containable → Ask", func(t *testing.T) {
		g := loop.RuleGuard{Engine: eng, Container: fakeContainer{can: false}, Target: target}
		got := g.Evaluate(context.Background(), call("ls -la"))
		if got.Decision != loop.DecisionAsk {
			t.Fatalf("Decision = %v, want DecisionAsk", got.Decision)
		}
	})
}

func TestLoadMissingFileIsDefault(t *testing.T) {
	// A missing config file is not an error: it yields the zero Config whose
	// Engine is the default contain-or-ask policy.
	cfg, err := config.Load(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("Load(missing): %v", err)
	}
	if len(cfg.Permissions) != 0 {
		t.Fatalf("Load(missing): Permissions=%v, want empty", cfg.Permissions)
	}
	verdict, _, matched := cfg.Engine().Evaluate(permission.Request{Tool: "bash", Target: "ls"})
	if !matched || verdict != event.VerdictAllow {
		t.Fatalf("default engine: (%q, matched=%v), want (allow, true)", verdict, matched)
	}
}

func TestLoadParsesRules(t *testing.T) {
	dir := t.TempDir()
	path := config.DefaultPath(dir)
	body := `{"permissions":[{"verdict":"deny","tool":"bash","specifier":"rm:*"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Permissions) != 1 || cfg.Permissions[0].Verdict != "deny" {
		t.Fatalf("Load: Permissions=%+v, want one deny rule", cfg.Permissions)
	}
	verdict, _, _ := cfg.Engine().Evaluate(permission.Request{Tool: "bash", Target: "rm -rf /"})
	if verdict != event.VerdictDeny {
		t.Fatalf("compiled deny rule: verdict=%q, want deny", verdict)
	}
}

func TestLoadRejectsBadVerdict(t *testing.T) {
	dir := t.TempDir()
	path := config.DefaultPath(dir)
	if err := os.WriteFile(path, []byte(`{"permissions":[{"verdict":"den","tool":"bash"}]}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := config.Load(path); err == nil {
		t.Fatal("Load: want error for unknown verdict, got nil")
	}
}

func TestLoadRejectsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := config.DefaultPath(dir)
	if err := os.WriteFile(path, []byte(`{not json`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := config.Load(path); err == nil {
		t.Fatal("Load: want error for malformed JSON, got nil")
	}
}
