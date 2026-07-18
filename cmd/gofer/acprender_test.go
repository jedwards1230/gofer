package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestACPRenderUsageUpdate covers the usage_update session/update variant the
// SDK now emits (and gofer forwards) — the client-side counterpart to the
// daemon-wire proof in internal/daemon/usage_test.go. It asserts the renderer
// surfaces the token counters and, when present, the priced cost.
func TestACPRenderUsageUpdate(t *testing.T) {
	tests := []struct {
		name   string
		update string
		want   []string
	}{
		{
			name:   "priced usage renders tokens and cost",
			update: `{"sessionUpdate":"usage_update","used":1500,"size":1000000,"cost":{"amount":0.0105,"currency":"USD"}}`,
			want:   []string{"usage", "1500/1000000 tokens", "$0.0105 USD"},
		},
		{
			name:   "unpriced usage renders tokens only",
			update: `{"sessionUpdate":"usage_update","used":130,"size":200000}`,
			want:   []string{"usage", "130/200000 tokens"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf strings.Builder
			r := newACPRenderer(&buf, false)
			params, _ := json.Marshal(map[string]json.RawMessage{"update": json.RawMessage(tt.update)})
			if err := r.render(params); err != nil {
				t.Fatalf("render: %v", err)
			}
			got := buf.String()
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("render output %q missing %q", got, w)
				}
			}
		})
	}
}

// TestACPRenderToolCallUpdateDiff covers the tool_call_update variant carrying
// "diff" content blocks (the SDK v0.7.0 edit/write tools now produce them): the
// renderer appends a compact "edited <paths>" summary and never leaks the
// diffs' before/after text.
func TestACPRenderToolCallUpdateDiff(t *testing.T) {
	tests := []struct {
		name    string
		update  string
		want    []string
		notWant []string
	}{
		{
			name:   "diff blocks render an edited-paths summary",
			update: `{"sessionUpdate":"tool_call_update","toolCallId":"call-1","status":"completed","content":[{"type":"diff","path":"main.go","oldText":"package old","newText":"package new"},{"type":"diff","path":"created.go","newText":"package created"}]}`,
			want:   []string{"tool_call_update", "call-1 → completed", "edited main.go, created.go"},
			// The before/after text is never surfaced in the one-line marker.
			notWant: []string{"package old", "package new", "package created"},
		},
		{
			name:    "non-diff content leaves the marker unchanged",
			update:  `{"sessionUpdate":"tool_call_update","toolCallId":"call-2","status":"completed","content":[{"type":"content"}]}`,
			want:    []string{"tool_call_update", "call-2 → completed"},
			notWant: []string{"edited"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf strings.Builder
			r := newACPRenderer(&buf, false)
			params, _ := json.Marshal(map[string]json.RawMessage{"update": json.RawMessage(tt.update)})
			if err := r.render(params); err != nil {
				t.Fatalf("render: %v", err)
			}
			got := buf.String()
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("render output %q missing %q", got, w)
				}
			}
			for _, w := range tt.notWant {
				if strings.Contains(got, w) {
					t.Errorf("render output %q unexpectedly contains %q", got, w)
				}
			}
		})
	}
}
