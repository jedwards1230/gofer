package daemonbridge_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/daemonbridge"
	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/tui"
)

// This file is the M6 Slice 0 de-risk prototype (see
// docs/milestones/M6-process-isolation.md §11). It proves the milestone's
// load-bearing premise with ZERO new wire code: a session's typed event stream
// and its permission round-trip survive TWO daemon-wire hops
// (client → router → worker), reconstructed by the EXISTING daemonbridge at
// each hop. It stands up one daemon as the "worker" (a real supervisor over a
// scripted provider) and a second as the "router" whose hosted [daemon.Supervisor]
// is [routerSupervisor] — a proxy pointed at the worker over the daemon wire.
// A client dials the router and sees the same reconstructed stream it would
// from a single daemon.
//
// It is deliberately a prototype: [routerSupervisor] wraps the tui-shaped
// [daemonbridge.Supervisor] and implements only what the two double-hop paths
// exercise (create, subscribe, prompt, permission reply, roster). The
// production router-side supervisor M6 Slice 1 builds talks the daemon wire
// directly and produces supervisor-shaped snapshots without the tui detour —
// the findings this prototype surfaced are recorded on the milestone PR.

// errRouterPrototype marks a [routerSupervisor] method the double-hop paths do
// not exercise. Reaching one means a test drove a surface this prototype never
// claimed to cover (the production Slice 1 supervisor covers them all).
var errRouterPrototype = errors.New("daemonbridge_test: router prototype does not implement this method")

// routerSupervisor is a [daemon.Supervisor] backed by a [daemonbridge.Supervisor]
// connected to another daemon (the "worker"). It is the prototype stand-in for
// M6's router-side remote supervisor: the methods a create+prompt+permission
// turn drives forward to the worker over the wire, translating the bridge's
// tui-shaped returns back to the supervisor-shaped ones the daemon's handlers
// expect.
type routerSupervisor struct{ b *daemonbridge.Supervisor }

var _ daemon.Supervisor = (*routerSupervisor)(nil)

func (r *routerSupervisor) Create(ctx context.Context, prompt string, opts supervisor.CreateOptions) (supervisor.SessionInfo, error) {
	info, err := r.b.Create(ctx, prompt, tui.CreateOptions{Cwd: opts.Cwd, Model: opts.Model})
	if err != nil {
		return supervisor.SessionInfo{}, err
	}
	// handleSessionNew reads only ID; the rest is carried for completeness.
	return supervisor.SessionInfo{ID: info.ID, Model: info.Model, Cwd: info.Cwd, Live: true}, nil
}

func (r *routerSupervisor) Send(ctx context.Context, sessionID, prompt string) error {
	return r.b.Send(ctx, sessionID, prompt)
}

// SubscribeLive maps to the bridge's Subscribe. The bridge's reconstructed
// broker retains a must-deliver replay, so this is not strictly the
// no-backlog stream SubscribeLive names — acceptable here (each test subscribes
// before the first turn, so the replay is empty), and a finding for the
// production supervisor, which should map SubscribeLive to a no-replay subscribe.
func (r *routerSupervisor) SubscribeLive(ctx context.Context, sessionID string) (*event.Subscription, error) {
	return r.b.Subscribe(ctx, sessionID)
}

func (r *routerSupervisor) Reply(sessionID string, op event.PermissionReply) error {
	return r.b.Reply(context.Background(), sessionID, op.ID, op.Verdict == event.VerdictAllow, op.Remember)
}

func (r *routerSupervisor) Interrupt(ctx context.Context, sessionID string) error {
	return r.b.Interrupt(ctx, sessionID)
}

func (r *routerSupervisor) SetModel(ctx context.Context, sessionID, model string) error {
	return r.b.SetModel(ctx, sessionID, model)
}

func (r *routerSupervisor) Kill(ctx context.Context, sessionID string) error {
	return r.b.Kill(ctx, sessionID)
}

