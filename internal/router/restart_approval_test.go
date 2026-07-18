package router

// restart_approval_test.go is the M6 Phase-2 demo criterion (design §7): a
// session blocked mid-approval SURVIVES a router restart. A worker holds its
// gate (the turn blocked awaiting a decision); a fresh router adopts the worker
// by scan, the still-open PermissionRequested re-surfaces into the adopted
// handle's reconstructed broker, and a reply routed through the new router
// resolves the gate so the turn proceeds. Nothing is lost because the gate never
// left the worker and the backlog never left the worker's broker.

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/loop"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/worker"
)

// gatedSession is a supervisor.Session whose Prompt emits a real
// PermissionRequested and blocks on the injected gate (opts.Approver) until a
// reply arrives — the live "blocked mid-approval" state adoption must survive.
// The verdict it acts on is published to verdicts for the test to assert the
// reply routed all the way through to the worker's gate.
type gatedSession struct {
	id       string
	callID   string
	broker   *event.Broker
	approver loop.Approver
	verdicts chan event.Verdict
}

func newGatedSession(id, callID string, approver loop.Approver) *gatedSession {
	return &gatedSession{
		id:       id,
		callID:   callID,
		broker:   event.NewBroker(event.WithReplay(64)),
		approver: approver,
		verdicts: make(chan event.Verdict, 1),
	}
}

func (f *gatedSession) ID() string                  { return f.id }
func (f *gatedSession) JournalPath() string         { return "" }
func (f *gatedSession) Fold() []provider.Message    { return nil }
func (f *gatedSession) Events() *event.Subscription { return f.broker.Subscribe(event.FilterAll, 64) }
func (f *gatedSession) EventsLive() *event.Subscription {
	return f.broker.SubscribeLive(event.FilterAll, 64)
}
func (f *gatedSession) Emit(e event.Event)       { f.broker.Publish(e) }
func (f *gatedSession) Cost() session.CostReport { return session.CostReport{} }
func (f *gatedSession) SetModel(string) error    { return nil }
func (f *gatedSession) Close() error             { f.broker.Close(); return nil }

func (f *gatedSession) Prompt(ctx context.Context, text string) error {
	f.broker.Publish(event.NewPermissionRequested(f.id, f.callID, "bash", map[string]any{"command": text}, []string{"rule: ask"}))
	reply, err := f.approver.Await(ctx, f.callID)
	if err != nil {
		f.broker.Publish(event.NewPermissionResolved(f.id, f.callID, event.VerdictDeny, "cancelled"))
		f.broker.Publish(event.NewTurnFinished(f.id, "cancelled", provider.Usage{}))
		return err
	}
	f.broker.Publish(event.NewPermissionResolved(f.id, f.callID, reply.Verdict, "human"))
	f.verdicts <- reply.Verdict
	f.broker.Publish(event.NewTurnFinished(f.id, "end_turn", provider.Usage{}))
	return nil
}

