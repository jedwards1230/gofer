package router

// outofturn_compact_test.go pins the property jedwards1230/gofer#280 exists for:
// under --workers, an event published with NO session/prompt in flight still
// reaches a merely-ATTACHED client. Two kinds are pinned here — session.compacted
// (the one the milestone started from) and session.config (the second, which
// failed at a different hop; see TestOutOfTurnModelChangeReachesAttachedClient).
//
// Compaction is the event that forces the question. session.compacted is
// must-deliver and the PRD's hard constraint is that a compaction is never
// silent — but explicit gofer/compact is idle-only and the automatic trigger
// fires BETWEEN turns, so in both cases there is no prompt handler anywhere
// draining the worker's broker. Before the worker-side observer, the frame was
// published inside the worker process and never put on the wire at all; the
// router's event bridge cannot forward a frame that was never sent.
//
// Why this test is at the router layer and drives a REAL worker process: the
// chain has four hops and only the whole chain is the property.
//
//	worker supervisor broker
//	  -> the worker daemon's standing observer      (what #280 adds)
//	  -> the router's persistent client connection  (gofer/event on the wire)
//	  -> wirestream reconstruction + its EventSink
//	  -> the router daemon's EventRelay -> the attached client
//
// internal/router/resume_test.go's recordingRelay fake stops at hop 3, so a
// green run there is not coverage for this. The re-exec harness in
// crashisolation_test.go (TestMain -> runFauxWorker) is reused verbatim: it runs
// the REAL production worker.Serve, which is the only place
// Config.RelayOutOfTurnEvents is ever set.
//
// The lever is a real RPC, not a timer and not a test-only env var:
// gofer/compact already exists end to end (internal/daemon's handleGoferCompact
// -> Supervisor.Compact -> the worker's own gofer/compact), and it is idle-only
// by contract — so calling it is exactly the "no prompt in flight" condition
// under test, with no race to arrange.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// TestOutOfTurnCompactionReachesAttachedClient drives a compaction on a session
// hosted by a real worker process, with nothing prompting, and asserts the
// resulting session.compacted arrives at a client that is merely attached.
func TestOutOfTurnCompactionReachesAttachedClient(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()
	sup, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	// Detached workers survive Supervisor.Close (design §3), so reap them here or
	// they outlive the test binary as orphans.
	t.Cleanup(func() {
		killWorkers(sup)
		_ = sup.Close()
	})

	d := daemon.New(sup, daemon.Config{DefaultModel: "faux"})
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	// The router-facing half of the bridge, wired exactly as cmd/gofer's daemon
	// does after construction. The router daemon itself must NOT set
	// RelayOutOfTurnEvents — only the worker does, or every out-of-turn event
	// would be delivered twice.
	sup.SetEventRelay(d)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// The driver creates the session and runs one turn. The turn is not
	// incidental: Compact refuses an empty context (runner.ErrNothingToCompact),
	// so there must be folded history to summarize.
	driver := mustDial(t, ctx, addr)
	go func() {
		for range driver.Notifications() {
		}
	}()
	cwd := t.TempDir()
	sessionID := mustNewSession(t, ctx, driver, cwd)
	mustPrompt(t, ctx, driver, sessionID)

	// The WATCHER is the client the property is about: it attaches and then does
	// nothing at all. It never prompts, so no prompt handler in either daemon is
	// fanning this session's events out on its behalf — every frame it receives
	// below travelled the out-of-turn path and could not have arrived any other
	// way.
	watcher := mustDial(t, ctx, addr)
	kinds := watchKinds(watcher, event.KindSessionCompactionStarted, event.KindSessionCompacted)
	if _, err := watcher.Call(ctx, acp.MethodSessionLoad, acp.LoadSessionRequest{
		SessionID: sessionID,
		Cwd:       cwd,
	}); err != nil {
		t.Fatalf("watcher attach to %s: %v", sessionID, err)
	}

	// THE OUT-OF-TURN EVENT. gofer/compact is idle-only, so this call is itself
	// the proof that no turn is in flight: the worker's supervisor refuses it
	// with ErrRunning otherwise.
	mustCompact(t, ctx, driver, sessionID)

	// Both terminals of the compaction, not just the settled one.
	// session.compaction_started arrived with SDK v0.24.0 and rides the same
	// runner.Compact publish path, so it is out-of-turn for exactly the same
	// reason — and it is worth asserting HERE rather than trusting the unit
	// round trip, because it is the first kind to cross this whole chain that
	// nobody hand-wrote a decode case for.
	awaitKind(t, kinds, event.KindSessionCompactionStarted, "the out-of-turn compaction start")
	awaitKind(t, kinds, event.KindSessionCompacted, "the out-of-turn compaction")
}

