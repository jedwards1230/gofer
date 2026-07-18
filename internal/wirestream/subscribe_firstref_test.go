package wirestream_test

// subscribe_firstref_test.go pins the ACTUAL behavior of a first-reference
// SubscribeLive (the M6 review's medium finding): SubscribeLive skips a
// session broker's retained REPLAY backlog, but first-referencing a session
// through it still triggers the core's one-shot session/load history replay,
// whose events publish onto the broker AFTER the subscription exists — so they
// arrive as live events the no-replay subscription DOES observe. A consumer
// wanting a clean live-only stream must reference the session first (see
// SubscribeLive's doc). This is an external test because the history replay
// rides a real session/load over a real *daemon.Client — the bare-Reconstructor
// seam the internal tests use has no client to drive it.

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider/faux"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/wirestream"
)

const firstRefWait = 5 * time.Second

// newFauxDaemonURL stands up an in-process daemon over a faux-provider
// supervisor (no network, deterministic — mirroring internal/daemonbridge's
// own harness, rebuilt here against exported API only) and returns its ws://
// base URL. Guard/Approver/Tools are stripped: this test exercises history
// reconstruction over the wire, not the permission pipeline.
func newFauxDaemonURL(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	store, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		t.Fatalf("session.NewFileStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	build := func(opts runner.Options) runner.Options {
		opts.Store = store
		opts.Model = "faux"
		opts.Provider = faux.New(faux.Default())
		opts.Guard, opts.Approver, opts.Tools = nil, nil, nil
		return opts
	}
	sup, err := supervisor.New(supervisor.Config{
		Root:  root,
		Store: store,
		NewSession: func(ctx context.Context, opts runner.Options) (supervisor.Session, error) {
			return runner.New(ctx, build(opts))
		},
		ResumeSession: func(ctx context.Context, id string, opts runner.Options) (supervisor.Session, error) {
			return runner.Resume(ctx, id, build(opts))
		},
	})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	d := daemon.New(sup, daemon.Config{DefaultModel: "faux"})
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	return "ws" + srv.URL[len("http"):]
}

// dialCore dials url and returns both the raw client (for direct control calls
// like session/new) and a Reconstructor wrapping it. The Reconstructor owns the
// client's lifecycle — its Close (registered for cleanup) closes the client, so
// the caller never closes the client directly.
func dialCore(t *testing.T, url string) (*daemon.Client, *wirestream.Reconstructor) {
	t.Helper()
	c, err := daemon.Dial(context.Background(), url, "")
	if err != nil {
		t.Fatalf("daemon.Dial: %v", err)
	}
	r := wirestream.New(c)
	t.Cleanup(func() { _ = r.Close() })
	return c, r
}

// waitForUserPrompt drains sub until it observes a MessageFinished(user) with
// the given content, or fails after firstRefWait. Returns true on match.
func waitForUserPrompt(t *testing.T, sub *event.Subscription, content string) bool {
	t.Helper()
	deadline := time.After(firstRefWait)
	for {
		select {
		case ev, ok := <-sub.C:
			if !ok {
				return false
			}
			if fin, isFin := ev.(event.MessageFinished); isFin && fin.MessageKind == event.MessageUser && fin.Content == content {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

// TestSubscribeLiveFirstReferenceReplaysHistory pins the finding: a
// SubscribeLive that is the FIRST reference to a session with prior history
// observes that history (via the triggered session/load replay), because the
// replay publishes after the subscription exists. It seeds one turn on a first
// connection, closes it, then on a SECOND, fresh Reconstructor calls
// SubscribeLive as the very first touch of the session id and asserts the
// seeded user prompt shows up — the "flood" the no-replay stream is not
// supposed to want. The router avoids this by referencing the session
// (RegisterFresh / prior Subscribe) before SubscribeLive; that safe ordering is
// covered by the double-hop and attach tests.
func TestSubscribeLiveFirstReferenceReplaysHistory(t *testing.T) {
	url := newFauxDaemonURL(t)

	// Seed history on a first connection: create a session, run one turn, let it
	// settle, then drop the connection (the session stays live server-side).
	seedClient, seed := dialCore(t, url)
	raw, err := seedClient.Call(context.Background(), acp.MethodSessionNew, acp.NewSessionRequest{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("session/new: %v", err)
	}
	var created acp.NewSessionResponse
	if err := json.Unmarshal(raw, &created); err != nil {
		t.Fatalf("decode session/new: %v", err)
	}
	sid := created.SessionID

	seed.RegisterFresh(sid)
	seedSub, err := seed.Subscribe(context.Background(), sid)
	if err != nil {
		t.Fatalf("seed Subscribe: %v", err)
	}
	if err := seed.Send(context.Background(), sid, "hi"); err != nil {
		t.Fatalf("seed Send: %v", err)
	}
	if !waitForUserPrompt(t, seedSub, "hi") {
		t.Fatal("seed turn never settled its user prompt")
	}
	seedSub.Close()
	if err := seed.Close(); err != nil {
		t.Fatalf("seed Close: %v", err)
	}

	// A brand-new Reconstructor that has never referenced sid. SubscribeLive is
	// its FIRST reference — which triggers the session/load history replay.
	_, fresh := dialCore(t, url)
	liveSub, err := fresh.SubscribeLive(context.Background(), sid)
	if err != nil {
		t.Fatalf("SubscribeLive (first reference): %v", err)
	}
	defer liveSub.Close()

	// The pinned behavior: the seeded history DOES arrive on this "no-replay"
	// stream, because the load replay publishes after the subscription exists.
	if !waitForUserPrompt(t, liveSub, "hi") {
		t.Fatal("first-reference SubscribeLive did NOT observe the session/load history replay — " +
			"behavior changed; update SubscribeLive's doc and the router's reference-before-SubscribeLive contract")
	}
}