// TestRestartMidApprovalSurvives runs the full §7 round trip in-process: an
// in-process worker holds a session blocked on its gate; a fresh router adopts
// it; the open request re-surfaces; a reply through the router resolves the gate.
func TestRestartMidApprovalSurvives(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()
	pinnedID := uuid.Must(uuid.NewV7()).String()
	const callID = "call-1"

	// A gatedSession bound to the router-pinned id; its gate is the supervisor's
	// injected Approver, which a router-routed reply resolves.
	fakeCh := make(chan *gatedSession, 1)
	build := func(_ context.Context, opts runner.Options) (supervisor.Session, error) {
		f := newGatedSession(pinnedID, callID, opts.Approver)
		fakeCh <- f
		return f, nil
	}
	sup, err := supervisor.New(supervisor.Config{
		Root:       root,
		NewSession: build,
		ResumeSession: func(ctx context.Context, _ string, opts runner.Options) (supervisor.Session, error) {
			return build(ctx, opts)
		},
	})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}

	// Run the worker in-process (its pid is this test process — alive — so the
	// adoption scan's liveness probe passes). Cancelling workerCtx shuts it down.
	workerCtx, stopWorker := context.WithCancel(context.Background())
	ready := make(chan worker.Handshake, 1)
	workerErr := make(chan error, 1)
	go func() {
		workerErr <- worker.Serve(workerCtx, worker.Options{
			Supervisor: sup,
			Session:    pinnedID,
			Stdout:     io.Discard, // we learn the addr via Ready, not the stdout line
			Ready:      func(hs worker.Handshake) { ready <- hs },
		})
	}()
	t.Cleanup(func() {
		stopWorker()
		select {
		case <-workerErr:
		case <-time.After(5 * time.Second):
		}
	})

	var hs worker.Handshake
	select {
	case hs = <-ready:
	case err := <-workerErr:
		t.Fatalf("worker exited before ready: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("worker never became ready")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Client A drives the turn to its gate over the worker socket.
	clientA, err := daemon.Dial(ctx, hs.Addr, "")
	if err != nil {
		t.Fatalf("dial worker: %v", err)
	}
	t.Cleanup(func() { _ = clientA.Close() })
	askSeen := make(chan struct{}, 1)
	go func() {
		for n := range clientA.Notifications() {
			if n.Method == "gofer/permission_requested" {
				select {
				case askSeen <- struct{}{}:
				default:
				}
			}
		}
	}()

	raw, err := clientA.Call(ctx, acp.MethodSessionNew, acp.NewSessionRequest{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}
	var created acp.NewSessionResponse
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode session/new: %v", err)
	}
	if created.SessionID != pinnedID {
		t.Fatalf("worker session id %q != pinned %q", created.SessionID, pinnedID)
	}

	promptDone := make(chan error, 1)
	go func() {
		_, perr := clientA.Call(ctx, acp.MethodSessionPrompt, acp.PromptRequest{
			SessionID: pinnedID,
			Prompt:    []acp.ContentBlock{acp.TextBlock("rm -rf /")},
		})
		promptDone <- perr
	}()

	select {
	case <-askSeen:
	case <-time.After(10 * time.Second):
		t.Fatal("worker never emitted the permission ask (turn not blocked on its gate)")
	}

	// THE RESTART: a fresh router adopts the still-blocked worker by scan.
	router, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router.New (adopting): %v", err)
	}
	t.Cleanup(func() { _ = router.Close() })

	h, ok := router.get(pinnedID)
	if !ok {
		t.Fatalf("adopting router did not adopt the blocked worker %s", pinnedID)
	}
	if h.cmd != nil {
		t.Errorf("adopted handle cmd = non-nil, want nil")
	}

	// The open request re-surfaces into the adopted handle's reconstructed broker
	// (Load settled it; Subscribe replays the retained backlog).
	sub, err := h.rec.Subscribe(ctx, pinnedID)
	if err != nil {
		t.Fatalf("subscribe adopted rec: %v", err)
	}
	defer sub.Close()

	if !waitForResurfacedAsk(t, sub, pinnedID, callID) {
		t.Fatal("open PermissionRequested did not re-surface into the adopted broker")
	}

	// Reply through the NEW router — it forwards permission.reply to the worker,
	// whose gate resolves and unblocks the turn.
	if err := router.Reply(pinnedID, event.PermissionReply{ID: callID, Verdict: event.VerdictAllow}); err != nil {
		t.Fatalf("router.Reply: %v", err)
	}

	// The worker's gate must have acted on the allow, and the driving turn returns.
	fake := <-fakeCh
	select {
	case v := <-fake.verdicts:
		if v != event.VerdictAllow {
			t.Errorf("gate resolved with verdict %v, want allow", v)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reply routed through the new router never reached the worker's gate")
	}
	select {
	case err := <-promptDone:
		if err != nil {
			t.Fatalf("driving session/prompt returned error after reply: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("driving session/prompt did not return after the gate resolved")
	}
}

// waitForResurfacedAsk drains sub until it observes the re-surfaced
// PermissionRequested for sessionID/callID, or fails after a deadline.
func waitForResurfacedAsk(t *testing.T, sub *event.Subscription, sessionID, callID string) bool {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-sub.C:
			if !ok {
				return false
			}
			if pr, isReq := ev.(event.PermissionRequested); isReq && pr.SessionID() == sessionID && pr.ID == callID {
				return true
			}
		case <-deadline:
			return false
		}
	}
}
