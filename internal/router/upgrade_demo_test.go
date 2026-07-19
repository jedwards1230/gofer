package router

// upgrade_demo_test.go is M6's milestone criterion (design §11), end to end.
// The criterion, verbatim: "upgrade the daemon binary mid-turn; the running
// session finishes uninterrupted on the old worker; the next session/new runs
// the new binary; session/list shows mixed binaryVersions." All four clauses are
// asserted below, so THE §11 CRITERION IS MET.
//
// What this test covers, in order:
//
//	upgrade the daemon MID-TURN
//	  -> the old worker FINISHES its mid-flight turn, on the OLD binary   [§11]
//	  -> that turn's tail is RECOVERABLE: it comes back as folded history
//	     on a re-attach, so nothing is durably lost
//	  -> its next turn's tail STREAMS LIVE to a merely-ATTACHED client of the
//	     new daemon                                                    [slice 3b]
//	  -> the next session/new runs the NEW binary                         [§11]
//	  -> session/list shows MIXED binaryVersions                          [§11]
//
// The live-streaming step is the one that only became possible in slice 3b, and
// it is verified by mutation: dropping the SetEventRelay call makes the tail
// never arrive.
//
// # A related property, deferred: live streaming of the PRE-upgrade turn's tail
//
// The criterion above never asked for the pre-upgrade turn's events to stream
// live to an attached client, and they do not. A worker has NO continuous broker
// drain outside a session/prompt handler — internal/daemon/handlers.go's
// advertiseModelChange states this explicitly — and the connection that drove
// the pre-upgrade turn is severed BY the upgrade, so that turn's tail is
// published to the worker's broker and never put on the wire at all. No amount
// of router-side bridging can forward a frame that was never sent.
//
// It is NOT durably lost, though, and the test asserts as much: the worker
// journals the tail, and it returns as folded history on the session's next
// session/load. The gap is "not streamed live", not "gone" — a re-attaching
// client sees the complete transcript.
//
// Closing the live half would need a standing observer on the WORKER side (in
// internal/daemon, which the worker runs via daemon.New/Serve), gated behind a
// Config flag beside ReplayPendingPermissionsOnAttach, and relying on exactly
// the promptHandlerActive guard this slice shipped to keep it from double-
// delivering against a real prompt handler. Deferred deliberately, recorded here
// and in internal/router/doc.go rather than papered over.
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
	root     string
	broker   *event.Broker
	approver loop.Approver
	released chan struct{}
	// journalErr carries a failure from the pre-upgrade turn's journal write, so
	// the test fails on it rather than silently asserting against a journal that
	// was never appended to.
	journalErr chan error
	// turns counts Prompt calls, so only the FIRST one gates.
	turns atomic.Int64
}

