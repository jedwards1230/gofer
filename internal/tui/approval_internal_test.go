package tui

// approval_internal_test.go lives in package tui because it exercises the
// approval prompt's unexported helpers directly — commandBody, the escape
// hatch, and the rationale paragraph/header formatting. Their outputs are what
// the goldens in golden_test.go (package tui_test) then lock as rendered text;
// testing them here means a golden diff says "the render moved", not "some
// sentence somewhere changed".
//
// The rationale's DERIVATION (what the sentences say) moved to
// internal/permrationale when the daemon started answering
// session/explain_permission with the same grammar — see that package's tests.

import (
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/acp"

	"github.com/jedwards1230/gofer/internal/permrationale"
	"github.com/jedwards1230/gofer/internal/tui/theme"
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

// TestRationaleParagraphsPrefersTheAuthoritativeRationale pins which rationale
// the render uses: the agent's own once an explain has answered, the local
// derivation until then — and the escape hatch beneath either, since it is
// this client's advice about this client and never comes off the wire.
func TestRationaleParagraphsPrefersTheAuthoritativeRationale(t *testing.T) {
	p := pendingApproval{
		tool:  "bash",
		spec:  map[string]any{"cmd": "rm -rf /tmp/x"},
		trace: []string{"rule: unmatched", "containable: false"},
	}

	local := rationaleParagraphs(p)
	wantLocal := permrationale.Derive(p.tool, p.trace).Reason
	if local[0] != wantLocal {
		t.Errorf("local reason = %q, want the derivation %q", local[0], wantLocal)
	}
	if !strings.Contains(local[1], "unmatched") || !strings.Contains(local[1], "containable: false") {
		t.Errorf("local policy paragraph = %q, want the label and the raw trace entry", local[1])
	}

	p.rationale = &acp.PermissionRationale{
		Reason: "The agent gated this because the sandbox profile forbids writes outside the workspace.",
		Policy: "workspace-write",
		Source: "project",
		Trace:  []string{"rule: workspace-write", "path: /tmp/x"},
	}
	authoritative := rationaleParagraphs(p)
	if authoritative[0] != p.rationale.Reason {
		t.Errorf("reason = %q, want the agent's own %q", authoritative[0], p.rationale.Reason)
	}
	for _, want := range []string{"workspace-write", "path: /tmp/x", "source: project"} {
		if !strings.Contains(authoritative[1], want) {
			t.Errorf("policy paragraph = %q, want it to carry %q", authoritative[1], want)
		}
	}
	if strings.Count(authoritative[1], "workspace-write") != 1 {
		t.Errorf("policy paragraph = %q, want the label printed once, not once per source", authoritative[1])
	}
	// The escape hatch is this client's own, on both paths.
	for _, paras := range [][]string{local, authoritative} {
		if hatch := paras[len(paras)-1]; !strings.Contains(hatch, "Press `r`") {
			t.Errorf("last paragraph = %q, want the client-side escape hatch", hatch)
		}
	}
}

// TestPolicyParagraphDegenerateShapes covers a rationale that carries less
// than gofer's own does: no provenance at all (no paragraph rather than a
// bare "Policy:"), a trace with no label (the trace still speaks), and a
// source that merely repeats the label (said once).
func TestPolicyParagraphDegenerateShapes(t *testing.T) {
	tests := []struct {
		name string
		r    acp.PermissionRationale
		want string
	}{
		{"no provenance at all", acp.PermissionRationale{Reason: "gated"}, ""},
		{
			name: "trace without a rule entry",
			r:    acp.PermissionRationale{Trace: []string{"hook: PreToolUse"}},
			want: "Policy: hook: PreToolUse",
		},
		{
			name: "policy label with no trace",
			r:    acp.PermissionRationale{Policy: "unmatched"},
			want: "Policy: unmatched",
		},
		{
			name: "source repeating the label is not repeated",
			r:    acp.PermissionRationale{Policy: "config", Source: "config", Trace: []string{"rule: config"}},
			want: "Policy: config",
		},
		{
			name: "label recovered from the trace when Policy is empty",
			r:    acp.PermissionRationale{Trace: []string{"rule: session", "containable: true"}},
			want: "Policy: session · containable: true",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := policyParagraph(tc.r); got != tc.want {
				t.Errorf("policyParagraph(%+v) = %q, want %q", tc.r, got, tc.want)
			}
		})
	}
}

// TestRationaleHeaderLineStates pins the header's three states — the suffix is
// how a user tells the agent's answer from this client's approximation, and
// sees that a fetch is under way at all.
func TestRationaleHeaderLineStates(t *testing.T) {
	th := theme.Test()
	const header = "Why you're being asked"

	if got := rationaleHeaderLine(th, pendingApproval{}); got != header {
		t.Errorf("default header = %q, want the bare %q", got, header)
	}
	if got := rationaleHeaderLine(th, pendingApproval{explaining: true}); !strings.Contains(got, "explaining") {
		t.Errorf("in-flight header = %q, want an explaining marker", got)
	}
	explained := rationaleHeaderLine(th, pendingApproval{rationale: &acp.PermissionRationale{Reason: "because"}})
	if !strings.Contains(explained, "agent's answer") {
		t.Errorf("explained header = %q, want it to name the agent's answer", explained)
	}
}

// TestRationaleLinesCollapse pins the short-frame collapse: only the opening
// paragraph survives, the ctrl+e pointer replaces what was dropped, and the
// paragraphs themselves are never half-rendered (the collapse re-renders
// rather than slicing rows off a wrapped block).
func TestRationaleLinesCollapse(t *testing.T) {
	th := theme.Test()
	p := pendingApproval{
		tool:  "bash",
		spec:  map[string]any{"cmd": "rm -rf /tmp/x"},
		trace: []string{"rule: unmatched", "containable: false"},
	}

	full := rationaleLines(th, p, 78, false)
	collapsed := rationaleLines(th, p, 78, true)
	if len(collapsed) >= len(full) {
		t.Fatalf("collapsed rationale = %d rows, full = %d — collapsing must save rows", len(collapsed), len(full))
	}
	joined := strings.Join(collapsed, "\n")
	if !strings.Contains(joined, "ctrl+e to explain") {
		t.Errorf("collapsed rationale = %q, want the ctrl+e pointer", joined)
	}
	if strings.Contains(joined, "Policy:") || strings.Contains(joined, "Press `r`") {
		t.Errorf("collapsed rationale = %q, want only the opening paragraph", joined)
	}
	if !strings.Contains(strings.Join(full, " "), "Policy:") {
		t.Errorf("full rationale = %q, want the policy paragraph", full)
	}
}
