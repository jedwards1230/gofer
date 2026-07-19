package router

// rostercache_test.go pins the PUSH-based roster (design §8, slice 3b): after a
// single seed per handle, Roster/List serve from cache and issue ZERO worker
// RPCs, and the cached row tracks the session's event stream.
//
// The central assertion is a COUNT taken at the worker, not a timing or a
// benchmark: a counting fake worker tallies every gofer/roster frame it
// receives, so a regression that quietly reintroduces a per-call RPC fails the
// test outright rather than merely making the roster slower.
//
// Everything here is deterministic — no sleeps. The seed's completion is
// observable on the handle's `seeded` channel (which exists for exactly this),
// and each pushed event's effect is awaited on a channel the fake worker closes
// when it has written the frame, plus a bounded poll on the published snapshot.
// Timeouts are failure backstops only.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// cacheTimeout bounds every wait in this file. It is a FAILURE BACKSTOP: each
// wait is on an observable signal that arrives in microseconds in practice.
const cacheTimeout = 10 * time.Second

// countingWorker is a fake worker that answers the router's adoption handshake
// and its one seed gofer/roster call, TALLIES every gofer/roster frame it
// receives, and can push gofer/event notifications on demand so a test can drive
// the cached row from the event stream the way a real worker does.
type countingWorker struct {
	id string
	// helloVersion is the build this worker reports over gofer/hello — the value
	// the router stamps on the handle and so onto every cached row. Set before
	// the router dials, never mutated after.
	helloVersion string

	// rosterCalls counts gofer/roster frames. The whole point of the cache is
	// that this reaches 1 (the seed) and stays there.
	rosterCalls atomic.Int64

	// row is the roster row the worker reports for its own session; the seed
	// snapshots it.
	row map[string]any

	mu   sync.Mutex
	conn *websocket.Conn
	ctx  context.Context
	// connected closes once the router's connection is established, so a test can
	// push events without racing the dial.
	connected chan struct{}
	once      sync.Once
}

