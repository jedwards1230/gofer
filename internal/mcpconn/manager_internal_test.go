package mcpconn

// manager_internal_test.go covers the Manager's behavioral contract end to
// end against the hermetic fakeServer (fake_server_internal_test.go) — no
// real MCP server, no subprocess, no network. dial_leak_test.go separately
// proves the REAL stdio Dialer never leaks a subprocess.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/mcp"

	"github.com/jedwards1230/gofer/internal/config"
)

// fakeCluster is a test [Dialer]: it hands back a live in-memory client for
// any server whose name it currently considers "up", and an error
// otherwise — the whole point being that a test can flip a server down
// (simulating "never comes up" or "died and hasn't reconnected") and back up
// (simulating a real server coming back) between Manager connect attempts,
// with no subprocess or network involved either way.
type fakeCluster struct {
	mu    sync.Mutex
	up    map[string]bool
	tools map[string][]fakeTool
	pairs []*pipePair // every pipePair ever started, for cleanup
}

func newFakeCluster() *fakeCluster {
	return &fakeCluster{up: make(map[string]bool), tools: make(map[string][]fakeTool)}
}

func (fc *fakeCluster) setUp(name string, tools []fakeTool, up bool) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.up[name] = up
	fc.tools[name] = tools
}

func (fc *fakeCluster) dial(ctx context.Context, srv config.MCPServer, connectTimeout, callTimeout time.Duration) (*mcp.Client, error) {
	fc.mu.Lock()
	up := fc.up[srv.Name]
	tools := fc.tools[srv.Name]
	fc.mu.Unlock()
	if !up {
		return nil, fmt.Errorf("fake cluster: server %q is down", srv.Name)
	}
	client, pp, err := startFakeClient(ctx, tools, mcp.WithConnectTimeout(connectTimeout), mcp.WithCallTimeout(callTimeout))
	if err != nil {
		return nil, err
	}
	fc.mu.Lock()
	fc.pairs = append(fc.pairs, pp)
	fc.mu.Unlock()
	return client, nil
}

func (fc *fakeCluster) killAll() {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	for _, pp := range fc.pairs {
		pp.kill()
	}
}

// echoTool is a fakeTool that reports the JSON args it was called with, so a
// test can assert a call actually reached the server.
func echoTool(name string) fakeTool {
	return fakeTool{
		name:   name,
		desc:   "echoes its input",
		schema: `{"type":"object"}`,
		call: func(args json.RawMessage) (string, bool) {
			return "echo:" + string(args), false
		},
	}
}

