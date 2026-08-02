package worker

// subagents_internal_test.go covers [RouterSubagents]' connection lifecycle and
// its model/cwd inheritance fallbacks — the branches that only run when
// something has gone wrong on the router side (a restart mid-session, a
// concurrent first spawn, a teardown racing a dial), and so are exactly the
// ones a happy-path integration test never reaches.
//
// It is an INTERNAL test (package worker, not worker_test) because dial is
// unexported and is the thing under test: asserting reconnection through Spawn
// alone would conflate it with the wire round trip on the other side.

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/provider/faux"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// serveRouterStub starts a daemon over a bare supervisor and returns its
// address plus a func that stops it — a stand-in for the router a worker dials
// back to. The supervisor hosts no sessions; every test here is about the
// CONNECTION, not about what is on the other end of it.
func serveRouterStub(t *testing.T) (addr string, stop func()) {
	t.Helper()
	sup, err := supervisor.New(supervisor.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	d := daemon.New(sup, daemon.Config{DefaultModel: "faux"})
	srv := httptest.NewServer(d.Handler())
	stopped := false
	return strings.TrimPrefix(srv.URL, "http://"), func() {
		if stopped {
			return
		}
		stopped = true
		srv.Close()
		_ = sup.Close()
	}
}

// TestRouterSubagentsDialIsLazyAndCached pins the two halves of "lazily
// dialed": nothing is connected until something needs it (a worker whose
// session never delegates must not hold a connection to the router for its
// whole life), and once connected the SAME client is reused rather than
// redialed per call.
func TestRouterSubagentsDialIsLazyAndCached(t *testing.T) {
	addr, stop := serveRouterStub(t)
	t.Cleanup(stop)

	r := NewRouterSubagents(addr, "", nil, nil)
	t.Cleanup(func() { _ = r.Close() })

	r.mu.Lock()
	pre := r.client
	r.mu.Unlock()
	if pre != nil {
		t.Fatal("NewRouterSubagents dialed eagerly; it must connect only on first use")
	}

	ctx := context.Background()
	first, err := r.dial(ctx)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	second, err := r.dial(ctx)
	if err != nil {
		t.Fatalf("second dial: %v", err)
	}
	if first != second {
		t.Fatal("dial redialed on a live connection; the client must be cached")
	}
}

// TestRouterSubagentsRedialsAfterTheRouterRestarts is the branch a router
// restart hits: the cached connection is dead, and the next spawn must
// transparently open a new one rather than failing forever against a corpse.
//
// A restart is the ordinary case, not an exotic one — workers are detached and
// deliberately outlive their router (design §3), so any worker alive across a
// `gofer daemon restart` reaches this path the first time its session delegates
// afterwards.
func TestRouterSubagentsRedialsAfterTheRouterRestarts(t *testing.T) {
	addr, stop := serveRouterStub(t)
	t.Cleanup(stop)

	r := NewRouterSubagents(addr, "", nil, nil)
	t.Cleanup(func() { _ = r.Close() })

	ctx := context.Background()
	first, err := r.dial(ctx)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Kill the connection out from under the seam, exactly as a router exit
	// would, and wait for the client to actually observe the close.
	_ = first.Close()
	select {
	case <-first.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the closed client never reported Done")
	}

	second, err := r.dial(ctx)
	if err != nil {
		t.Fatalf("redial after the connection dropped: %v", err)
	}
	if second == first {
		t.Fatal("dial handed back the DEAD client; a router restart would wedge every later spawn")
	}
	select {
	case <-second.Done():
		t.Fatal("the redialed client is already closed")
	default:
	}
}

// TestRouterSubagentsConcurrentDialKeepsOneClient covers the race-loser branch:
// two goroutines can reach the dial at once (a model firing two spawn calls in
// one turn), and both must end up on ONE client with the loser's connection
// closed rather than leaked.
//
// The assertion is the invariant, not the interleaving: every caller sees the
// same client, and the seam retains exactly that one. That is what the branch
// exists to guarantee, and it holds whether or not a given run happens to race.
func TestRouterSubagentsConcurrentDialKeepsOneClient(t *testing.T) {
	addr, stop := serveRouterStub(t)
	t.Cleanup(stop)

	r := NewRouterSubagents(addr, "", nil, nil)
	t.Cleanup(func() { _ = r.Close() })

	const n = 8
	var wg sync.WaitGroup
	got := make([]*daemon.Client, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = r.dial(context.Background())
		}()
	}
	wg.Wait()

	r.mu.Lock()
	retained := r.client
	r.mu.Unlock()
	if retained == nil {
		t.Fatal("no client retained after concurrent dials")
	}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("dial %d: %v", i, errs[i])
		}
		if got[i] != retained {
			t.Fatalf("dial %d returned a client the seam did not retain — the loser's connection leaks", i)
		}
	}
}

