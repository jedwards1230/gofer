package tui

// approval_internal_test.go lives in package tui because it exercises the
// approval prompt's unexported derivation helpers directly — commandBody,
// splitTrace, and approvalRationale. Their outputs are what the goldens in
// golden_test.go (package tui_test) then lock as rendered text; testing the
// derivation here means a golden diff says "the render moved", not "some
// sentence somewhere changed".

import (
	"reflect"
	"strings"
	"testing"
)

// TestCommandBodyPreferenceOrder covers commandBody's key preference: each
// key in turn is the sole carrier of the body, the documented order wins when
// several are present, a spec with none of them yields no key at all, and a
// non-string value under a command key is ignored rather than formatted as
// one (an edit tool's structured payload must fall through to the k=v list,
// not render as "map[...]").
func TestCommandBodyPreferenceOrder(t *testing.T) {
	tests := []struct {
		name     string
		spec     map[string]any
		wantBody string
		wantKey  string
	}{
		{"command", map[string]any{"command": "go test ./..."}, "go test ./...", "command"},
		{"cmd", map[string]any{"cmd": "ls -la"}, "ls -la", "cmd"},
		{"script", map[string]any{"script": "print('hi')"}, "print('hi')", "script"},
		{"file_path", map[string]any{"file_path": "internal/tui/app.go"}, "internal/tui/app.go", "file_path"},
		{"path", map[string]any{"path": "/etc/hosts"}, "/etc/hosts", "path"},
		{
			name:     "command wins over every later key",
			spec:     map[string]any{"path": "/etc/hosts", "cmd": "ls", "command": "go build"},
			wantBody: "go build",
			wantKey:  "command",
		},
		{
			name:     "cmd wins over path",
			spec:     map[string]any{"path": "/etc/hosts", "cmd": "ls"},
			wantBody: "ls",
			wantKey:  "cmd",
		},
		{"no command key", map[string]any{"timeout": 120}, "", ""},
		{"empty spec", map[string]any{}, "", ""},
		{"nil spec", nil, "", ""},
		{"empty string value is not a body", map[string]any{"command": ""}, "", ""},
		{"non-string value ignored", map[string]any{"command": map[string]any{"a": 1}}, "", ""},
		{
			name:     "non-string command falls through to the next key",
			spec:     map[string]any{"command": 42, "cmd": "ls"},
			wantBody: "ls",
			wantKey:  "cmd",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, key := commandBody(tc.spec)
			if body != tc.wantBody || key != tc.wantKey {
				t.Errorf("commandBody(%v) = (%q, %q), want (%q, %q)", tc.spec, body, key, tc.wantBody, tc.wantKey)
			}
		})
	}
}

// TestSplitTrace covers the trace parser: the "rule: " entry's label is
// lifted out (by prefix, wherever it sits) and everything else is preserved
// in order, so the Policy paragraph can echo the raw entries verbatim.
func TestSplitTrace(t *testing.T) {
	tests := []struct {
		name     string
		trace    []string
		wantRule string
		wantRest []string
	}{
		{"empty", nil, "", nil},
		{"rule only", []string{"rule: unmatched"}, "unmatched", nil},
		{
			name:     "rule plus containability",
			trace:    []string{"rule: unmatched", "containable: false"},
			wantRule: "unmatched",
			wantRest: []string{"containable: false"},
		},
		{
			name:     "rule need not come first",
			trace:    []string{"containable: true", "rule: config"},
			wantRule: "config",
			wantRest: []string{"containable: true"},
		},
		{
			name:     "no rule entry keeps every entry as detail",
			trace:    []string{"something the guard said"},
			wantRule: "",
			wantRest: []string{"something the guard said"},
		},
		{
			name:     "a second rule entry stays as detail rather than overwriting",
			trace:    []string{"rule: unmatched", "rule: shadow"},
			wantRule: "unmatched",
			wantRest: []string{"rule: shadow"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rule, rest := splitTrace(tc.trace)
			if rule != tc.wantRule {
				t.Errorf("splitTrace(%v) rule = %q, want %q", tc.trace, rule, tc.wantRule)
			}
			if !reflect.DeepEqual(rest, tc.wantRest) {
				t.Errorf("splitTrace(%v) rest = %v, want %v", tc.trace, rest, tc.wantRest)
			}
		})
	}
}

