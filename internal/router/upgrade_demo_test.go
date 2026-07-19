package router

// upgrade_demo_test.go is M6's milestone criterion (design §11), end to end:
//
//	upgrade the daemon MID-TURN
//	  -> the old worker FINISHES its mid-flight turn, on the OLD binary
//	  -> its next turn's tail STREAMS LIVE to a merely-ATTACHED client of the
//	     new daemon
//	  -> the next session/new runs the NEW binary
//	  -> session/list shows MIXED binaryVersions
//
// The streaming step is the one that only became possible in slice 3b, and it is
// verified by mutation: dropping the SetEventRelay call makes the tail never
// arrive.
//
// # Why the streaming turn is a FRESH one, not the mid-flight turn
//
// A worker has NO continuous broker drain outside a session/prompt handler —
// internal/daemon/handlers.go's advertiseModelChange states this explicitly. The
// connection that drove the pre-upgrade turn is severed BY the upgrade, so that
// turn's own tail is published to the worker's broker and fanned out to nobody:
// the worker never puts it on the wire. No amount of router-side bridging can
// forward a frame that was never sent, and closing that gap would need a
// standing observer inside internal/worker — out of scope for this slice, and
// recorded here rather than papered over.
//
// What the bridge DOES fix is the hop it owns. The router receives a worker's
// events for a turn the ROUTER drives (rtr.Send, over the router's own
// persistent connection), and before slice 3b nothing forwarded them to the
// router's own clients unless one of those clients was itself running a
// session/prompt handler. The client here is watching, not driving — exactly the
// position an operator attaching to someone else's session is in — so every
// frame it receives travelled the event bridge and could not have arrived any
// other way.
//
// Determinism: there is not a single sleep. Every wait is a receive on a channel
// fed by an observable event — the worker's Ready callback, a notification off a
// real client connection, the handle's own seeded channel — and every timeout is
// a failure backstop, never a synchronization device.
//
// Two seams are REUSED rather than reinvented: the permission-ask lever that
// restart_approval_test.go pulls to hold a turn mid-flight, and slice 3a's
// faux-worker version env seam (fauxWorkerSeamOpts in crashisolation_test.go)
// for binary-skew injection.

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

// Binary versions the two halves of the upgrade report. They only have to
// DIFFER; the demo criterion is that both are simultaneously visible.
const (
	oldBinaryVersion = "6.0.0-old"
	newBinaryVersion = "6.1.0-new"
)

// demoWait bounds every wait in this file. Failure backstop only.
const demoWait = 20 * time.Second

// gatedTailSession is the permission-ask lever, extended with a TAIL. Its FIRST
// Prompt blocks on the injected gate, exactly like restart_approval_test.go's
// gatedSession; every later Prompt runs straight through and emits an assistant
// message plus the terminal turn.finished.
//
// The tail is what this test needs that the plain gate does not provide:
// permission.* events travel their own gofer/permission_* wire, so a session
// that only resolved its gate would prove nothing about the gofer/EVENT bridge.
// The message/turn tail rides gofer/event, so observing it at a merely-attached
// client of the new daemon is a direct assertion that the bridge carried it.
type gatedTailSession struct {
	id       string
	callID   string
	broker   *event.Broker
	approver loop.Approver
	released chan struct{}
	// turns counts Prompt calls, so only the FIRST one gates.
	turns atomic.Int64
}

func newGatedTailSession(id, callID string, approver loop.Approver) *gatedTailSession {
	return &gatedTailSession{
		id:       id,
		callID:   callID,
		broker:   event.NewBroker(event.WithReplay(64)),
		approver: approver,
		released: make(chan struct{}, 1),
	}
}

func (s *gatedTailSession) ID() string               { return s.id }
func (s *gatedTailSession) JournalPath() string      { return "" }
func (s *gatedTailSession) Fold() []provider.Message { return nil }
func (s *gatedTailSession) Events() *event.Subscription {
	return s.broker.Subscribe(event.FilterAll, 64)
}
func (s *gatedTailSession) EventsLive() *event.Subscription {
	return s.broker.SubscribeLive(event.FilterAll, 64)
}
func (s *gatedTailSession) Emit(e event.Event)       { s.broker.Publish(e) }
func (s *gatedTailSession) Cost() session.CostReport { return session.CostReport{} }
func (s *gatedTailSession) SetModel(string) error    { return nil }
func (s *gatedTailSession) Close() error             { s.broker.Close(); return nil }