func (r *routerSupervisor) Archive(ctx context.Context, sessionID string) error {
	return r.b.Archive(ctx, sessionID)
}

func (r *routerSupervisor) Roster(ctx context.Context) ([]supervisor.SessionInfo, error) {
	rows, err := r.b.Roster(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]supervisor.SessionInfo, len(rows))
	for i, row := range rows {
		out[i] = supervisor.SessionInfo{
			ID:      row.ID,
			Title:   row.Title,
			Status:  supervisorStatus(row.Status),
			Model:   row.Model,
			Cost:    row.Cost,
			Usage:   row.Usage,
			Pending: row.Pending,
			Cwd:     row.Cwd,
			Created: row.Created,
			Updated: row.Updated,
			Live:    true,
		}
	}
	return out, nil
}

// List has no disk-view over the bridge (the bridge only sees the worker's live
// roster); the prototype returns the same live set Roster does.
func (r *routerSupervisor) List(ctx context.Context) ([]supervisor.SessionInfo, error) {
	return r.Roster(ctx)
}

func (r *routerSupervisor) Resume(context.Context, string, supervisor.ResumeOptions) (supervisor.SessionInfo, error) {
	return supervisor.SessionInfo{}, errRouterPrototype
}

func (r *routerSupervisor) History(context.Context, string) ([]provider.Message, error) {
	return nil, errRouterPrototype
}

func (r *routerSupervisor) EmitConfigOptions(string, []event.ConfigOption) error {
	return errRouterPrototype
}

// supervisorStatus is the reverse of daemonbridge's statusFromWire, mapping the
// tui status enum back to the supervisor one for the prototype's roster rows.
func supervisorStatus(s tui.SessionStatus) supervisor.SessionStatus {
	switch s {
	case tui.StatusWorking:
		return supervisor.StatusWorking
	case tui.StatusFinished:
		return supervisor.StatusFinished
	default:
		return supervisor.StatusNeedsInput
	}
}

// newRouterDaemon dials the worker at workerURL, wraps it in a router-side
// proxy supervisor, and fronts that with a second daemon over its own
// httptest server — returning the router's ws:// URL. Both the bridge to the
// worker and the router's server are torn down on cleanup.
func newRouterDaemon(t *testing.T, workerURL string) string {
	t.Helper()
	c, err := daemon.Dial(context.Background(), workerURL, "")
	if err != nil {
		t.Fatalf("daemon.Dial (router→worker): %v", err)
	}
	b := daemonbridge.New(c)
	t.Cleanup(func() { _ = b.Close() })

	rd := daemon.New(&routerSupervisor{b: b}, daemon.Config{DefaultModel: "faux"})
	srv := httptest.NewServer(rd.Handler())
	t.Cleanup(srv.Close)
	return "ws" + srv.URL[len("http"):]
}