// startCountingWorker binds a worker socket for a fresh session id, writes its
// endpoint file so the router's startup scan adopts it, and serves the minimal
// worker protocol the cache path needs.
func startCountingWorker(t *testing.T, version string) *countingWorker {
	t.Helper()
	id := uuid.Must(uuid.NewV7()).String()
	w := &countingWorker{
		id:           id,
		helloVersion: version,
		connected:    make(chan struct{}),
		row: map[string]any{
			"id": id, "title": "seeded title", "status": "working",
			"model": "faux", "project": "gofer", "live": true,
			"queued": 3, "created": time.Now().UTC(), "updated": time.Now().UTC(),
		},
	}

	sockPath, err := daemon.WorkerSocketPath(id)
	if err != nil {
		t.Fatalf("WorkerSocketPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatalf("mkdir workers dir: %v", err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}

	srv := &http.Server{Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		c, aerr := websocket.Accept(rw, r, nil)
		if aerr != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		w.mu.Lock()
		w.conn, w.ctx = c, r.Context()
		w.mu.Unlock()
		w.once.Do(func() { close(w.connected) })
		w.serve(r.Context(), c)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = ln.Close() })

	// The endpoint file's own BinaryVersion is only the pre-dial hint; the
	// gofer/hello reply above is what the router actually stamps on the handle.
	writeEndpoint(t, id, os.Getpid(), daemon.WireVersion, "unix://"+sockPath)
	return w
}

// serve is the request loop: gofer/hello and gofer/roster get real answers,
// everything else a clean application error (a fake worker that ignored requests
// would wedge the adopting router on its bounded Calls).
func (w *countingWorker) serve(ctx context.Context, c *websocket.Conn) {
	for {
		var env struct {
			Method string          `json:"method"`
			ID     json.RawMessage `json:"id"`
		}
		if err := wsjson.Read(ctx, c, &env); err != nil {
			return
		}
		switch {
		case env.Method == "gofer/hello":
			w.write(ctx, c, jsonRPCResult(env.ID, map[string]any{
				"binaryVersion": w.helloVersion,
				"wireVersion":   daemon.WireVersion,
				"sessionId":     w.id,
			}))
		case env.Method == "gofer/roster":
			w.rosterCalls.Add(1)
			w.write(ctx, c, jsonRPCResult(env.ID, []any{w.row}))
		case len(env.ID) > 0:
			w.write(ctx, c, jsonRPCError(env.ID, -32000, "counting worker: "+env.Method+" unimplemented"))
		}
	}
}

func (w *countingWorker) write(ctx context.Context, c *websocket.Conn, v any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = wsjson.Write(ctx, c, v)
}

// pushEvent sends one gofer/event notification, in the event's own MarshalJSON
// envelope — exactly what a real worker puts on the wire.
func (w *countingWorker) pushEvent(t *testing.T, params map[string]any) {
	t.Helper()
	select {
	case <-w.connected:
	case <-time.After(cacheTimeout):
		t.Fatal("router never connected to the counting worker")
	}
	w.mu.Lock()
	c, ctx := w.conn, w.ctx
	w.mu.Unlock()
	if c == nil {
		t.Fatal("counting worker has no connection to push on")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := wsjson.Write(ctx, c, map[string]any{
		"jsonrpc": "2.0", "method": "gofer/event", "params": params,
	}); err != nil {
		t.Fatalf("push gofer/event: %v", err)
	}
}

// awaitSeed blocks until the handle's roster seed has settled — success or
// failure. This is what makes the zero-RPC assertion deterministic instead of a
// sleep: after it returns, every subsequent Roster must be cache-served.
func awaitSeed(t *testing.T, h *workerHandle) {
	t.Helper()
	select {
	case <-h.seeded:
	case <-time.After(cacheTimeout):
		t.Fatal("roster cache seed never settled")
	}
}

// awaitSnapshot polls the handle's published snapshot until want reports true.
// The poll is on an atomic load of an immutable value — never a sleep standing
// in for a signal — and is bounded by cacheTimeout as a failure backstop. It
// exists because a pushed event's effect lands on the watcher goroutine, which
// has no completion channel of its own to wait on.
//
// Each miss YIELDS rather than spinning. A bare retry loop here is a busy-spin
// that burns a core for the whole wait, and — worse for a test — starves the
// very watcher goroutine it is waiting on whenever the scheduler has fewer
// runnable Ps than the package's other tests are already using. Gosched is the
// yield, not a sleep: it hands the processor over and resumes immediately, so
// the loop still observes the update at the first opportunity.
func awaitSnapshot(t *testing.T, h *workerHandle, what string, want func(supervisor.SessionInfo) bool) supervisor.SessionInfo {
	t.Helper()
	deadline := time.After(cacheTimeout)
	for {
		if info := h.info.Load(); info != nil && want(*info) {
			return *info
		}
		select {
		case <-deadline:
			var got supervisor.SessionInfo
			if info := h.info.Load(); info != nil {
				got = *info
			}
			t.Fatalf("cached roster row never reached %s; last snapshot: %+v", what, got)
		default:
			runtime.Gosched()
		}
	}
}

// adoptCountingWorker stands up a router that adopts w and returns it with w's
// handle.
func adoptCountingWorker(t *testing.T, root string, w *countingWorker) (*Supervisor, *workerHandle) {
	t.Helper()
	sup, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	h, ok := sup.get(w.id)
	if !ok {
		t.Fatalf("router did not adopt the counting worker %s", w.id)
	}
	awaitSeed(t, h)
	return sup, h
}

// TestRosterServesFromCacheWithoutWorkerRPCs is the §8 assertion: after the one
// seed, no number of Roster/List calls produces another gofer/roster frame at
// the worker.
//
// Before the cache, every Roster cost one RPC PER LIVE WORKER — so this loop
// would have tallied 21 calls, and a wedged worker socket would have sat on the
// latency path of an operator listing an unrelated session.
func TestRosterServesFromCacheWithoutWorkerRPCs(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()
	w := startCountingWorker(t, "0.3.0")
	sup, _ := adoptCountingWorker(t, root, w)

	// Bring-up costs a small CONSTANT number of roster calls, independent of how
	// often the roster is later read: one for the cache seed, plus one from
	// adoption's own wirestream Load (its sessionCwd lookup resolves the session's
	// cwd off a roster row — pre-existing, and unrelated to this cache). What
	// matters is that the number does not GROW with reads; the bound is asserted
	// so a new per-bring-up RPC still has to be a deliberate change.
	seedCalls := w.rosterCalls.Load()
	if seedCalls < 1 || seedCalls > 2 {
		t.Fatalf("bring-up issued %d gofer/roster calls, want 1 (seed) or 2 (seed + adoption's cwd lookup)", seedCalls)
	}

	ctx := context.Background()
	for range 20 {
		rows, err := sup.Roster(ctx)
		if err != nil {
			t.Fatalf("Roster: %v", err)
		}
		if len(rows) != 1 || rows[0].ID != w.id {
			t.Fatalf("Roster returned %d rows %+v, want exactly the cached row for %s", len(rows), rows, w.id)
		}
	}
	// List unions the live roster with disk, so it must be cache-served too.
	if _, err := sup.List(ctx); err != nil {
		t.Fatalf("List: %v", err)
	}

	if got := w.rosterCalls.Load(); got != seedCalls {
		t.Errorf("worker received %d gofer/roster calls after the seed, want 0 (cache is not being read — %d total)", got-seedCalls, got)
	}
}

// TestRosterCacheSeedContent pins that the cache-served row carries the seed's
// content and the ROUTER's binary-version stamp. The version is stamped from the
// handle's gofer/hello, not read off the row: a worker's own roster reports the
// sessions it hosts and has no reason to know it is being proxied.
func TestRosterCacheSeedContent(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()
	w := startCountingWorker(t, "0.9.9")
	sup, _ := adoptCountingWorker(t, root, w)

	rows, err := sup.Roster(context.Background())
	if err != nil {
		t.Fatalf("Roster: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("Roster returned %d rows, want 1", len(rows))
	}
	got := rows[0]
	if got.Title != "seeded title" {
		t.Errorf("cached row Title = %q, want %q", got.Title, "seeded title")
	}
	if got.BinaryVersion != "0.9.9" {
		t.Errorf("cached row BinaryVersion = %q, want %q (stamped by the router from gofer/hello)", got.BinaryVersion, "0.9.9")
	}
	if !got.Live {
		t.Error("cached row Live = false, want true (it came from a live worker)")
	}
	// Queued has no event to maintain it, so it keeps its seeded value — a
	// documented limitation of the push model, pinned here so the behavior is
	// deliberate rather than accidental.
	if got.Queued != 3 {
		t.Errorf("cached row Queued = %d, want the seeded 3 (no event maintains Queued)", got.Queued)
	}
}

// TestRosterCacheTracksEventStream drives the cached row from the worker's event
// stream — the "pushed" half of the design. A turn.finished must flip the row to
// needs-input and fold in its usage/cost deltas, and a session.info must retitle
// it, all without another worker RPC.
func TestRosterCacheTracksEventStream(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()
	w := startCountingWorker(t, "0.3.0")
	_, h := adoptCountingWorker(t, root, w)
	afterSeed := w.rosterCalls.Load()

	w.pushEvent(t, map[string]any{
		"type": string(event.KindSessionInfo), "session_id": w.id, "title": "retitled by the stream",
	})
	awaitSnapshot(t, h, "the pushed title", func(i supervisor.SessionInfo) bool {
		return i.Title == "retitled by the stream"
	})

	w.pushEvent(t, map[string]any{
		"type": string(event.KindTurnFinished), "session_id": w.id, "stop_reason": "end_turn",
		"usage": map[string]any{"input_tokens": 10, "output_tokens": 20},
	})
	got := awaitSnapshot(t, h, "needs-input after turn.finished", func(i supervisor.SessionInfo) bool {
		return i.Status == supervisor.StatusNeedsInput
	})
	if got.Usage.InputTokens != 10 || got.Usage.OutputTokens != 20 {
		t.Errorf("cached row Usage = %+v, want the turn's 10/20 folded in", got.Usage)
	}
	if got.Title != "retitled by the stream" {
		t.Errorf("cached row Title = %q; the later event clobbered the earlier one", got.Title)
	}

	// Still zero RPCs: every one of those updates came off the stream.
	if got := w.rosterCalls.Load(); got != afterSeed {
		t.Errorf("worker received %d gofer/roster calls while the stream drove the row, want 0", got-afterSeed)
	}
}

// TestRosterCacheMidTurnStatus pins that a turn.finished carrying the loop's
// MID-turn "tool_use" marker does NOT retire the row to needs-input: the model
// is about to run tools and call again within the same dispatch, so the session
// is still working. It mirrors the same test in wirestream's handleGoferEvent
// and the daemon's prompt handler — three places that must agree.
func TestRosterCacheMidTurnStatus(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()
	w := startCountingWorker(t, "0.3.0")
	_, h := adoptCountingWorker(t, root, w)

	// Seeded "working"; a tool_use turn.finished must leave it working, and its
	// usage must still fold in.
	w.pushEvent(t, map[string]any{
		"type": string(event.KindTurnFinished), "session_id": w.id, "stop_reason": "tool_use",
		"usage": map[string]any{"input_tokens": 5},
	})
	got := awaitSnapshot(t, h, "the tool_use usage fold", func(i supervisor.SessionInfo) bool {
		return i.Usage.InputTokens == 5
	})
	if got.Status != supervisor.StatusWorking {
		t.Errorf("cached row Status = %v after a tool_use turn.finished, want still working", got.Status)
	}
}

// TestRosterCacheMissFallsBackToRPC pins the DEGRADED path: a handle with no
// cached row falls back to a live per-call RPC rather than vanishing from the
// roster. A struggling worker degrades to the pre-cache behavior; it does not
// disappear, which would make an operator think the session was gone.
func TestRosterCacheMissFallsBackToRPC(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()
	w := startCountingWorker(t, "0.3.0")
	sup, h := adoptCountingWorker(t, root, w)

	// Force a cache miss the way a failed seed leaves one: no published snapshot.
	h.info.Store(nil)
	before := w.rosterCalls.Load()

	rows, err := sup.Roster(context.Background())
	if err != nil {
		t.Fatalf("Roster: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != w.id {
		t.Fatalf("Roster returned %+v, want the uncached session %s served by fallback RPC", rows, w.id)
	}
	if got := w.rosterCalls.Load(); got != before+1 {
		t.Errorf("cache miss issued %d gofer/roster calls, want exactly 1 fallback", got-before)
	}
}