// demoTailText is the assistant content the released turn emits — the marker the
// test looks for in a gofer/event frame at the attached client.
const demoTailText = "finished on the old binary"

// Prompt gates the FIRST turn on the permission approver — the mid-flight state
// the upgrade has to survive — and runs every later turn straight through. The
// second turn is the one whose tail the event bridge must carry (see the test's
// own comment on why it has to be a fresh, router-driven turn).
func (s *gatedTailSession) Prompt(ctx context.Context, text string) error {
	s.broker.Publish(event.NewTurnStarted(s.id))

	if s.turns.Add(1) == 1 {
		s.broker.Publish(event.NewPermissionRequested(s.id, s.callID, "bash", map[string]any{"command": text}, []string{"rule: ask"}))
		reply, err := s.approver.Await(ctx, s.callID)
		if err != nil {
			s.broker.Publish(event.NewPermissionResolved(s.id, s.callID, event.VerdictDeny, "cancelled"))
			s.broker.Publish(event.NewTurnFinished(s.id, "cancelled", provider.Usage{}))
			return err
		}
		s.broker.Publish(event.NewPermissionResolved(s.id, s.callID, reply.Verdict, "human"))
		s.broker.Publish(event.NewTurnFinished(s.id, "end_turn", provider.Usage{}))
		s.released <- struct{}{}
		return nil
	}

	// THE TAIL — what must stream live to a client of the upgraded daemon.
	s.broker.Publish(event.NewMessageStarted(s.id, event.MessageText))
	s.broker.Publish(event.NewMessageDelta(s.id, event.MessageText, demoTailText))
	s.broker.Publish(event.NewMessageFinished(s.id, event.MessageText, demoTailText))
	s.broker.Publish(event.NewTurnFinished(s.id, "end_turn", provider.Usage{}))
	return nil
}

// watchClient starts the SINGLE collector on c's notification stream, splitting
// it into the two streams this test asserts on: the text of every gofer/event
// frame that carries one, and the call id of every re-surfaced permission ask.
//
// One goroutine, not two, is essential. [daemon.Client.Notifications] is a plain
// channel, so two competing consumers would each receive a DISJOINT half of the
// stream — the permission ask would be silently swallowed by whichever collector
// happened to read it and did not care about it. (That is exactly how the first
// draft of this test failed, with the ask "never re-surfacing" while the router
// had in fact relayed it correctly.)
//
// Returning channels rather than slices is what keeps the test sleep-free: every
// assertion below is a receive.
func watchClient(c *daemon.Client) (texts <-chan string, askIDs <-chan string) {
	textCh := make(chan string, 64)
	askCh := make(chan string, 16)
	go func() {
		defer close(textCh)
		defer close(askCh)
		for n := range c.Notifications() {
			switch n.Method {
			case "gofer/event":
				var p struct {
					Text string `json:"text"`
				}
				if err := json.Unmarshal(n.Params, &p); err != nil || p.Text == "" {
					continue
				}
				select {
				case textCh <- p.Text:
				default:
				}
			case "gofer/permission_requested":
				var p struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(n.Params, &p); err != nil {
					continue
				}
				select {
				case askCh <- p.ID:
				default:
				}
			}
		}
	}()
	return textCh, askCh
}

// awaitText blocks until want appears on texts.
func awaitText(t *testing.T, texts <-chan string, want, what string) {
	t.Helper()
	deadline := time.After(demoWait)
	for {
		select {
		case got, ok := <-texts:
			if !ok {
				t.Fatalf("%s: connection closed before %q arrived", what, want)
			}
			if strings.Contains(got, want) {
				return
			}
		case <-deadline:
			t.Fatalf("%s: %q never arrived", what, want)
		}
	}
}

