package permrationale_test

// permrationale_test.go is the derivation's own coverage, moved here from
// internal/tui/approval_internal_test.go when the grammar became shared: the
// TUI's goldens lock what the RENDER of a rationale looks like, and these
// tests lock what the rationale SAYS. Keeping them apart means a golden diff
// reads "the render moved", not "some sentence somewhere changed".

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jedwards1230/gofer/internal/permrationale"
)

// TestSplitTrace covers the trace parser: the "rule: " entry's label is
// lifted out (by prefix, wherever it sits) and everything else is preserved
// in order, so a renderer can echo the raw entries verbatim beside it.
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
			rule, rest := permrationale.SplitTrace(tc.trace)
			if rule != tc.wantRule {
				t.Errorf("SplitTrace(%v) rule = %q, want %q", tc.trace, rule, tc.wantRule)
			}
			if !reflect.DeepEqual(rest, tc.wantRest) {
				t.Errorf("SplitTrace(%v) rest = %v, want %v", tc.trace, rest, tc.wantRest)
			}
		})
	}
}

// TestDeriveTraceShapes covers the derivation for every trace shape the SDK's
// loop.RuleGuard actually produces (see loop/guard.go's Evaluate,
// containOrAsk and ruleLabel), plus the empty-trace fallback. Each case
// asserts the reason prose, the policy label, the provenance, and that the
// raw trace survives verbatim — the guarantee that nothing the guard reported
// is dropped by the prose summarizing it.
func TestDeriveTraceShapes(t *testing.T) {
	tests := []struct {
		name       string
		trace      []string
		wantReason string
		wantPolicy string
		wantSource string
	}{
		{
			name:       "unmatched",
			trace:      []string{"rule: unmatched"},
			wantReason: "No permission rule matched this `bash` call, so gofer is asking before it runs.",
			wantPolicy: "unmatched",
		},
		{
			name:       "unmatched and not containable",
			trace:      []string{"rule: unmatched", "containable: false (no container configured)"},
			wantReason: "No permission rule matched this `bash` call, so gofer is asking before it runs. It also cannot be sandboxed on this host, so an allow rule alone will not let it run unattended.",
			wantPolicy: "unmatched",
		},
		{
			name:       "matched ask rule",
			trace:      []string{"rule: ask bash(rm *)"},
			wantReason: "A permission rule matched this `bash` call with the `ask` verdict.",
			wantPolicy: "ask bash(rm *)",
		},
		{
			name:       "named rule source",
			trace:      []string{"rule: config", "containable: false"},
			wantReason: "The `config` permission rule matched this `bash` call, and it was still gated for a decision. It also cannot be sandboxed on this host, so an allow rule alone will not let it run unattended.",
			wantPolicy: "config",
			wantSource: "config",
		},
		{
			name:       "session grant",
			trace:      []string{"rule: session"},
			wantReason: "The `session` permission rule matched this `bash` call, and it was still gated for a decision.",
			wantPolicy: "session",
			wantSource: "session",
		},
		{
			name:       "containability check errored",
			trace:      []string{"rule: default", "containable: error: seatbelt probe failed"},
			wantReason: "The `default` permission rule matched this `bash` call, and it was still gated for a decision.",
			wantPolicy: "default",
			wantSource: "default",
		},
		{
			name:       "empty trace falls back and claims no policy",
			trace:      nil,
			wantReason: "gofer could not determine why this `bash` call was gated.",
		},
		{
			name:       "unparseable trace keeps its entries in the trace",
			trace:      []string{"no rule"},
			wantReason: "gofer could not determine why this `bash` call was gated.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := permrationale.Derive("bash", tc.trace)
			if got.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", got.Reason, tc.wantReason)
			}
			if got.Policy != tc.wantPolicy {
				t.Errorf("Policy = %q, want %q", got.Policy, tc.wantPolicy)
			}
			if got.Source != tc.wantSource {
				t.Errorf("Source = %q, want %q", got.Source, tc.wantSource)
			}
			if !reflect.DeepEqual(got.Trace, tc.trace) {
				t.Errorf("Trace = %v, want the raw entries %v verbatim", got.Trace, tc.trace)
			}
		})
	}
}

// TestDeriveNamesTheToolHonestly pins the naming rule: a known tool is named
// in the prose (so a rationale read on its own, without the prompt above it,
// still says what was gated), and an un-named call degrades to "this call"
// rather than rendering empty backticks.
func TestDeriveNamesTheToolHonestly(t *testing.T) {
	withTool := permrationale.Derive("web_search", []string{"rule: unmatched"})
	if !strings.Contains(withTool.Reason, "this `web_search` call") {
		t.Errorf("Reason = %q, want it to name the tool", withTool.Reason)
	}

	noTool := permrationale.Derive("", []string{"rule: unmatched"})
	if strings.Contains(noTool.Reason, "``") {
		t.Errorf("Reason = %q, want no empty backticks for an un-named call", noTool.Reason)
	}
	if !strings.Contains(noTool.Reason, "this call") {
		t.Errorf("Reason = %q, want the un-named fallback %q", noTool.Reason, "this call")
	}
}

// TestDeriveSourceOnlyForGoferLabels pins the honesty rule on provenance: only
// the three labels gofer itself stamps resolve to a Source; anything else —
// including the SDK's own "<verdict> <tool>(<specifier>)" summary label —
// leaves it empty rather than inventing an origin.
func TestDeriveSourceOnlyForGoferLabels(t *testing.T) {
	for _, label := range []string{"session", "config", "default"} {
		if got := permrationale.Derive("bash", []string{"rule: " + label}).Source; got != label {
			t.Errorf("Derive(rule: %s).Source = %q, want %q", label, got, label)
		}
	}
	for _, label := range []string{"unmatched", "ask bash(rm *)", "some-plugin-hook"} {
		if got := permrationale.Derive("bash", []string{"rule: " + label}).Source; got != "" {
			t.Errorf("Derive(rule: %s).Source = %q, want empty (unknown provenance)", label, got)
		}
	}
}
