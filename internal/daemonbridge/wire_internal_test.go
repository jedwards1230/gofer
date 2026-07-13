package daemonbridge

import (
	"testing"

	"github.com/jedwards1230/gofer/internal/tui"
)

// TestStatusFromWireMapping locks the daemon's roster status STRING → TUI
// enum mapping for all three states plus an unrecognized value. It is an
// internal (package daemonbridge) test so it can call statusFromWire
// directly: the live M2 daemon never actually emits "finished"
// (supervisor.SessionStatus's doc: reserved, never emitted in M2), so a
// round trip through a real daemon (see bridge_test.go's
// TestRosterReflectsCreatedSession, which covers "needs-input" end to end)
// can never exercise that branch or the unrecognized-value fallback.
func TestStatusFromWireMapping(t *testing.T) {
	cases := []struct {
		wire string
		want tui.SessionStatus
	}{
		{"working", tui.StatusWorking},
		{"needs-input", tui.StatusNeedsInput},
		{"finished", tui.StatusFinished},
		{"unknown", tui.StatusNeedsInput},
		{"", tui.StatusNeedsInput},
	}
	for _, tc := range cases {
		t.Run(tc.wire, func(t *testing.T) {
			if got := statusFromWire(tc.wire); got != tc.want {
				t.Errorf("statusFromWire(%q) = %v, want %v", tc.wire, got, tc.want)
			}
		})
	}
}