// TestDoubleHopTranscriptFidelity drives a full turn through TWO hops
// (client bridge → router daemon → worker daemon → real session) and asserts
// the client sees the exact same reconstructed transcript a single-hop bridge
// sees in TestSendReconstructsTranscript — proving the event stream survives a
// second serialize→reconstruct cycle byte-for-byte (kind and content).
func TestDoubleHopTranscriptFidelity(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	workerURL := newTestDaemon(t, sup)
	routerURL := newRouterDaemon(t, workerURL)
	b := newBridge(t, routerURL)

	info, err := b.Create(context.Background(), "", tui.CreateOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sub, err := b.Subscribe(context.Background(), info.ID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	if err := b.Send(context.Background(), info.ID, "hi"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Identical to TestSendReconstructsTranscript's single-hop expectation.
	events := drainFirstTurnEvents(t, sub, "hi", 13)
	wantKinds := []string{
		"message.started", "message.finished",
		"turn.started",
		"message.started", "message.delta", "message.delta", "message.finished",
		"message.started", "message.delta", "message.delta", "message.delta", "message.finished",
		"turn.finished",
	}
	for i, want := range wantKinds {
		if got := events[i].Kind(); got != want {
			t.Errorf("event %d: Kind() = %q, want %q", i, got, want)
		}
	}
	if fin, ok := events[6].(event.MessageFinished); !ok || fin.Content != "The user said hello. I'll greet them back." {
		t.Errorf("event 6 (reasoning finished) = %+v, want the joined reasoning chunks", events[6])
	}
	if fin, ok := events[11].(event.MessageFinished); !ok || fin.Content != "Hello! How can I help you today?" {
		t.Errorf("event 11 (text finished) = %+v, want the joined text chunks", events[11])
	}
	tf, ok := events[12].(event.TurnFinished)
	if !ok {
		t.Fatalf("event 12 = %+v, want TurnFinished", events[12])
	}
	if tf.StopReason != "end_turn" {
		t.Errorf("TurnFinished.StopReason = %q, want end_turn", tf.StopReason)
	}
}

// TestDoubleHopPermissionRoundTrip is TestPermissionRelayEndToEnd across two
// hops: the worker's guarded supervisor asks, the permission request is fanned
// out worker → router → client, the client answers through the same bridge
// method the TUI's approval dialog calls, and the reply routes back
// client → router → worker to resolve the gate. It proves the whole approvals
// pipeline survives the double hop — the milestone's other load-bearing claim.
func TestDoubleHopPermissionRoundTrip(t *testing.T) {
	sup := newTestSupervisorGuarded(t, func() provider.Provider { return &toolTurnProvider{} }, true)
	workerURL := newTestDaemon(t, sup)
	routerURL := newRouterDaemon(t, workerURL)
	b := newBridge(t, routerURL)

	info, err := b.Create(context.Background(), "", tui.CreateOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sub, err := b.Subscribe(context.Background(), info.ID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	if err := b.Send(context.Background(), info.ID, "read a.txt"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// message.started(user), message.finished(user), turn.started,
	// tool.call.started, turn.finished(tool_use), permission.requested — after
	// the leading session.info title event, exactly as TestPermissionRelayEndToEnd.
	before := drainFirstTurnEvents(t, sub, "read a.txt", 6)
	if _, ok := before[3].(event.ToolCallStarted); !ok {
		t.Fatalf("event 3 = %+v, want ToolCallStarted", before[3])
	}
	if tf, ok := before[4].(event.TurnFinished); !ok || tf.StopReason != "tool_use" {
		t.Fatalf("event 4 = %+v, want TurnFinished(stop_reason=tool_use)", before[4])
	}
	pr, ok := before[5].(event.PermissionRequested)
	if !ok {
		t.Fatalf("event 5 = %+v, want PermissionRequested", before[5])
	}
	if pr.Tool != "read_file" {
		t.Errorf("PermissionRequested.Tool = %q, want %q", pr.Tool, "read_file")
	}

	if err := b.Reply(context.Background(), info.ID, pr.ID, true, false); err != nil {
		t.Fatalf("Reply: %v", err)
	}

	// permission.resolved, tool.call.finished, turn.started, message.started(text),
	// message.delta, message.finished(text), turn.finished(end_turn) = 7 events.
	after := drainEvents(t, sub, 7)
	resolved, ok := after[0].(event.PermissionResolved)
	if !ok {
		t.Fatalf("event 0 (post-reply) = %+v, want PermissionResolved", after[0])
	}
	if resolved.ID != pr.ID || resolved.Verdict != event.VerdictAllow {
		t.Errorf("PermissionResolved = %+v, want ID=%q Verdict=allow", resolved, pr.ID)
	}
	if _, ok := after[1].(event.ToolCallFinished); !ok {
		t.Fatalf("event 1 (post-reply) = %+v, want ToolCallFinished", after[1])
	}
	tf, ok := after[6].(event.TurnFinished)
	if !ok {
		t.Fatalf("event 6 (post-reply) = %+v, want TurnFinished", after[6])
	}
	if tf.StopReason != "end_turn" {
		t.Errorf("TurnFinished.StopReason = %q, want end_turn", tf.StopReason)
	}
}

// TestDoubleHopFidelityToolCallDeltaAndSpill is TestFidelityToolCallDeltaAndSpill
// across two hops — the strongest fidelity proof M6 Slice 0 makes. It drives one
// turn whose SOURCE stream (subscribed directly off the worker's
// *supervisor.Supervisor) carries a tool.call.delta (the streaming INPUT
// fragment ACP's session/update drops) AND a tool.call.finished bearing
// Diagnostics + all three Spill* fields (also dropped by the ACP projection),
// then asserts the CLIENT stream — reconstructed over client → router → worker,
// two full serialize→reconstruct cycles — is equal to the source event-by-event,
// kind AND every payload field (ignoring only the locally-reassigned seq/time).
// If the second hop lost these gofer/event-only fields, this fails; it passes,
// so the additive event envelope survives the double hop verbatim.
func TestDoubleHopFidelityToolCallDeltaAndSpill(t *testing.T) {
	tools := oneToolRegistry{name: "diag_tool", tool: diagnosticSpillTool{}}
	sup := newTestSupervisorWithTools(t, func() provider.Provider { return &deltaToolTurnProvider{} }, tools)
	workerURL := newTestDaemon(t, sup)
	routerURL := newRouterDaemon(t, workerURL)
	b := newBridge(t, routerURL)

	ctx := context.Background()
	info, err := b.Create(ctx, "", tui.CreateOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Source: straight off the worker's supervisor (zero hops). Client: over
	// both hops via the router. Subscribe to both before driving the turn.
	sourceSub, err := sup.SubscribeLive(ctx, info.ID)
	if err != nil {
		t.Fatalf("sup.SubscribeLive: %v", err)
	}
	defer sourceSub.Close()

	clientSub, err := b.Subscribe(ctx, info.ID)
	if err != nil {
		t.Fatalf("b.Subscribe: %v", err)
	}
	defer clientSub.Close()

	if err := b.Send(ctx, info.ID, "read a.txt"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Same 17-event turn as TestFidelityToolCallDeltaAndSpill (which documents
	// the exact sequence).
	const wantEvents = 17
	sourceEvents := drainEvents(t, sourceSub, wantEvents)
	clientEvents := drainEvents(t, clientSub, wantEvents)

	var sawDelta, sawSpilledFinished bool
	for i, se := range sourceEvents {
		ce := clientEvents[i]
		if se.Kind() != ce.Kind() {
			t.Errorf("event %d: twice-reconstructed Kind() = %q, want %q", i, ce.Kind(), se.Kind())
			continue
		}
		if se.SessionID() != ce.SessionID() {
			t.Errorf("event %d (%s): twice-reconstructed SessionID() = %q, want %q", i, se.Kind(), ce.SessionID(), se.SessionID())
		}
		wantFields := stripEnvelopeSeqTime(t, se)
		gotFields := stripEnvelopeSeqTime(t, ce)
		if !reflect.DeepEqual(gotFields, wantFields) {
			t.Errorf("event %d (%s): twice-reconstructed payload = %+v, want %+v", i, se.Kind(), gotFields, wantFields)
		}

		switch ev := se.(type) {
		case event.ToolCallDelta:
			sawDelta = true
		case event.ToolCallFinished:
			if ev.SpillPath != "" && ev.SpillBytes > 0 && ev.SpillSHA256 != "" && len(ev.Diagnostics) > 0 {
				sawSpilledFinished = true
			}
		}
	}

	if !sawDelta {
		t.Error("source stream never contained a tool.call.delta — test setup bug, not a fidelity failure")
	}
	if !sawSpilledFinished {
		t.Error("source stream never contained a spilled+diagnostic tool.call.finished — test setup bug, not a fidelity failure")
	}
}
