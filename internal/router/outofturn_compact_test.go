package router

// outofturn_compact_test.go pins the property jedwards1230/gofer#280 exists for:
// under --workers, an event published with NO session/prompt in flight still
// reaches a merely-ATTACHED client.
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
	kinds := watchKinds(watcher)
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

	awaitKind(t, kinds, event.KindSessionCompacted, "the out-of-turn compaction")
}

// watchKinds starts the single collector on c's notification stream and reports
// the kind of every gofer/event frame. One goroutine, not several:
// [daemon.Client.Notifications] is a plain channel, so competing consumers would
// each receive a DISJOINT half of the stream (see watchClient's own note).
//
// The wire field is "type", NOT "kind" — an event's own MarshalJSON envelope
// names it that way, which is why wirestream's own decoder reads
// goferEventWire.Type. A collector keyed on "kind" matches nothing and reports a
// silent, permanently empty stream.
func watchKinds(c *daemon.Client) <-chan string {
	out := make(chan string, 128)
	go func() {
		defer close(out)
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
			select {
			case out <- p.Type:
			default:
			}
		}
	}()
	return out
}

// awaitKind blocks until want appears on kinds. The deadline is a failure
// backstop, never a synchronization device.
func awaitKind(t *testing.T, kinds <-chan string, want, what string) {
	t.Helper()
	deadline := time.After(demoWait)
	for {
		select {
		case got, ok := <-kinds:
			if !ok {
				t.Fatalf("%s: connection closed before a %q frame arrived", what, want)
			}
			if got == want {
				return
			}
		case <-deadline:
			t.Fatalf("%s: no %q frame ever reached the attached client", what, want)
		}
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