// TestOutOfTurnModelChangeReachesAttachedClient is the same property for
// session.config, and it exists because session.compacted turned out NOT to be
// the last kind that could go missing on this path — this is the second.
//
// The two failures are at DIFFERENT hops, which is why one test does not cover
// the other and why fixing the first did not fix this. session.compacted was
// never put on the wire at all: with no prompt handler draining the worker's
// broker, the frame stayed inside the worker process, and the worker-side
// standing observer is what put it there. session.config WAS on the wire the
// whole time — the worker's advertiseModelChange broadcasts it directly to its
// attached peer, the router — and it died one layer lower, in the router's own
// decode: wirestream's hand-rolled per-kind dispatch table had no case for
// `session.config`, so the frame fell to a default that returned before both the
// publish and the EventSink push (docs/EVENT-MATRIX.md's GAP 2). A frame that is
// decoded by nobody is indistinguishable from a frame that was never sent, so
// this was invisible in-turn as well as out-of-turn, for every client under
// --workers.
//
// The lever is a real RPC for the same reason the compaction test's is:
// gofer/set_model exists end to end (the router forwards it to the worker, whose
// daemon advertises the change — see internal/daemon's advertiseModelChange), and
// the model swap happens with nothing prompting, so the frame the watcher
// receives could only have travelled the out-of-turn path.
//
// The model must be one provider.Resolve accepts (the SDK gates a swap on it) and
// must DIFFER from the session's current model, because advertiseModelChange
// returns early when current == prev. The faux worker's sessions run "faux",
// which Resolve cannot place at all — so there is no second faux model to swap
// to, and the swap targets a registered id instead. That is safe precisely
// because nothing prompts afterwards: a runner's provider client is fixed at
// creation, so the session keeps its faux provider and the new id is never dialled.
func TestOutOfTurnModelChangeReachesAttachedClient(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()
	sup, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	// Detached workers survive Supervisor.Close (design §3), so reap them here or
	// they outlive the test binary as orphans.
	t.Cleanup(func() {
		killWorkers(sup)
		_ = sup.Close()
	})

	d := daemon.New(sup, daemon.Config{DefaultModel: "faux"})
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	// Same wiring as the compaction test: the router daemon is the relay end of
	// the bridge and must NOT set RelayOutOfTurnEvents — only the worker does.
	sup.SetEventRelay(d)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	driver := mustDial(t, ctx, addr)
	go func() {
		for range driver.Notifications() {
		}
	}()
	cwd := t.TempDir()
	sessionID := mustNewSession(t, ctx, driver, cwd)
	// One turn, so the watcher's session/load has real history to replay and the
	// model change lands BETWEEN turns rather than before the session has ever
	// run — the shape a user actually hits. It is not required by set_model
	// itself (unlike compaction, which refuses an empty context).
	mustPrompt(t, ctx, driver, sessionID)

	// The WATCHER only attaches. It never prompts, so no prompt handler in either
	// daemon is draining this session on its behalf and every frame it sees
	// travelled the out-of-turn path.
	watcher := mustDial(t, ctx, addr)
	kinds := watchKinds(watcher, event.KindSessionConfig)
	if _, err := watcher.Call(ctx, acp.MethodSessionLoad, acp.LoadSessionRequest{
		SessionID: sessionID,
		Cwd:       cwd,
	}); err != nil {
		t.Fatalf("watcher attach to %s: %v", sessionID, err)
	}

	// THE OUT-OF-TURN EVENT.
	mustSetModel(t, ctx, driver, sessionID, outOfTurnSwapModel)

	awaitKind(t, kinds, event.KindSessionConfig, "the out-of-turn model change")
}

// outOfTurnSwapModel is the model TestOutOfTurnModelChangeReachesAttachedClient
// swaps to. It is a registered SDK model id rather than a second faux one
// because provider.Resolve — which both supervisor.SetModel and runner.SetModel
// gate on — cannot place "faux" (it is in neither the registry nor
// providerPrefixes), so "faux2" and friends are rejected outright. Resolve
// failing for the CURRENT model is what makes this legal: the cross-provider
// guard is skipped when the current id cannot be placed, so an anthropic id is
// accepted on a faux session.
const outOfTurnSwapModel = "claude-sonnet-5"

