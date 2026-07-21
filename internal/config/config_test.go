package config_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

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

// TestAutoscrollDefaultsTrueOnMissingConfig covers the tui.autoscroll
// default: a missing config.json (the common case — an unconfigured gofer)
// must resolve to autoscroll ENABLED, even though the underlying field is a
// *bool whose zero value (nil) has to mean "unset", not "false" — see
// [config.TUI.AutoscrollEnabled]'s doc for why a plain bool can't carry this
// default safely through a JSON round trip.
func TestAutoscrollDefaultsTrueOnMissingConfig(t *testing.T) {
	cfg, err := config.Load(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("Load(missing): %v", err)
	}
	if !cfg.TUI.AutoscrollEnabled() {
		t.Fatal("AutoscrollEnabled() on a missing config = false, want true (the default)")
	}
}

// TestAutoscrollExplicitFalseRoundTrips covers the failure mode a plain bool
// would have here: Save-ing an explicit tui.autoscroll=false, then Load-ing
// it back, must still read back as false — not silently revert to the
// nil/absent default of true.
func TestAutoscrollExplicitFalseRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := config.DefaultPath(dir)

	disabled := false
	if err := config.Save(path, config.Config{TUI: config.TUI{Autoscroll: &disabled}}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.TUI.AutoscrollEnabled() {
		t.Fatal("AutoscrollEnabled() after Save/Load of an explicit false = true, want false")
	}

	// The explicit true case round-trips too, and is byte-distinguishable
	// from "unset" only in that both resolve to the same effective value —
	// AutoscrollEnabled is what every caller should read, not the raw field.
	enabled := true
	if err := config.Save(path, config.Config{TUI: config.TUI{Autoscroll: &enabled}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err = config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.TUI.AutoscrollEnabled() {
		t.Fatal("AutoscrollEnabled() after Save/Load of an explicit true = false, want true")
	}
}

// TestMouseDefaultsTrueOnMissingConfig covers the tui.mouse default: a
// missing config.json must resolve to mouse capture ENABLED, the same
// *bool nil-means-unset contract [TestAutoscrollDefaultsTrueOnMissingConfig]
// pins for tui.autoscroll (see [config.TUI.MouseEnabled]'s doc).
func TestMouseDefaultsTrueOnMissingConfig(t *testing.T) {
	cfg, err := config.Load(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if err != nil {
		t.Fatalf("Load(missing): %v", err)
	}
	if !cfg.TUI.MouseEnabled() {
		t.Fatal("MouseEnabled() on a missing config = false, want true (the default)")
	}
}

// TestMouseExplicitFalseRoundTrips is tui.mouse's counterpart to
// [TestAutoscrollExplicitFalseRoundTrips]: an explicit false must survive a
// Save/Load round trip rather than silently reverting to the nil/absent
// default of true.
func TestMouseExplicitFalseRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := config.DefaultPath(dir)

	disabled := false
	if err := config.Save(path, config.Config{TUI: config.TUI{Mouse: &disabled}}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.TUI.MouseEnabled() {
		t.Fatal("MouseEnabled() after Save/Load of an explicit false = true, want false")
	}

	enabled := true
	if err := config.Save(path, config.Config{TUI: config.TUI{Mouse: &enabled}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err = config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.TUI.MouseEnabled() {
		t.Fatal("MouseEnabled() after Save/Load of an explicit true = false, want true")
	}
}

// TestSessionLoadSettleTimeout covers the resolver for session/load's
// journaling-settle bound (issue #137): unset and non-positive values fall back
// to the default, an explicit positive value is taken as a millisecond bound.
func TestSessionLoadSettleTimeout(t *testing.T) {
	ms := func(v int) *int { return &v }
	tests := []struct {
		name string
		in   *int
		want time.Duration
	}{
		{"unset resolves to default", nil, config.DefaultLoadSettleTimeout},
		{"zero resolves to default", ms(0), config.DefaultLoadSettleTimeout},
		{"negative resolves to default", ms(-5), config.DefaultLoadSettleTimeout},
		{"explicit value is a millisecond bound", ms(500), 500 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := config.Session{LoadSettleTimeoutMS: tt.in}
			if got := s.LoadSettleTimeout(); got != tt.want {
				t.Fatalf("LoadSettleTimeout() = %s, want %s", got, tt.want)
			}
		})
	}
}

// TestTUIApprovalBodyLineLimit covers the resolver for the inline approval
// prompt's body row cap: unset and non-positive values fall back to the
// default (a zero-row body would hide the very call being approved — unlike
// tui.max_paste_bytes, 0 is NOT "unlimited" here), an explicit positive value
// is taken as the cap. The round trip through Save/Load pins that an explicit
// value actually survives on disk.
func TestTUIApprovalBodyLineLimit(t *testing.T) {
	n := func(v int) *int { return &v }
	tests := []struct {
		name string
		in   *int
		want int
	}{
		{"unset resolves to default", nil, config.DefaultApprovalBodyLines},
		{"zero resolves to default", n(0), config.DefaultApprovalBodyLines},
		{"negative resolves to default", n(-5), config.DefaultApprovalBodyLines},
		{"explicit value is the cap", n(4), 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tui := config.TUI{ApprovalBodyLines: tt.in}
			if got := tui.ApprovalBodyLineLimit(); got != tt.want {
				t.Fatalf("ApprovalBodyLineLimit() = %d, want %d", got, tt.want)
			}
		})
	}

	dir := t.TempDir()
	path := config.DefaultPath(dir)
	if err := config.Save(path, config.Config{TUI: config.TUI{ApprovalBodyLines: n(30)}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if limit := got.TUI.ApprovalBodyLineLimit(); limit != 30 {
		t.Fatalf("ApprovalBodyLineLimit() after Save/Load = %d, want 30", limit)
	}
}

// TestDaemonDrainTimeout covers the resolver for the graceful-shutdown drain
// bound: unset and non-positive values fall back to the default, an explicit
// positive value is taken as a millisecond bound.
func TestDaemonDrainTimeout(t *testing.T) {
	ms := func(v int) *int { return &v }
	tests := []struct {
		name string
		in   *int
		want time.Duration
	}{
		{"unset resolves to default", nil, config.DefaultDrainTimeout},
		{"zero resolves to default", ms(0), config.DefaultDrainTimeout},
		{"negative resolves to default", ms(-5), config.DefaultDrainTimeout},
		{"explicit value is a millisecond bound", ms(1500), 1500 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := config.Daemon{DrainTimeoutMS: tt.in}
			if got := d.DrainTimeout(); got != tt.want {
				t.Fatalf("DrainTimeout() = %s, want %s", got, tt.want)
			}
		})
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
