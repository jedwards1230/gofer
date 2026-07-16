package daemonbridge_test

import (
	"context"
	"testing"

	"github.com/jedwards1230/gofer/internal/tui"
)

// TestSetModel asserts SetModel issues the gofer/set_model Call with the
// right params: a subsequent Roster reflects the new model, which is only
// possible if sessionId/model both reached the daemon and it applied them to
// the right session (see internal/daemon/wire.go's decodeSetModelParams and
// handleGoferSetModel). The compile-time
// `var _ tui.Supervisor = (*Supervisor)(nil)` assertion in bridge.go covers
// interface satisfaction; this covers the wire behavior.
func TestSetModel(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	url := newTestDaemon(t, sup)
	b := newBridge(t, url)

	info, err := b.Create(context.Background(), "", tui.CreateOptions{Cwd: t.TempDir(), Model: "claude-sonnet-5"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := b.SetModel(context.Background(), info.ID, "claude-opus-4-8"); err != nil {
		t.Fatalf("SetModel: %v", err)
	}

	roster, err := b.Roster(context.Background())
	if err != nil {
		t.Fatalf("Roster: %v", err)
	}
	var found bool
	for _, e := range roster {
		if e.ID != info.ID {
			continue
		}
		found = true
		if e.Model != "claude-opus-4-8" {
			t.Errorf("roster Model after SetModel = %q, want claude-opus-4-8", e.Model)
		}
	}
	if !found {
		t.Fatalf("session %s missing from Roster: %+v", info.ID, roster)
	}
}

// TestSetModelUnknownSession asserts SetModel against a session id the
// daemon has never seen surfaces as a plain error through the bridge.
func TestSetModelUnknownSession(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	url := newTestDaemon(t, sup)
	b := newBridge(t, url)

	if err := b.SetModel(context.Background(), "does-not-exist", "claude-opus-4-8"); err == nil {
		t.Fatal("SetModel on unknown session: want an error, got none")
	}
}