// TestApprovalRationaleTraceShapes covers the trace→paragraphs derivation for
// every trace shape the SDK's loop.RuleGuard actually produces (see
// loop/guard.go's Evaluate, containOrAsk and ruleLabel), plus the empty-trace
// fallback. Each case asserts the paragraph count, the reason sentence, and —
// where the shape carries policy detail — that the raw trace entries survive
// verbatim into the Policy paragraph.
func TestApprovalRationaleTraceShapes(t *testing.T) {
	spec := map[string]any{"cmd": "go test ./..."}

	tests := []struct {
		name       string
		trace      []string
		wantParas  int
		wantReason string
		wantPolicy string
	}{
		{
			name:       "unmatched",
			trace:      []string{"rule: unmatched"},
			wantParas:  3,
			wantReason: "No permission rule matched this call, so gofer is asking before it runs.",
			wantPolicy: "Policy: unmatched",
		},
		{
			name:       "unmatched and not containable",
			trace:      []string{"rule: unmatched", "containable: false (no container configured)"},
			wantParas:  3,
			wantReason: "No permission rule matched this call, so gofer is asking before it runs. It also cannot be sandboxed on this host, so an allow rule alone will not let it run unattended.",
			wantPolicy: "Policy: unmatched · containable: false (no container configured)",
		},
		{
			name:       "matched ask rule",
			trace:      []string{"rule: ask bash(rm *)"},
			wantParas:  3,
			wantReason: "A permission rule matched this call with the `ask` verdict.",
			wantPolicy: "Policy: ask bash(rm *)",
		},
		{
			name:       "named rule source",
			trace:      []string{"rule: config", "containable: false"},
			wantParas:  3,
			wantReason: "The `config` permission rule matched this call, and it was still gated for a decision. It also cannot be sandboxed on this host, so an allow rule alone will not let it run unattended.",
			wantPolicy: "Policy: config · containable: false",
		},
		{
			name:       "containability check errored",
			trace:      []string{"rule: default", "containable: error: seatbelt probe failed"},
			wantParas:  3,
			wantReason: "The `default` permission rule matched this call, and it was still gated for a decision.",
			wantPolicy: "Policy: default · containable: error: seatbelt probe failed",
		},
		{
			name:       "empty trace falls back and prints no policy",
			trace:      nil,
			wantParas:  2,
			wantReason: "gofer could not determine why this call was gated.",
		},
		{
			name:       "unparseable trace keeps its entries as policy detail",
			trace:      []string{"no rule"},
			wantParas:  3,
			wantReason: "gofer could not determine why this call was gated.",
			wantPolicy: "Policy: no rule",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			paras := approvalRationale(pendingApproval{tool: "bash", spec: spec, trace: tc.trace})
			if len(paras) != tc.wantParas {
				t.Fatalf("approvalRationale(%v) = %d paragraphs %q, want %d", tc.trace, len(paras), paras, tc.wantParas)
			}
			if paras[0] != tc.wantReason {
				t.Errorf("reason paragraph = %q, want %q", paras[0], tc.wantReason)
			}
			if tc.wantPolicy != "" && paras[1] != tc.wantPolicy {
				t.Errorf("policy paragraph = %q, want %q", paras[1], tc.wantPolicy)
			}
			// Every raw trace entry must survive somewhere in the rendered
			// prose — the Policy paragraph exists so the guard's own words are
			// never silently dropped in favor of the summary sentence.
			joined := strings.Join(paras, "\n")
			for _, entry := range tc.trace {
				if raw := strings.TrimPrefix(entry, "rule: "); !strings.Contains(joined, raw) {
					t.Errorf("trace entry %q missing from the rationale:\n%s", entry, joined)
				}
			}
			// The escape hatch is always last, always concrete, and never
			// advertises an affordance this codebase doesn't have.
			hatch := paras[len(paras)-1]
			for _, want := range []string{"Press `r`", "`config.json`", `"tool": "bash"`, `"specifier": "go *"`} {
				if !strings.Contains(hatch, want) {
					t.Errorf("escape-hatch paragraph missing %q:\n%s", want, hatch)
				}
			}
		})
	}
}

// TestApprovalEscapeHatchWithoutCommandOmitsExample pins the honesty rule on
// the escape hatch: with no command body there is no first token to build an
// example specifier from, so the "e.g." clause is omitted rather than
// fabricated — while the two real escape hatches themselves still render.
func TestApprovalEscapeHatchWithoutCommandOmitsExample(t *testing.T) {
	got := approvalEscapeHatch(pendingApproval{tool: "web_search", spec: map[string]any{"query": "gofer"}})
	if strings.Contains(got, "e.g.") {
		t.Errorf("escape hatch invented an example with no command body: %q", got)
	}
	for _, want := range []string{"Press `r`", "`permissions`", "`config.json`", "to stop being asked."} {
		if !strings.Contains(got, want) {
			t.Errorf("escape hatch = %q, want it to contain %q", got, want)
		}
	}
}
