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
	"net/http/httptest"
	"strings"
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
			Supervisor:   sup,
			Session:      pinnedID,
			DefaultModel: "faux",
			Stdout:       io.Discard, // we learn the addr via Ready, not the stdout line
			Ready:        func(hs worker.Handshake) { ready <- hs },
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

	go func() {
		// The driving prompt Call is severed below (clientA.Close) before it can
		// return; its result is irrelevant — the turn survives on the worker.
		_, _ = clientA.Call(ctx, acp.MethodSessionPrompt, acp.PromptRequest{
			SessionID: pinnedID,
			Prompt:    []acp.ContentBlock{acp.TextBlock("rm -rf /")},
		})
	}()

	select {
	case <-askSeen:
	case <-time.After(10 * time.Second):
		t.Fatal("worker never emitted the permission ask (turn not blocked on its gate)")
	}
	fake := <-fakeCh // the gatedSession the worker built for this turn

	// SEVER the driving connection BEFORE adoption — the whole point of §7 is that
	// the gate lives in the worker, not in any client connection.
	_ = clientA.Close()

	// Assert the gate SURVIVES the severance: the worker's pump still holds
	// gatedSession.Prompt blocked on its gate, so no verdict has been delivered.
	// (A turn tied to the client connection would have unwound to a cancelled
	// resolution here.)
	select {
	case v := <-fake.verdicts:
		t.Fatalf("gate resolved with %v after the driving connection was severed; it should still be held", v)
	case <-time.After(300 * time.Millisecond):
		// Still blocked — the gate survived the disconnect, as §7 requires.
	}

	// THE RESTART: a fresh router adopts the still-blocked worker by scan.
	rtr, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router.New (adopting): %v", err)
	}
	t.Cleanup(func() { _ = rtr.Close() })

	h, ok := rtr.get(pinnedID)
	if !ok {
		t.Fatalf("adopting router did not adopt the blocked worker %s", pinnedID)
	}
	if h.cmd != nil {
		t.Errorf("adopted handle cmd = non-nil, want nil")
	}

	// Stand up the NEW router's OWN daemon (the router as its Supervisor) — a real
	// ACP-over-WebSocket surface a real client connects to, exactly as production.
	d := daemon.New(rtr, daemon.Config{
		DefaultModel:                     "faux",
		ReplayPendingPermissionsOnAttach: true,
	})
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	newRouterAddr := strings.TrimPrefix(srv.URL, "http://")

	// Client B is a REAL client of the NEW router's daemon. It attaches to the
	// adopted session via session/load (the live attach path — Resume returns the
	// live snapshot, so the peer joins the fan-out set) BEFORE the permission
	// relay is wired, so the standing watcher's live broadcast reaches it.
	clientB, err := daemon.Dial(ctx, newRouterAddr, "")
	if err != nil {
		t.Fatalf("dial new router daemon: %v", err)
	}
	t.Cleanup(func() { _ = clientB.Close() })
	askB := make(chan string, 4)
	go func() {
		for n := range clientB.Notifications() {
			if n.Method == "gofer/permission_requested" {
				var p struct {
					ID string `json:"id"`
				}
				_ = json.Unmarshal(n.Params, &p)
				askB <- p.ID
			}
		}
	}()
	if _, err := clientB.Call(ctx, acp.MethodSessionLoad, acp.LoadSessionRequest{SessionID: pinnedID, Cwd: t.TempDir()}); err != nil {
		t.Fatalf("clientB session/load (attach to adopted session): %v", err)
	}

	// Wire the permission relay (F1): this starts the adopted session's standing
	// watcher, which re-surfaces the open request into the daemon's route table +
	// pending map and broadcasts it to attached peers — reaching clientB LIVE.
	rtr.SetPermissionRelay(d)

	select {
	case id := <-askB:
		if id != callID {
			t.Errorf("clientB received re-surfaced ask for call %q, want %q", id, callID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("F1 gap: the re-surfaced permission never reached a real client of the new router's daemon")
	}

	// Answer through the DAEMON's handlePermissionReply — NOT router.Reply directly
	// — proving the standing watcher recorded the call→session route so the reply
	// resolves for an ADOPTED session.
	if err := clientB.Notify("permission.reply", map[string]any{"id": callID, "verdict": string(event.VerdictAllow)}); err != nil {
		t.Fatalf("clientB permission.reply: %v", err)
	}

	// The reply must reach the worker's gate: gatedSession.Prompt acts on the allow.
	select {
	case v := <-fake.verdicts:
		if v != event.VerdictAllow {
			t.Errorf("gate resolved with verdict %v, want allow", v)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reply routed through the new router's daemon never reached the worker's gate")
	}
}