// TestUpgradeMidTurnMixedBinaryVersions is the §11 demo.
func TestUpgradeMidTurnMixedBinaryVersions(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()
	oldSessionID := uuid.Must(uuid.NewV7()).String()
	const callID = "demo-call-1"

	// ---- THE OLD WORLD: a worker on the OLD binary, holding a turn at its gate.

	gatedCh := make(chan *gatedTailSession, 1)
	build := func(_ context.Context, opts runner.Options) (supervisor.Session, error) {
		g := newGatedTailSession(oldSessionID, callID, opts.Approver)
		gatedCh <- g
		return g, nil
	}
	oldSup, err := supervisor.New(supervisor.Config{
		Root:       root,
		NewSession: build,
		ResumeSession: func(ctx context.Context, _ string, opts runner.Options) (supervisor.Session, error) {
			return build(ctx, opts)
		},
	})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}

	workerCtx, stopWorker := context.WithCancel(context.Background())
	ready := make(chan worker.Handshake, 1)
	workerErr := make(chan error, 1)
	go func() {
		workerErr <- worker.Serve(workerCtx, worker.Options{
			Supervisor: oldSup,
			Session:    oldSessionID,
			Version:    oldBinaryVersion, // slice 3a's version seam, worker-side
			Stdout:     io.Discard,
			Ready:      func(hs worker.Handshake) { ready <- hs },
		})
	}()
	t.Cleanup(func() {
		stopWorker()
		select {
		case <-workerErr:
		case <-time.After(demoWait):
		}
	})

	var hs worker.Handshake
	select {
	case hs = <-ready:
	case err := <-workerErr:
		t.Fatalf("old worker exited before ready: %v", err)
	case <-time.After(demoWait):
		t.Fatal("old worker never became ready")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Drive the turn to its gate over the worker's own socket, then sever the
	// connection: the gate lives in the WORKER, not in any client connection.
	driver, err := daemon.Dial(ctx, hs.Addr, "")
	if err != nil {
		t.Fatalf("dial old worker: %v", err)
	}
	asked := make(chan struct{}, 1)
	go func() {
		for n := range driver.Notifications() {
			if n.Method == "gofer/permission_requested" {
				select {
				case asked <- struct{}{}:
				default:
				}
			}
		}
	}()
	raw, err := driver.Call(ctx, acp.MethodSessionNew, acp.NewSessionRequest{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("session/new on old worker: %v", err)
	}
	var created acp.NewSessionResponse
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode session/new: %v", err)
	}
	go func() {
		// Severed below before it can return; the turn survives on the worker.
		_, _ = driver.Call(ctx, acp.MethodSessionPrompt, acp.PromptRequest{
			SessionID: oldSessionID,
			Prompt:    []acp.ContentBlock{acp.TextBlock("rm -rf /")},
		})
	}()
	select {
	case <-asked:
	case <-time.After(demoWait):
		t.Fatal("old worker never emitted the permission ask (turn not held mid-flight)")
	}
	gated := <-gatedCh
	_ = driver.Close()

	// ---- THE UPGRADE: a NEW router+daemon, spawning NEW-binary workers, adopts
	// the still-blocked old worker.

	rtr, err := New(Config{
		Root:         root,
		NewWorkerCmd: fauxWorkerSeamOpts(root, fauxWorkerOptions{Version: newBinaryVersion}),
	})
	if err != nil {
		t.Fatalf("router.New (the upgraded daemon): %v", err)
	}
	t.Cleanup(func() { killWorkers(rtr); _ = rtr.Close() })

	h, ok := rtr.get(oldSessionID)
	if !ok {
		t.Fatalf("upgraded router did not adopt the mid-turn worker %s", oldSessionID)
	}
	if h.binaryVersion != oldBinaryVersion {
		t.Errorf("adopted handle binaryVersion = %q, want %q", h.binaryVersion, oldBinaryVersion)
	}
	awaitSeed(t, h)

	d := daemon.New(rtr, daemon.Config{
		DefaultModel:                     "faux",
		ReplayPendingPermissionsOnAttach: true,
	})
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	newAddr := strings.TrimPrefix(srv.URL, "http://")

	// A real client of the UPGRADED daemon attaches to the adopted session.
	client, err := daemon.Dial(ctx, newAddr, "")
	if err != nil {
		t.Fatalf("dial upgraded daemon: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	tail, askIDs := watchClient(client)
	if _, err := client.Call(ctx, acp.MethodSessionLoad, acp.LoadSessionRequest{SessionID: oldSessionID, Cwd: t.TempDir()}); err != nil {
		t.Fatalf("attach to the adopted session: %v", err)
	}

	// Wire BOTH bridges, as cmd/gofer's daemon does after construction.
	rtr.SetPermissionRelay(d)
	rtr.SetEventRelay(d)

	select {
	case id := <-askIDs:
		if id != callID {
			t.Errorf("re-surfaced ask for call %q, want %q", id, callID)
		}
	case <-time.After(demoWait):
		t.Fatal("the mid-turn permission never re-surfaced at a client of the upgraded daemon")
	}

	// ---- THE OLD WORKER FINISHES ITS MID-FLIGHT TURN, ON THE OLD BINARY.

	if err := client.Notify(ctx, "permission.reply", map[string]any{
		"id": callID, "verdict": string(event.VerdictAllow),
	}); err != nil {
		t.Fatalf("permission.reply: %v", err)
	}
	select {
	case <-gated.released:
	case <-time.After(demoWait):
		t.Fatal("the mid-flight turn never completed after the reply routed through the upgraded daemon")
	}

	// ---- AND ITS NEXT TURN'S TAIL STREAMS LIVE THROUGH THE BRIDGE.
	//
	// The streaming turn has to be a FRESH, ROUTER-DRIVEN one rather than the
	// mid-flight turn above, and the reason is a real property of the system
	// worth recording: a worker has NO continuous broker drain outside a
	// session/prompt handler (internal/daemon/handlers.go's advertiseModelChange
	// says so explicitly). The connection that drove the pre-upgrade turn was
	// severed by the upgrade, so that turn's own tail is published to the
	// worker's broker and fanned out to nobody — the worker never puts it on the
	// wire at all. No amount of router-side bridging can forward a frame that was
	// never sent. Closing THAT gap would need a standing observer inside
	// internal/worker, which is out of this slice's scope.
	//
	// What the bridge does fix is the hop it owns: the router receives the
	// worker's events for a turn IT drives (rtr.Send, over the router's own
	// persistent connection), and before slice 3b nothing forwarded them to the
	// router's own clients unless one of those clients happened to be running a
	// session/prompt handler. A client attached to this session is in exactly
	// that position — it is watching, not driving — so every frame it receives
	// below travelled the slice-3b event bridge and could not have arrived any
	// other way.
	if err := rtr.Send(ctx, oldSessionID, "keep going"); err != nil {
		t.Fatalf("router-driven prompt on the adopted worker: %v", err)
	}
	awaitText(t, tail, demoTailText, "the adopted session's tail")

	// ---- THE NEXT SESSION RUNS THE NEW BINARY.

	newRaw, err := client.Call(ctx, acp.MethodSessionNew, acp.NewSessionRequest{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("session/new on the upgraded daemon: %v", err)
	}
	var newCreated acp.NewSessionResponse
	if err := json.Unmarshal(newRaw, &newCreated); err != nil {
		t.Fatalf("decode session/new: %v", err)
	}
	newSessionID := newCreated.SessionID
	if newSessionID == oldSessionID {
		t.Fatal("session/new returned the adopted session's id")
	}
	newHandle, ok := rtr.get(newSessionID)
	if !ok {
		t.Fatalf("router did not register a handle for the new session %s", newSessionID)
	}
	if newHandle.binaryVersion != newBinaryVersion {
		t.Errorf("new session's worker binaryVersion = %q, want %q", newHandle.binaryVersion, newBinaryVersion)
	}
	awaitSeed(t, newHandle)

	// ---- session/list SHOWS MIXED binaryVersions.

	versions := rosterVersions(t, ctx, client)
	if got := versions[oldSessionID]; got != oldBinaryVersion {
		t.Errorf("roster binaryVersion for the adopted session = %q, want %q", got, oldBinaryVersion)
	}
	if got := versions[newSessionID]; got != newBinaryVersion {
		t.Errorf("roster binaryVersion for the new session = %q, want %q", got, newBinaryVersion)
	}
	if versions[oldSessionID] == versions[newSessionID] {
		t.Fatalf("roster shows a single binary version %q; the demo criterion is a MIXED roster", versions[oldSessionID])
	}
}

// rosterVersions calls gofer/roster over c and returns session id -> binary
// version. It goes over the WIRE rather than reading the router's handles, so it
// asserts what a real client (a `gofer ps`, an editor, a phone) actually sees.
func rosterVersions(t *testing.T, ctx context.Context, c *daemon.Client) map[string]string {
	t.Helper()
	raw, err := c.Call(ctx, "gofer/roster", nil)
	if err != nil {
		t.Fatalf("gofer/roster: %v", err)
	}
	var rows []struct {
		ID            string `json:"id"`
		BinaryVersion string `json:"binaryVersion"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode gofer/roster: %v", err)
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.ID] = r.BinaryVersion
	}
	return out
}