// TestRouterSubagentsCloseIsIdempotentAndFinal pins teardown: Close works with
// no connection ever opened, twice in a row, and permanently refuses later use.
// The last of those matters most — a dial that succeeded AFTER Close would
// reopen a connection nothing will ever close, on a worker that is shutting
// down.
func TestRouterSubagentsCloseIsIdempotentAndFinal(t *testing.T) {
	addr, stop := serveRouterStub(t)
	t.Cleanup(stop)

	t.Run("close with no connection", func(t *testing.T) {
		r := NewRouterSubagents(addr, "", nil, nil)
		if err := r.Close(); err != nil {
			t.Fatalf("Close with no connection: %v", err)
		}
		if err := r.Close(); err != nil {
			t.Fatalf("second Close: %v", err)
		}
	})

	t.Run("close after dialing refuses later use", func(t *testing.T) {
		r := NewRouterSubagents(addr, "", nil, nil)
		client, err := r.dial(context.Background())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		if err := r.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		select {
		case <-client.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("Close did not close the live connection")
		}
		if _, err := r.dial(context.Background()); err == nil {
			t.Fatal("dial succeeded after Close; a closed seam must stay closed")
		}
		// The seam's public surface refuses too, not just the unexported dial.
		if _, err := r.Spawn(context.Background(), "parent", "agent", "go"); err == nil {
			t.Fatal("Spawn succeeded after Close")
		}
		if err := r.Report(context.Background(), "parent", "done"); err == nil {
			t.Fatal("Report succeeded after Close")
		}
	})
}

// TestRouterSubagentsInheritFallbacks pins that a child's model/cwd inheritance
// can never REFUSE a spawn. Every way of not knowing the parent's settings —
// no supervisor at all, a getter that has not been filled in yet, a parent that
// is not on this worker's roster — must degrade to empty (which the router
// resolves to its own defaults), never to an error.
//
// This is the difference between a spawn that runs in a slightly less specific
// place and a spawn that fails; only the first is acceptable, since the
// inheritance is a convenience and the delegation is the point.
func TestRouterSubagentsInheritFallbacks(t *testing.T) {
	live, err := supervisor.New(supervisor.Config{Root: t.TempDir()})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = live.Close() })

	tests := []struct {
		name string
		sup  func() *supervisor.Supervisor
	}{
		{"no getter at all", nil},
		{"getter returns nil", func() *supervisor.Supervisor { return nil }},
		{"live supervisor that does not host the parent", func() *supervisor.Supervisor { return live }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRouterSubagents("127.0.0.1:1", "", tc.sup, nil)
			t.Cleanup(func() { _ = r.Close() })
			model, cwd := r.inherit(context.Background(), "not-a-live-session")
			if model != "" || cwd != "" {
				t.Fatalf("inherit = {model %q, cwd %q}, want both empty", model, cwd)
			}
		})
	}
}

// TestRouterSubagentsInheritsTheParentsSettings is the positive half: when the
// parent IS on this worker's roster, the child gets its model and cwd. Without
// it the fallback test above would pass just as happily against an inherit that
// always returned empty — which is precisely the vacuous shape the fallbacks
// make easy to ship by accident.
func TestRouterSubagentsInheritsTheParentsSettings(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	store, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		t.Fatalf("session.NewFileStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	sup, err := supervisor.New(supervisor.Config{
		Root:  root,
		Store: store,
		NewSession: func(ctx context.Context, opts runner.Options) (supervisor.Session, error) {
			opts.Store = store
			opts.Provider = faux.New(faux.Default())
			return runner.New(ctx, opts)
		},
	})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	parent, err := sup.Create(context.Background(), "", supervisor.CreateOptions{Cwd: cwd, Model: "faux-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	r := NewRouterSubagents("127.0.0.1:1", "", func() *supervisor.Supervisor { return sup }, nil)
	t.Cleanup(func() { _ = r.Close() })

	model, gotCwd := r.inherit(context.Background(), parent.ID)
	if model != "faux-1" || gotCwd != cwd {
		t.Fatalf("inherit = {model %q, cwd %q}, want the parent's {faux-1, %q}", model, gotCwd, cwd)
	}
}
