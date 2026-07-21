package daemonbridge_test

// explain_test.go is the cross-hop proof for ctrl+e: a REAL gated call (a
// guarded supervisor behind a real daemon), explained through
// Supervisor.ExplainPermission over the wire, and then answered — because the
// whole point of the method is that asking why does not cost you the ability
// to answer. The daemon-side handler has its own unit coverage
// (internal/daemon/explain_test.go); what this file adds is the wire round
// trip and the second hop.

import (
	"context"
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/daemonbridge"
	"github.com/jedwards1230/gofer/internal/tui"
)

// pendingGatedCall drives b's session to a real pending permission request —
// the same opening TestPermissionRelayEndToEnd documents event by event — and
// returns the session id, the live subscription, and the request itself.
func pendingGatedCall(t *testing.T, b *daemonbridge.Supervisor) (sessionID string, sub *event.Subscription, pr event.PermissionRequested) {
	t.Helper()

	info, err := b.Create(context.Background(), "", tui.CreateOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sub, err = b.Subscribe(context.Background(), info.ID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Close)

	if err := b.Send(context.Background(), info.ID, "read a.txt"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	before := drainFirstTurnEvents(t, sub, "read a.txt", 6)
	pr, ok := before[5].(event.PermissionRequested)
	if !ok {
		t.Fatalf("event 5 = %+v, want PermissionRequested", before[5])
	}
	return info.ID, sub, pr
}

// assertExplainThenReplyStillResolves is the shared body of the single- and
// double-hop cases: explain the pending call over the wire, check the answer
// describes THIS gating decision, then reply and require the resolution to
// still arrive. An explain that resolved (or dropped) the request would leave
// the reply with nothing to answer and this would hang out to its deadline.
func assertExplainThenReplyStillResolves(t *testing.T, b *daemonbridge.Supervisor, hop string) {
	t.Helper()

	sessionID, sub, pr := pendingGatedCall(t, b)

	rationale, err := b.ExplainPermission(context.Background(), sessionID, pr.ID)
	if err != nil {
		t.Fatalf("ExplainPermission (%s): %v", hop, err)
	}
	if rationale.Reason == "" {
		t.Errorf("rationale (%s) carried no reason: %+v", hop, rationale)
	}
	if !strings.Contains(rationale.Reason, pr.Tool) {
		t.Errorf("rationale reason %q (%s) does not name the gated tool %q", rationale.Reason, hop, pr.Tool)
	}
	if rationale.Policy == "" {
		t.Errorf("rationale (%s) carried no policy label; the guard reported trace %v", hop, pr.Trace)
	}
	// The guard's own trace survives the hop(s) verbatim — the rationale must
	// not be a summary that quietly drops what the guard actually said.
	if len(rationale.Trace) != len(pr.Trace) {
		t.Errorf("rationale trace (%s) = %v, want the request's own %v", hop, rationale.Trace, pr.Trace)
	}

	// Explaining twice is still read-only, and the request is still answerable.
	if _, err := b.ExplainPermission(context.Background(), sessionID, pr.ID); err != nil {
		t.Fatalf("second ExplainPermission (%s): %v", hop, err)
	}

	if err := b.Reply(context.Background(), sessionID, pr.ID, true, false); err != nil {
		t.Fatalf("Reply (%s): %v", hop, err)
	}
	after := drainEvents(t, sub, 1)
	resolved, ok := after[0].(event.PermissionResolved)
	if !ok {
		t.Fatalf("post-reply event (%s) = %+v, want PermissionResolved — the explain cost the human their answer", hop, after[0])
	}
	if resolved.ID != pr.ID || resolved.Verdict != event.VerdictAllow {
		t.Errorf("PermissionResolved (%s) = %+v, want ID=%q Verdict=allow", hop, resolved, pr.ID)
	}
}

// TestExplainPermissionEndToEnd is the single-hop case: client bridge → daemon
// → guarded supervisor.
func TestExplainPermissionEndToEnd(t *testing.T) {
	sup := newTestSupervisorGuarded(t, func() provider.Provider { return &toolTurnProvider{} }, true)
	b := newBridge(t, newTestDaemon(t, sup))
	assertExplainThenReplyStillResolves(t, b, "single hop")
}

// TestDoubleHopExplainPermission is the same thing across TWO daemon hops
// (client → router → worker), in doublehop_test.go's prototype style.
//
// It needs no forwarding at the router: the router daemon's own prompt handler
// observes the reconstructed permission stream and retains the request exactly
// as a single daemon does (see internal/daemon's handleSessionPrompt, and
// permission_relay.go for the production router's equivalent), so the explain
// is answered by the hop the client is actually talking to. That is why the
// handler reads the daemon's own pendingPerms instead of asking the hosted
// supervisor — [daemon.Supervisor] gains no method, and this prototype's
// routerSupervisor (which implements that interface) needed no change at all.
func TestDoubleHopExplainPermission(t *testing.T) {
	sup := newTestSupervisorGuarded(t, func() provider.Provider { return &toolTurnProvider{} }, true)
	workerURL := newTestDaemon(t, sup)
	b := newBridge(t, newRouterDaemon(t, workerURL))
	assertExplainThenReplyStillResolves(t, b, "double hop")
}

// TestExplainPermissionUnknownCallSurfacesError pins the other half of the
// contract at the client edge: a call id nothing is pending for comes back as
// an ERROR, not an empty rationale a TUI would render as "gated for no stated
// reason".
func TestExplainPermissionUnknownCallSurfacesError(t *testing.T) {
	sup := newTestSupervisorGuarded(t, func() provider.Provider { return &toolTurnProvider{} }, true)
	b := newBridge(t, newTestDaemon(t, sup))

	sessionID, _, pr := pendingGatedCall(t, b)

	if _, err := b.ExplainPermission(context.Background(), sessionID, "no-such-call"); err == nil {
		t.Error("ExplainPermission(unknown call id): want an error, got a rationale")
	}
	// ...and the real one still explains, so the failure above was about the
	// id and not about the pipeline being broken.
	if _, err := b.ExplainPermission(context.Background(), sessionID, pr.ID); err != nil {
		t.Errorf("ExplainPermission(live call id): %v", err)
	}
}
