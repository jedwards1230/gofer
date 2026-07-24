package wirestream

import (
	"encoding/json"
	"testing"
)

// TestSessionInfoDecodesSubagentLink pins the client half of the roster row's
// subagent link. This type is a hand-maintained MIRROR of internal/daemon's
// unexported sessionInfoDTO (the daemon's type is unexported by design — it IS
// the wire contract), so nothing but a test holds the two in step: a tag typo
// here decodes silently to the zero values, which read as "a root session" and
// flatten every subagent tree that reaches a client through this decoder
// (internal/daemonbridge and the M6 router both).
//
// The raw JSON is written literally, exactly as the daemon emits it, rather than
// round-tripped from a Go value — a round trip through this same struct would
// pass with both sides misspelled identically.
func TestSessionInfoDecodesSubagentLink(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantParent string
		wantAgent  string
		wantDepth  int
	}{
		{
			name:       "child row carries the link",
			raw:        `{"id":"s2","status":"needs-input","live":true,"parentId":"s1","agent":"go-developer","depth":2}`,
			wantParent: "s1",
			wantAgent:  "go-developer",
			wantDepth:  2,
		},
		{
			// A root session omits all three (they are omitempty daemon-side), and
			// so does any daemon predating subagents — both must decode to "root".
			name: "root row omits all three",
			raw:  `{"id":"s1","status":"needs-input","live":true}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got SessionInfo
			if err := json.Unmarshal([]byte(tc.raw), &got); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.raw, err)
			}
			if got.ParentID != tc.wantParent || got.Agent != tc.wantAgent || got.Depth != tc.wantDepth {
				t.Errorf("decoded %s = {parent %q, agent %q, depth %d}, want {%q, %q, %d}",
					tc.raw, got.ParentID, got.Agent, got.Depth, tc.wantParent, tc.wantAgent, tc.wantDepth)
			}
		})
	}
}
