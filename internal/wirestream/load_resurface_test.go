package wirestream_test

// load_resurface_test.go covers the M6 §7 adoption re-surface at the wirestream
// tier: a Reconstructor that attaches to a worker via
// [wirestream.Reconstructor.Load] re-surfaces a still-OPEN permission request
// into its reconstructed broker. This is the piece a router relies on when it
// adopts a worker whose turn is blocked mid-approval after a router restart —
// the outstanding gate is live in-flight state, not journaled, so it reaches the
// newly attached reconstructor only if the worker re-emits it on the load, which
// the worker's [daemon.Config.ReplayPendingPermissionsOnAttach] does.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/loop"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/wirestream"
)

const resurfaceWait = 5 * time.Second

// blockingApprovalSession is a supervisor.Session whose Prompt emits a real
// event.PermissionRequested and then BLOCKS on the injected approver (the
// supervisor's per-session gate), exactly as a live turn holds its gate awaiting
// a decision. It never resolves on its own — the turn stays outstanding until
// the test's ctx is cancelled — which is precisely the "worker blocked
// mid-approval" state adoption must re-surface. It keeps no journal (Fold is
// nil; a live session's history reads come from Fold, not disk).
type blockingApprovalSession struct {
	id       string
	callID   string
	broker   *event.Broker
	approver loop.Approver
}

func newBlockingApprovalSession(id, callID string, approver loop.Approver) *blockingApprovalSession {
	return &blockingApprovalSession{
		id:       id,
		callID:   callID,
		broker:   event.NewBroker(event.WithReplay(64)),
		approver: approver,
	}
}

func (f *blockingApprovalSession) ID() string               { return f.id }
func (f *blockingApprovalSession) JournalPath() string      { return "" }
func (f *blockingApprovalSession) Fold() []provider.Message { return nil }
func (f *blockingApprovalSession) Events() *event.Subscription {
	return f.broker.Subscribe(event.FilterAll, 64)
}
func (f *blockingApprovalSession) EventsLive() *event.Subscription {
	return f.broker.SubscribeLive(event.FilterAll, 64)
}
func (f *blockingApprovalSession) Emit(e event.Event)       { f.broker.Publish(e) }
func (f *blockingApprovalSession) Cost() session.CostReport { return session.CostReport{} }
func (f *blockingApprovalSession) SetModel(string) error    { return nil }
func (f *blockingApprovalSession) SetEffort(string) error   { return nil }
func (f *blockingApprovalSession) Close() error             { f.broker.Close(); return nil }

func (f *blockingApprovalSession) Prompt(ctx context.Context, text string) error {
	f.broker.Publish(event.NewPermissionRequested(f.id, f.callID, "bash", map[string]any{"command": text}, []string{"rule: ask"}))
	// Block until the gate is answered (a Reply) or the turn is cancelled. On
	// cancel, resolve+terminate so a driving session/prompt returns rather than
	// hanging past the test.
	reply, err := f.approver.Await(ctx, f.callID)
	if err != nil {
		f.broker.Publish(event.NewPermissionResolved(f.id, f.callID, event.VerdictDeny, "cancelled"))
		f.broker.Publish(event.NewTurnFinished(f.id, "cancelled", provider.Usage{}))
		return err
	}
	f.broker.Publish(event.NewPermissionResolved(f.id, f.callID, reply.Verdict, "human"))
	f.broker.Publish(event.NewTurnFinished(f.id, "end_turn", provider.Usage{}))
	return nil
}

// newBlockedApprovalWorkerURL stands up an in-process daemon in WORKER mode
// (ReplayPendingPermissionsOnAttach set, like internal/worker's Serve) over a
// supervisor whose one session blocks mid-approval, and returns its ws:// URL.
func newBlockedApprovalWorkerURL(t *testing.T) string {
	t.Helper()
	const callID = "call-1"
	build := func(_ context.Context, opts runner.Options) (supervisor.Session, error) {
		id := "sess-blocked"
		return newBlockingApprovalSession(id, callID, opts.Approver), nil
	}
	sup, err := supervisor.New(supervisor.Config{
		Root:       t.TempDir(),
		NewSession: build,
		ResumeSession: func(ctx context.Context, _ string, opts runner.Options) (supervisor.Session, error) {
			return build(ctx, opts)
		},
	})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	d := daemon.New(sup, daemon.Config{
		DefaultModel:                     "faux",
		ReplayPendingPermissionsOnAttach: true,
	})
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	return "ws" + srv.URL[len("http"):]
}

// TestLoadResurfacesOpenPermission drives a turn to its gate on a worker-mode
// daemon (client A), then attaches a SECOND, fresh Reconstructor via Load
// (client B, the "adopting router") and asserts the still-open
// PermissionRequested re-surfaces into client B's reconstructed broker — the §7
// adoption guarantee at the wirestream tier.
func TestLoadResurfacesOpenPermission(t *testing.T) {
	url := newBlockedApprovalWorkerURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Client A creates the session and drives a turn that blocks on its gate.
	clientA, err := daemon.Dial(ctx, url, "")
	if err != nil {
		t.Fatalf("dial A: %v", err)
	}
	t.Cleanup(func() { _ = clientA.Close() })
	// A must drain notifications while a prompt streams (Client contract), and it
	// lets us confirm the ask went outstanding before we adopt.
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
	sid := created.SessionID

	// Drive the blocking turn in the background — session/prompt returns only
	// once the gate resolves (which this test never does; ctx cancel unwinds it).
	go func() {
		_, _ = clientA.Call(ctx, acp.MethodSessionPrompt, acp.PromptRequest{
			SessionID: sid,
			Prompt:    []acp.ContentBlock{acp.TextBlock("rm -rf /")},
		})
	}()

	select {
	case <-askSeen:
	case <-time.After(resurfaceWait):
		t.Fatal("worker never emitted the permission ask to the driving client")
	}

	// Client B is the adopting router: a fresh connection + Reconstructor that
	// attaches purely via Load — no prompt of its own — and must nonetheless see
	// the open request re-surfaced.
	clientB, err := daemon.Dial(ctx, url, "")
	if err != nil {
		t.Fatalf("dial B: %v", err)
	}
	rec := wirestream.New(clientB)
	t.Cleanup(func() { _ = rec.Close() })

	loadCtx, loadCancel := context.WithTimeout(ctx, resurfaceWait)
	if err := rec.Load(loadCtx, sid); err != nil {
		t.Fatalf("rec.Load: %v", err)
	}
	loadCancel()

	// Subscribe WITH replay: Load settled the re-surfaced request into the broker
	// (retained by WithReplay), and Subscribe replays that backlog to us.
	sub, err := rec.Subscribe(ctx, sid)
	if err != nil {
		t.Fatalf("rec.Subscribe: %v", err)
	}
	defer sub.Close()

	deadline := time.After(resurfaceWait)
	for {
		select {
		case ev, ok := <-sub.C:
			if !ok {
				t.Fatal("broker closed before the re-surfaced permission arrived")
			}
			if pr, isReq := ev.(event.PermissionRequested); isReq {
				if pr.SessionID() != sid {
					t.Errorf("re-surfaced request for wrong session: got %q want %q", pr.SessionID(), sid)
				}
				if pr.ID != "call-1" {
					t.Errorf("re-surfaced request call id = %q, want call-1", pr.ID)
				}
				return // re-surface observed
			}
		case <-deadline:
			t.Fatal("Load did not re-surface the open PermissionRequested into the reconstructed broker")
		}
	}
}