func newTestManager(t *testing.T, cfg config.MCP, dial Dialer) *Manager {
	t.Helper()
	m := NewManager(Config{MCP: cfg, Dialer: dial})
	m.healthInterval = 20 * time.Millisecond // fast enough for a death to be detected quickly in tests
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// TestManager_NeverComesUp proves a server that never connects: contributes
// no tools, is reported in Snapshot().Down, and never blocks Start or a
// bounded AwaitReady beyond that bound.
func TestManager_NeverComesUp(t *testing.T) {
	fc := newFakeCluster() // "down" is the zero value — never brought up
	cfg := config.MCP{Servers: []config.MCPServer{{Name: "ghost", Command: "fake"}}}
	m := newTestManager(t, cfg, fc.dial)

	m.Start(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m.AwaitReady(ctx) // must return once the one failed attempt lands, not wait out ctx

	snap := m.Snapshot()
	if len(snap.Tools) != 0 {
		t.Fatalf("Snapshot().Tools = %d, want 0 for a server that never connected", len(snap.Tools))
	}
	if len(snap.Down) != 1 || snap.Down[0] != "ghost" {
		t.Fatalf("Snapshot().Down = %v, want [ghost]", snap.Down)
	}
}

// TestManager_AwaitReady_BoundsOnCallerCtx proves the ready wait is bounded
// by the CALLER's ctx, not by however long the server actually takes to
// connect — the "session create must never block on a server" requirement.
func TestManager_AwaitReady_BoundsOnCallerCtx(t *testing.T) {
	fc := newFakeCluster()
	block := make(chan struct{})
	slowDial := func(ctx context.Context, srv config.MCPServer, connectTimeout, callTimeout time.Duration) (*mcp.Client, error) {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return fc.dial(ctx, srv, connectTimeout, callTimeout)
	}
	cfg := config.MCP{Servers: []config.MCPServer{{Name: "slow", Command: "fake"}}}
	fc.setUp("slow", []fakeTool{echoTool("t")}, true)
	m := newTestManager(t, cfg, slowDial)
	m.Start(context.Background())

	start := time.Now()
	readyCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	m.AwaitReady(readyCtx)
	elapsed := time.Since(start)

	if elapsed > 500*time.Millisecond {
		t.Fatalf("AwaitReady blocked for %s, want bounded near the 50ms caller ctx", elapsed)
	}
	snap := m.Snapshot()
	if len(snap.Tools) != 0 {
		t.Fatalf("Snapshot().Tools = %d before the slow dial unblocked, want 0", len(snap.Tools))
	}

	// Unblock the dial; the server eventually settles, proving AwaitReady's
	// bound was about THIS call, not that the manager gave up on the server.
	close(block)
	for i := 0; i < 100 && len(m.Snapshot().Tools) == 0; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if len(m.Snapshot().Tools) == 0 {
		t.Fatal("server never connected after unblocking dial")
	}
}

// TestManager_LateConnectDoesNotMutateLiveSnapshot is THE hard-invariant
// test: a Snapshot taken while a server is still down (what a session's
// sessionGuard would register) must never be altered once that server
// finishes connecting afterward — a session's tool set is fixed at create.
func TestManager_LateConnectDoesNotMutateLiveSnapshot(t *testing.T) {
	fc := newFakeCluster() // starts down
	cfg := config.MCP{Servers: []config.MCPServer{{Name: "late", Command: "fake"}}}
	m := newTestManager(t, cfg, fc.dial)
	m.Start(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	m.AwaitReady(ctx) // settles quickly: the one attempt fails, server still down

	// This is what a live session's sessionGuard would have registered.
	liveSessionSnapshot := m.Snapshot()
	if len(liveSessionSnapshot.Tools) != 0 {
		t.Fatalf("liveSessionSnapshot.Tools = %d, want 0 (server was down at create)", len(liveSessionSnapshot.Tools))
	}

	// The server connects AFTER the "session" above already captured its
	// snapshot — mirroring a server finishing initialization mid-run.
	fc.setUp("late", []fakeTool{echoTool("t")}, true)
	for i := 0; i < 600 && len(m.Snapshot().Tools) == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if len(m.Snapshot().Tools) == 0 {
		t.Fatal("server never came up — cannot prove the invariant without a real late connect")
	}

	// The invariant: the ALREADY-RETURNED snapshot from before must still be
	// exactly what it was — nothing in Manager may reach back and grow it.
	if len(liveSessionSnapshot.Tools) != 0 {
		t.Fatalf("liveSessionSnapshot.Tools mutated to %d after a late connect — a live session's tool set must never grow", len(liveSessionSnapshot.Tools))
	}
	if len(liveSessionSnapshot.Down) != 1 || liveSessionSnapshot.Down[0] != "late" {
		t.Fatalf("liveSessionSnapshot.Down mutated to %v after a late connect", liveSessionSnapshot.Down)
	}

	// A NEW snapshot (what the NEXT session would register) correctly sees it.
	next := m.Snapshot()
	if len(next.Tools) != 1 {
		t.Fatalf("a fresh Snapshot() after the late connect has %d tools, want 1 (the next session should see it)", len(next.Tools))
	}
}

// TestManager_DiesMidSession_ReconnectsAndToolsWork proves a registered
// proxyTool degrades to IsError while its server is down and starts working
// again, with NO re-registration, once the Manager reconnects — the exact
// tool.Tool object a session held onto the whole time.
func TestManager_DiesMidSession_ReconnectsAndToolsWork(t *testing.T) {
	fc := newFakeCluster()
	fc.setUp("flaky", []fakeTool{echoTool("ping")}, true)
	cfg := config.MCP{
		Servers: []config.MCPServer{{
			Name: "flaky", Command: "fake",
			CallTimeoutMS: intPtr(200),
		}},
	}
	m := newTestManager(t, cfg, fc.dial)
	m.Start(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m.AwaitReady(ctx)

	snap := m.Snapshot()
	if len(snap.Tools) != 1 {
		t.Fatalf("Snapshot().Tools = %d, want 1", len(snap.Tools))
	}
	// This is the SAME tool.Tool object the whole test uses — never swapped
	// out, mirroring a session's fixed registry entry.
	registered := snap.Tools[0]
	if got := registered.Name(); !strings.Contains(got, "flaky") || !strings.Contains(got, "ping") {
		t.Fatalf("registered.Name() = %q, want it to name server+tool", got)
	}

	res, err := registered.Run(context.Background(), json.RawMessage(`{"x":1}`))
	if err != nil {
		t.Fatalf("Run() while healthy: %v", err)
	}
	if res.IsError || !strings.Contains(res.Content, "echo:") {
		t.Fatalf("Run() while healthy = %+v, want a successful echo", res)
	}

	// Kill the connection. The health check (every 20ms in this test) will
	// notice within a couple of ticks and mark the server down.
	fc.killAll()
	var deadRes struct{ ok bool }
	for i := 0; i < 200; i++ {
		r, _ := registered.Run(context.Background(), json.RawMessage(`{}`))
		if r.IsError {
			deadRes.ok = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !deadRes.ok {
		t.Fatal("registered.Run() never started reporting IsError after the server died")
	}

	// Bring the server back "up" for the NEXT dial attempt (a fresh fake
	// server instance, exactly like a real restarted process).
	fc.setUp("flaky", []fakeTool{echoTool("ping")}, true)

	var recovered bool
	for i := 0; i < 600; i++ {
		r, _ := registered.Run(context.Background(), json.RawMessage(`{"x":2}`))
		if !r.IsError && strings.Contains(r.Content, "echo:") {
			recovered = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !recovered {
		t.Fatal("the SAME registered tool never started succeeding again after reconnect")
	}
}

// TestManager_AllowDenyGlobs proves per-server Allow/Deny actually filters
// which tools a server contributes to Snapshot(), by the tool's ORIGINAL
// (server-local) name.
func TestManager_AllowDenyGlobs(t *testing.T) {
	fc := newFakeCluster()
	tools := []fakeTool{echoTool("read_page"), echoTool("write_page"), echoTool("delete_page")}
	fc.setUp("wiki", tools, true)
	cfg := config.MCP{
		Servers: []config.MCPServer{{
			Name: "wiki", Command: "fake",
			Allow: []string{"read_*", "write_*"},
			Deny:  []string{"write_*"},
		}},
	}
	m := newTestManager(t, cfg, fc.dial)
	m.Start(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m.AwaitReady(ctx)

	snap := m.Snapshot()
	if len(snap.Tools) != 1 {
		names := make([]string, len(snap.Tools))
		for i, tl := range snap.Tools {
			names[i] = tl.Name()
		}
		t.Fatalf("Snapshot().Tools = %v, want exactly the read_page tool (write_* allowed then denied, delete_page never allowed)", names)
	}
	if !strings.Contains(snap.Tools[0].Name(), "read_page") {
		t.Fatalf("Snapshot().Tools[0].Name() = %q, want it to name read_page", snap.Tools[0].Name())
	}
}

// TestManager_Close_NoLeakedGoroutines proves Close stops every per-server
// goroutine and closes every live client — Manager's half of "no leaked
// resources" (dial_leak_test.go proves the subprocess half for the real
// Dialer).
func TestManager_Close_NoLeakedGoroutines(t *testing.T) {
	fc := newFakeCluster()
	fc.setUp("a", []fakeTool{echoTool("t")}, true)
	cfg := config.MCP{Servers: []config.MCPServer{{Name: "a", Command: "fake"}, {Name: "b", Command: "fake"}}}
	m := NewManager(Config{MCP: cfg, Dialer: fc.dial})
	m.healthInterval = 20 * time.Millisecond
	m.Start(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m.AwaitReady(ctx)

	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// wg.Wait() inside Close already proves every runServer goroutine
	// exited; a second Close must stay a no-op (idempotent) rather than
	// panicking on a nil cancel or double-closing a channel.
	if err := m.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func intPtr(v int) *int { return &v }
