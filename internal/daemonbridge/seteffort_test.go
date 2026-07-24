package daemonbridge_test

import (
	"context"
	"testing"

	"github.com/jedwards1230/gofer/internal/daemonbridge"
	"github.com/jedwards1230/gofer/internal/tui"
)

// TestSetEffort asserts SetEffort issues the gofer/set_effort Call with the
// right params: a subsequent Roster reflects the new level, which is only
// possible if sessionId/effort both reached the daemon, it applied them to the
// right session, and the level survived the roster wire DTO round trip in both
// directions (see internal/daemon/wire.go's setEffortParams + sessionInfoDTO).
// The compile-time `var _ tui.Supervisor = (*Supervisor)(nil)` assertion in
// bridge.go covers interface satisfaction; this covers the wire behavior.
func TestSetEffort(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	url := newTestDaemon(t, sup)
	b := newBridge(t, url)

	info, err := b.Create(context.Background(), "", tui.CreateOptions{Cwd: t.TempDir(), Model: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := b.SetEffort(context.Background(), info.ID, "high"); err != nil {
		t.Fatalf("SetEffort: %v", err)
	}
	if got := rosterEffort(t, b, info.ID); got != "high" {
		t.Errorf("roster Effort after SetEffort = %q, want high", got)
	}

	// The clear is the case gofer/set_model has no analogue for: an empty value
	// is a real request here, not a missing param, so it must round-trip as one.
	if err := b.SetEffort(context.Background(), info.ID, ""); err != nil {
		t.Fatalf("SetEffort(\"\") — the clear: %v", err)
	}
	if got := rosterEffort(t, b, info.ID); got != "" {
		t.Errorf("roster Effort after the clear = %q, want \"\"", got)
	}
}

// TestSetEffortUnknownLevel asserts a level outside the unified vocabulary
// comes back as a plain error through the bridge — the supervisor's typed
// ErrInvalidEffort does not survive the wire, but its message does.
func TestSetEffortUnknownLevel(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	url := newTestDaemon(t, sup)
	b := newBridge(t, url)

	info, err := b.Create(context.Background(), "", tui.CreateOptions{Cwd: t.TempDir(), Model: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := b.SetEffort(context.Background(), info.ID, "ultra"); err == nil {
		t.Fatal("SetEffort with an unknown level: want an error, got none")
	}
}

// TestSetEffortUnknownSession asserts SetEffort against a session id the
// daemon has never seen surfaces as a plain error through the bridge.
func TestSetEffortUnknownSession(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	url := newTestDaemon(t, sup)
	b := newBridge(t, url)

	if err := b.SetEffort(context.Background(), "does-not-exist", "low"); err == nil {
		t.Fatal("SetEffort on unknown session: want an error, got none")
	}
}

// rosterEffort reads sessionID's effort back off the bridge's own Roster — the
// full client-side path (gofer/roster → wirestream.SessionInfo →
// tui.SessionInfo), so a level dropped in ANY of those mappings fails here.
func rosterEffort(t *testing.T, b *daemonbridge.Supervisor, sessionID string) string {
	t.Helper()
	roster, err := b.Roster(context.Background())
	if err != nil {
		t.Fatalf("Roster: %v", err)
	}
	for _, e := range roster {
		if e.ID == sessionID {
			return e.Effort
		}
	}
	t.Fatalf("session %s missing from Roster: %+v", sessionID, roster)
	return ""
}