func newGatedTailSession(id, callID, root string, approver loop.Approver) *gatedTailSession {
	return &gatedTailSession{
		id:         id,
		callID:     callID,
		root:       root,
		broker:     event.NewBroker(event.WithReplay(64)),
		approver:   approver,
		released:   make(chan struct{}, 1),
		journalErr: make(chan error, 1),
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

// demoPreUpgradeTail is the PRE-upgrade turn's own assistant content: the tail
// that is not streamed live (its driving connection was severed by the upgrade)
// but IS journaled, and so must come back as folded history on a re-attach.
const demoPreUpgradeTail = "resolved after the upgrade, on the old binary"

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

		// THE PRE-UPGRADE TURN'S TAIL. Published to the broker, and JOURNALED —
		// modelling what a real runner does, which is what makes this tail
		// recoverable rather than lost. Nothing is draining the broker at this
		// point (the connection that drove this turn was severed by the upgrade),
		// so these events never reach the wire; the journal is the only path by
		// which a client can ever see them, and the test asserts exactly that.
		//
		// The journal write happens BEFORE released fires, so the test's re-attach
		// below is ordered strictly after it — no sleep, no polling.
		s.broker.Publish(event.NewMessageStarted(s.id, event.MessageText))
		s.broker.Publish(event.NewMessageFinished(s.id, event.MessageText, demoPreUpgradeTail))
		if jerr := journalAssistantMessage(s.root, s.id, demoPreUpgradeTail); jerr != nil {
			select {
			case s.journalErr <- jerr:
			default:
			}
		}

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

// createSessionJournal creates the on-disk journal a real worker's runner leaves
// for id under root, with the session_meta entry every fold needs. It uses
// CreateWithID because a worker pins its session uuid rather than minting one
// (design Option A), so the journal's id must match the id the router adopts.
//
// The router reads history from DISK ([Supervisor.History]), never from the
// worker, so this file is the only thing standing between a severed turn's tail
// and oblivion — which is precisely the property under test.
func createSessionJournal(t *testing.T, root, cwd, id string) {
	t.Helper()
	store, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		t.Fatalf("session.NewFileStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	j, err := store.CreateWithID(context.Background(), session.Slugify(cwd), id)
	if err != nil {
		t.Fatalf("store.CreateWithID(%s): %v", id, err)
	}
	if _, err := j.Append(session.NewMetaEntry(cwd)); err != nil {
		t.Fatalf("append meta entry: %v", err)
	}
	if err := j.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
}

// journalAssistantMessage appends text to id's journal as an assistant message —
// the journal write a real runner performs as it emits the message.
//
// It opens and closes a THROWAWAY store per call, mirroring what
// [Supervisor.History] does on the read side, so the entry is durably on disk by
// the time this returns and a subsequent History fold is guaranteed to see it.
func journalAssistantMessage(root, id, text string) error {
	store, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	j, err := store.Open(context.Background(), id)
	if err != nil {
		return err
	}
	defer func() { _ = j.Close() }()

	_, err = j.Append(session.NewMessageEntry(provider.Message{
		Role:    provider.RoleAssistant,
		Content: []provider.ContentBlock{{Type: provider.BlockText, Text: text}},
	}))
	return err
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
				// "text" is a live message.delta's field; "content" is a settled
				// message.finished's. History replay emits only the latter (see
				// internal/daemon's historyEvents), so a collector that read just
				// "text" would be blind to exactly the folded-history frames the
				// re-attach assertion depends on.
				var p struct {
					Text    string `json:"text"`
					Content string `json:"content"`
				}
				if err := json.Unmarshal(n.Params, &p); err != nil {
					continue
				}
				body := p.Text
				if body == "" {
					body = p.Content
				}
				if body == "" {
					continue
				}
				select {
				case textCh <- body:
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
	sessionCwd := t.TempDir()
	const callID = "demo-call-1"

	// The journal a real worker's runner would have created for this session. It
	// is what makes the pre-upgrade turn's tail recoverable after the upgrade.
	createSessionJournal(t, root, sessionCwd, oldSessionID)

	// ---- THE OLD WORLD: a worker on the OLD binary, holding a turn at its gate.

	gatedCh := make(chan *gatedTailSession, 1)
	build := func(_ context.Context, opts runner.Options) (supervisor.Session, error) {
		g := newGatedTailSession(oldSessionID, callID, root, opts.Approver)
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
	select {
	case jerr := <-gated.journalErr:
		t.Fatalf("the pre-upgrade turn failed to journal its tail: %v", jerr)
	default:
	}

	// ---- AND THAT TURN'S TAIL IS NOT DURABLY LOST.
	//
	// This is the property the file header claims and this phase pins. The
	// pre-upgrade turn's tail was published to the worker's broker with nothing
	// draining it — its driving connection was severed by the upgrade — so it
	// never reached the wire and no attached client saw it live. But the worker
	// JOURNALED it, so it comes back as folded history the moment anyone
	// re-attaches: [Supervisor.History] reads the journal from disk through a
	// throwaway store (never asking the worker), the daemon projects that fold
	// into gofer/event history frames, and session/load replays them before its
	// response.
	//
	// A FRESH client is used rather than the attached one, because that is the
	// real scenario: an operator whose client went away with the old daemon comes
	// back and expects the complete transcript. Ordering is strict, not timed —
	// the journal write completes before released fires above, so the fold read
	// below cannot race it.
	reattached, err := daemon.Dial(ctx, newAddr, "")
	if err != nil {
		t.Fatalf("re-attach dial: %v", err)
	}
	t.Cleanup(func() { _ = reattached.Close() })
	recovered, _ := watchClient(reattached)
	if _, err := reattached.Call(ctx, acp.MethodSessionLoad, acp.LoadSessionRequest{SessionID: oldSessionID, Cwd: sessionCwd}); err != nil {
		t.Fatalf("re-attach to the adopted session: %v", err)
	}
	awaitText(t, recovered, demoPreUpgradeTail, "the pre-upgrade turn's tail, recovered from the journal")

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
