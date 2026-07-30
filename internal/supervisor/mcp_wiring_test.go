package supervisor_test

// mcp_wiring_test.go proves config.MCP actually reaches sessionGuard's real
// wiring (not just internal/mcpconn in isolation, which manager_internal_test.go
// already covers) — a configured, reachable server's tools land in a
// CREATED session's registry, AND a session created before a server finishes
// connecting never gains that server's tools afterward (the hard invariant,
// exercised here at the supervisor integration level). It uses a loopback
// httptest.Server as the "MCP server" — no subprocess, no external network.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/runner"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// startFakeMCPHTTPServer serves just enough MCP-over-HTTP (initialize,
// tools/list, tools/call) for a real agent-sdk-go/mcp.Client to connect
// through — refusing every request (503) while up reports false, so a test
// can flip a server from down to up mid-run.
func startFakeMCPHTTPServer(t *testing.T, up *atomic.Bool, toolName string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "initialize":
			resp["result"] = map[string]any{"serverInfo": map[string]string{"name": "fake", "version": "0"}}
		case "tools/list":
			resp["result"] = map[string]any{"tools": []map[string]any{
				{"name": toolName, "description": "a fake tool", "inputSchema": json.RawMessage(`{"type":"object"}`)},
			}}
		case "tools/call":
			resp["result"] = map[string]any{
				"content": []map[string]any{{"type": "text", "text": "ok"}},
				"isError": false,
			}
		default:
			resp["error"] = map[string]any{"code": -32601, "message": "method not found"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newMCPHarness is [newHarness] (helpers_test.go) with an explicit MCP
// resolver injected — newHarness itself builds a Config with none set, so it
// cannot exercise it.
func newMCPHarness(t *testing.T, mcpCfg config.MCP) *harness {
	t.Helper()
	h := &harness{t: t, root: t.TempDir(), sessions: make(map[string]*fakeSession)}

	var nextID int64
	cfg := supervisor.Config{
		Root: h.root,
		MCP:  func() config.MCP { return mcpCfg },
		NewSession: func(_ context.Context, opts runner.Options) (supervisor.Session, error) {
			id := "sess-" + strconv.FormatInt(atomic.AddInt64(&nextID, 1), 10)
			fs := h.register(id, opts.Cwd)
			fs.approver = opts.Approver
			fs.tools = opts.Tools
			return fs, nil
		},
	}

	sup, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	h.sup = sup
	t.Cleanup(func() { _ = sup.Close() })
	return h
}

// TestSessionGuard_MCPConfigWiresTools proves an enabled, reachable MCP
// server in config.MCP reaches sessionGuard: the tool it offers is resolvable
// out of a CREATED session's registry, qualified as mcp__<server>__<tool>.
func TestSessionGuard_MCPConfigWiresTools(t *testing.T) {
	up := &atomic.Bool{}
	up.Store(true)
	srv := startFakeMCPHTTPServer(t, up, "echo")

	readyMS := 2000
	mcpCfg := config.MCP{
		Servers:        []config.MCPServer{{Name: "fake", URL: srv.URL}},
		ReadyTimeoutMS: &readyMS,
	}
	h := newMCPHarness(t, mcpCfg)

	info, err := h.sup.Create(context.Background(), "", supervisor.CreateOptions{Cwd: t.TempDir(), Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(info.ID)
	if fs.tools == nil {
		t.Fatal("supervisor did not inject a tool registry")
	}
	if _, ok := fs.tools.Get("mcp__fake__echo"); !ok {
		specs := fs.tools.Specs()
		names := make([]string, len(specs))
		for i, s := range specs {
			names[i] = s.Name
		}
		t.Fatalf("mcp__fake__echo not registered; got tools: %v", names)
	}
}

// TestSessionGuard_MCPLateConnectDoesNotMutateLiveSession is the supervisor
// integration counterpart of internal/mcpconn's own invariant test: a
// session created while the ONLY configured server is still down (bounded by
// a short ReadyTimeout) must never gain that server's tools once it finishes
// connecting afterward — its registry is fixed forever at the snapshot
// sessionGuard captured at Create.
func TestSessionGuard_MCPLateConnectDoesNotMutateLiveSession(t *testing.T) {
	up := &atomic.Bool{} // starts false: down when the first session is created
	srv := startFakeMCPHTTPServer(t, up, "echo")

	readyMS := 20 // short: Create must not wait for the server
	mcpCfg := config.MCP{
		Servers:        []config.MCPServer{{Name: "late", URL: srv.URL}},
		ReadyTimeoutMS: &readyMS,
	}
	h := newMCPHarness(t, mcpCfg)

	first, err := h.sup.Create(context.Background(), "", supervisor.CreateOptions{Cwd: t.TempDir(), Model: "m"})
	if err != nil {
		t.Fatalf("Create (first): %v", err)
	}
	firstSpecs := h.session(first.ID).tools.Specs()
	for _, s := range firstSpecs {
		if s.Name == "mcp__late__echo" {
			t.Fatalf("first session already has mcp__late__echo — the server was supposed to be down at create")
		}
	}

	// Bring the server up and wait (via a SECOND session's create, which
	// re-checks readiness) until the manager has actually connected.
	up.Store(true)
	var second supervisor.SessionInfo
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		second, err = h.sup.Create(context.Background(), "", supervisor.CreateOptions{Cwd: t.TempDir(), Model: "m"})
		if err != nil {
			t.Fatalf("Create (second): %v", err)
		}
		if _, ok := h.session(second.ID).tools.Get("mcp__late__echo"); ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, ok := h.session(second.ID).tools.Get("mcp__late__echo"); !ok {
		t.Fatal("server never came up — cannot prove the invariant without a real late connect")
	}

	// The invariant: the FIRST session's registry — captured before the
	// server connected — must be untouched.
	firstToolsNow := h.session(first.ID).tools.Specs()
	if len(firstToolsNow) != len(firstSpecs) {
		t.Fatalf("first session's tool count changed from %d to %d after the server connected — a live session's tools must never grow",
			len(firstSpecs), len(firstToolsNow))
	}
	if _, ok := h.session(first.ID).tools.Get("mcp__late__echo"); ok {
		t.Fatal("first session gained mcp__late__echo after a late connect — the hard invariant is broken")
	}
}

// TestSessionGuard_MCPDownServerEmitsVisibleNotice proves a server that is
// down at session create still lets the session create (never fails it) and
// emits a visible, non-fatal session.error naming the server — the required
// "notice" a down server must produce.
func TestSessionGuard_MCPDownServerEmitsVisibleNotice(t *testing.T) {
	readyMS := 20
	mcpCfg := config.MCP{
		Servers:        []config.MCPServer{{Name: "ghost", URL: "http://127.0.0.1:1"}}, // nothing listens here
		ReadyTimeoutMS: &readyMS,
	}
	h := newMCPHarness(t, mcpCfg)

	info, err := h.sup.Create(context.Background(), "", supervisor.CreateOptions{Cwd: t.TempDir(), Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v (a down MCP server must never fail session create)", err)
	}

	sub, err := h.sup.Subscribe(context.Background(), info.ID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case e := <-sub.C:
			se, ok := e.(event.SessionError)
			if !ok {
				continue
			}
			if se.Fatal {
				t.Fatalf("session.error for a down MCP server was Fatal=true, want a non-fatal notice: %q", se.Err)
			}
			if !strings.Contains(se.Err, "ghost") {
				t.Fatalf("session.error = %q, want it to name the down server %q", se.Err, "ghost")
			}
			return
		case <-deadline:
			t.Fatal("no session.error notice observed for the down MCP server")
		}
	}
}

// TestSessionGuard_MCPReadyTimeoutReachesCreate is the guard for
// config.MCP.ReadyTimeout specifically. The manager-construction read
// (mcpconn.NewManager) is already covered by the down-server notice test, but
// the PER-SESSION ReadyTimeout read that bounds Create's best-effort wait had
// no test of its own: replacing it with a hardcoded zero config left the whole
// package green, so an operator setting mcp.ready_timeout_ms could have been
// silently ignored — the exact silent-disable class this milestone exists to
// remove.
//
// ReadyTimeout is a CEILING, not a floor: AwaitReady returns as soon as
// discovery settles, and a refused connection settles in microseconds. So the
// only way to observe the bound is a server that ACCEPTS the connection and
// then never answers — discovery cannot settle, and Create can only return by
// hitting the timeout. A short configured value must then bound Create well
// below the 2s package default, which is what a dropped config read resolves
// to.
func TestSessionGuard_MCPReadyTimeoutReachesCreate(t *testing.T) {
	// Accepts, then blocks until the test releases it: discovery can never
	// settle on its own. The release channel is closed BEFORE Close() because
	// httptest.Server.Close waits for in-flight handlers — blocking on the
	// request context instead would deadlock teardown against itself.
	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	t.Cleanup(func() {
		close(release)
		hang.Close()
	})

	// Far below config.DefaultMCPReadyTimeout (2s), so a dropped config read
	// resolves to that default and blows the ceiling.
	readyMS := 150
	h := newMCPHarness(t, config.MCP{
		Servers:        []config.MCPServer{{Name: "hang", URL: hang.URL}},
		ReadyTimeoutMS: &readyMS,
	})

	start := time.Now()
	if _, err := h.sup.Create(context.Background(), "", supervisor.CreateOptions{Cwd: t.TempDir(), Model: "m"}); err != nil {
		t.Fatalf("Create: %v (an unresponsive MCP server must never fail session create)", err)
	}
	elapsed := time.Since(start)

	// Ceiling sits between the configured 150ms and the 2s default, with
	// enough headroom that a slow machine cannot flip it.
	const ceiling = time.Second
	if elapsed > ceiling {
		t.Fatalf("Create took %s, want under %s — config.MCP.ReadyTimeout (%dms) did not reach the per-session wait; "+
			"a hardcoded default (%s) produces exactly this", elapsed, ceiling, readyMS, config.DefaultMCPReadyTimeout)
	}
}
