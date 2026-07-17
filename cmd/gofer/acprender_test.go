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
