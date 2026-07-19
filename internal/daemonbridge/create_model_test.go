package daemonbridge_test

// create_model_test.go covers issue #162's defect 2: the model a
// daemon-created session actually runs must come from the session/new
// RESPONSE, never from the request the client happened to send. The whole
// point is the NORMAL path, where the client sends no model at all and lets
// the daemon resolve its own default — the case where echoing the request
// yields the empty string.
//
// Driven against the real in-process daemon (newTestDaemon, DefaultModel
// "faux") over a real WebSocket, so it pins the wire contract end to end
// rather than a hand-rolled response fixture. No network leaves the process.

import (
	"context"
	"testing"

	"github.com/jedwards1230/gofer/internal/tui"
)

// TestCreateReportsTheAssignedModelNotTheRequest is the regression gate for
// the echo bug. The bridge sends CreateOptions with NO model — exactly what
// the TUI's doCreate does — so opts.Model is "". Before the `_meta` extension,
// SessionInfo.Model was set from that empty request value and the freshly
// created roster row showed no model at all, which is a large part of why a
// daemon-attached TUI looked like it had ignored a /model change.
//
// Reverting internal/daemonbridge's assignedModel to `return requested` (the
// old `Model: opts.Model`) fails this test.
func TestCreateReportsTheAssignedModelNotTheRequest(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	url := newTestDaemon(t, sup) // daemon.Config{DefaultModel: "faux"}
	b := newBridge(t, url)

	info, err := b.Create(context.Background(), "", tui.CreateOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// "faux" is the DAEMON's default, resolved daemon-side. The client never
	// named it, so the only way it can reach this row is off the response.
	if info.Model != "faux" {
		t.Fatalf("Create with no requested model: Model = %q, want the daemon-assigned %q "+
			"(an empty value means the row is echoing the request instead of reading the response)", info.Model, "faux")
	}
}

// TestCreateReportsTheAssignedModelWhenRequested is the twin: when the client
// DOES name a model, the daemon honors it, so the response carries that same
// id back. This case passed even with the echo bug — which is exactly why it
// could not catch it, and why the no-model case above is the real gate. Kept
// so a future change that starts ignoring a requested model is still caught.
func TestCreateReportsTheAssignedModelWhenRequested(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	url := newTestDaemon(t, sup)
	b := newBridge(t, url)

	info, err := b.Create(context.Background(), "", tui.CreateOptions{Cwd: t.TempDir(), Model: "faux"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if info.Model != "faux" {
		t.Fatalf("Create with a requested model: Model = %q, want %q", info.Model, "faux")
	}
}