// mustSetModel calls gofer/set_model for sessionID. Unlike mustCompact it needs
// no retry: a model change has no idle-only restriction anywhere on the chain
// (the SDK runner reads the model at the top of the next turn), so the only
// errors it can return are real ones.
func mustSetModel(t *testing.T, ctx context.Context, c *daemon.Client, sessionID, model string) {
	t.Helper()
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := c.Call(cctx, "gofer/set_model", map[string]string{
		"sessionId": sessionID,
		"model":     model,
	}); err != nil {
		t.Fatalf("gofer/set_model %s -> %s: %v", sessionID, model, err)
	}
}

// kindWatcher reports whether each of a fixed set of gofer/event kinds has been
// seen on a client's notification stream. It does NOT queue frames, and that is
// the point: a buffered channel of every arriving kind can drop the one the test
// asserts on — a session/load replays folded history, so a long transcript
// bursts well past any fixed buffer, and a non-blocking send would then fail a
// CORRECT system. (Blocking instead is not the fix either: it wedges the
// collector, and through it the client's whole notification stream, whenever the
// test stops reading — which is exactly what happens on the failure path.)
//
// So the state is one already-closed-or-not channel per WANTED kind. First
// sighting closes it; every later sighting is a no-op. Closing cannot block and
// cannot drop, so an arrival can never be lost no matter how deep the burst,
// and awaitKind's wait becomes a plain receive on a closed channel.
type kindWatcher struct {
	seen map[string]chan struct{}
	// closed is closed when the client's notification stream ends, so a dropped
	// connection fails awaitKind immediately with the reason instead of burning
	// the full deadline on a stream that can no longer deliver anything.
	closed chan struct{}
}

// watchKinds starts the single collector on c's notification stream, latching
// each of want the first time a gofer/event frame carries it. One goroutine, not
// several: [daemon.Client.Notifications] is a plain channel, so competing
// consumers would each receive a DISJOINT half of the stream (see watchClient's
// own note).
//
// The wire field is "type", NOT "kind" — an event's own MarshalJSON envelope
// names it that way, which is what the SDK's [event.Unmarshal] reads on the
// router side. A collector keyed on "kind" matches nothing and reports a
// silent, permanently empty stream.
//
// Kinds outside want are ignored rather than recorded: asserting on a kind the
// watcher was not asked about is a test bug, and awaitKind fails loudly for it
// rather than blocking until the deadline.
func watchKinds(c *daemon.Client, want ...string) *kindWatcher {
	w := &kindWatcher{
		seen:   make(map[string]chan struct{}, len(want)),
		closed: make(chan struct{}),
	}
	for _, k := range want {
		w.seen[k] = make(chan struct{})
	}
	go func() {
		defer close(w.closed)
		for n := range c.Notifications() {
			if n.Method != "gofer/event" {
				continue
			}
			var p struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(n.Params, &p); err != nil || p.Type == "" {
				continue
			}
			ch, wanted := w.seen[p.Type]
			if !wanted {
				continue
			}
			// Idempotent: a kind can legitimately arrive more than once (the
			// worker's direct fan-out and its standing observer can both carry
			// one), and closing twice panics.
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
	}()
	return w
}

// awaitKind blocks until want has been seen. The deadline is a failure backstop,
// never a synchronization device.
func awaitKind(t *testing.T, w *kindWatcher, want, what string) {
	t.Helper()
	ch, wanted := w.seen[want]
	if !wanted {
		t.Fatalf("%s: watchKinds was never asked to watch %q", what, want)
	}
	select {
	case <-ch:
	case <-w.closed:
		// Re-check: the stream ending and the frame arriving can be observed in
		// either order, and a frame latched just before the close is a PASS.
		select {
		case <-ch:
		default:
			t.Fatalf("%s: connection closed before a %q frame arrived", what, want)
		}
	case <-time.After(demoWait):
		t.Fatalf("%s: no %q frame ever reached the attached client", what, want)
	}
}

// mustCompact calls gofer/compact for sessionID, retrying while the worker still
// reports the just-finished turn as running.
//
// The retry is not a sleep-until-it-works: session/prompt returns once the turn
// has finished streaming, and the supervisor clears its own running state a beat
// later on the pump goroutine, so an immediate compact can legitimately lose
// that footrace and be refused with ErrRunning. Every OTHER error fails
// immediately, so a genuinely broken compact path can never be papered over as
// "still running".
func mustCompact(t *testing.T, ctx context.Context, c *daemon.Client, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(demoWait)
	for {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_, err := c.Call(cctx, "gofer/compact", map[string]string{"sessionId": sessionID})
		cancel()
		if err == nil {
			return
		}
		if !strings.Contains(err.Error(), "session is running") {
			t.Fatalf("gofer/compact %s: %v", sessionID, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("gofer/compact %s: session never went idle: %v", sessionID, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
