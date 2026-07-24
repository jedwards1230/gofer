package router

import (
	"testing"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// TestStatusFromWireMapping locks the worker's roster status STRING → supervisor
// enum mapping the roster cache seed folds a worker's gofer/roster response
// through (see rostercache.go). "idle" is the reloaded/at-rest status a resumed
// worker reports for a session that has not been prompted since it came back live
// — it must map to supervisor.StatusIdle, not fall through to the
// StatusNeedsInput default, or reopening an offline row would land it back on the
// overview's awaiting-input counter after the seed.
func TestStatusFromWireMapping(t *testing.T) {
	cases := []struct {
		wire string
		want supervisor.SessionStatus
	}{
		{"working", supervisor.StatusWorking},
		{"finished", supervisor.StatusFinished},
		{"idle", supervisor.StatusIdle},
		{"needs-input", supervisor.StatusNeedsInput},
		{"unknown", supervisor.StatusNeedsInput},
		{"", supervisor.StatusNeedsInput},
	}
	for _, tc := range cases {
		t.Run(tc.wire, func(t *testing.T) {
			if got := statusFromWire(tc.wire); got != tc.want {
				t.Errorf("statusFromWire(%q) = %v, want %v", tc.wire, got, tc.want)
			}
		})
	}
}
