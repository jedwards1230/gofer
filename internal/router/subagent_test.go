package router

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/worker"
)

// subagent_test.go covers the M6 half of the parent/child session primitive: the
// router forwards a create's subagent link to the worker that actually hosts the
// session, and reports the link for sessions whose workers are gone.
//
// The router used to keep its own offline-row builder, which is what made the
// second half easy to get wrong. It now calls [supervisor.DiskSessionInfo], so
// this is a regression test against the duplicate ever coming back.

// TestListOfflineSubagentLinkFromSidecar is the regression test for the router's
// List. Under M6 the router IS the daemon a TUI or `gofer ps` talks to, so if its
// offline rows skip the sidecar a subagent tree collapses into a flat list of
// roots the moment its workers exit — on the PRIMARY deployment path, while the
// in-process supervisor's List keeps working and hides it.
//
// It is deliberately worker-free: journals and a sidecar are written straight to
// the store root, so the assertion is about the offline-row builder alone with no
// spawn timing in the way.
func TestListOfflineSubagentLinkFromSidecar(t *testing.T) {
	root := t.TempDir()
	const (
		slug     = "subagent-proj"
		parentID = "0192a1b2-0000-7000-8000-000000000001"
		childID  = "0192a1b2-0000-7000-8000-000000000002"
	)
	dir := filepath.Join(root, "sessions", slug)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	for _, id := range []string{parentID, childID} {
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), nil, 0o600); err != nil {
			t.Fatalf("write journal %s: %v", id, err)
		}
	}
	// Only the child has a sidecar — a root session writes none, by design.
	sidecar := []byte(`{"parentId":"` + parentID + `","agent":"go-developer","depth":1}`)
	if err := os.WriteFile(filepath.Join(dir, childID+".meta.json"), sidecar, 0o600); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}

	sup, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	rows, err := sup.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	tests := []struct {
		name       string
		id         string
		wantParent string
		wantAgent  string
		wantDepth  int
	}{
		{"offline child keeps its parent link", childID, parentID, "go-developer", 1},
		{"offline root stays a root", parentID, "", "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row := findRouterInfo(rows, tc.id)
			if row == nil {
				t.Fatalf("List missing %s: %+v", tc.id, rows)
			}
			if row.Live {
				t.Errorf("Live = true, want false (no worker)")
			}
			if row.ParentID != tc.wantParent || row.Agent != tc.wantAgent || row.Depth != tc.wantDepth {
				t.Errorf("row = {parent %q, agent %q, depth %d}, want {%q, %q, %d}",
					row.ParentID, row.Agent, row.Depth, tc.wantParent, tc.wantAgent, tc.wantDepth)
			}
		})
	}
}

// TestCreateSubagentThroughWorker drives the whole M6 chain for real: the router
// forwards the link on the worker's session/new `_meta`, the worker's daemon
// decodes it, the worker's supervisor resolves the parent against the SHARED
// store root and derives the depth, and the router reports back what the worker
// assigned. A swapped parent/agent argument or a dropped `_meta` fails here.
func TestCreateSubagentThroughWorker(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()
	cwd := t.TempDir()

	sup, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	t.Cleanup(func() {
		killWorkers(sup)
		_ = sup.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	parent, err := sup.Create(ctx, "", supervisor.CreateOptions{Cwd: cwd})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	if parent.ParentID != "" || parent.Depth != 0 {
		t.Fatalf("root create reported a link: %+v", parent)
	}

	child, err := sup.Create(ctx, "", supervisor.CreateOptions{
		Cwd: cwd, ParentID: parent.ID, Agent: "go-developer",
	})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	if child.ParentID != parent.ID {
		t.Errorf("ParentID = %q, want %q", child.ParentID, parent.ID)
	}
	if child.Agent != "go-developer" {
		t.Errorf("Agent = %q, want go-developer", child.Agent)
	}
	if child.Depth != 1 {
		t.Errorf("Depth = %d, want 1 (derived by the worker from the parent)", child.Depth)
	}

	// The worker persisted the link, so the router's List reports it too — live
	// here (via the roster cache seeded off the worker's own roster) rather than
	// from disk.
	rows, err := sup.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	row := findRouterInfo(rows, child.ID)
	if row == nil {
		t.Fatalf("List missing child %s: %+v", child.ID, rows)
	}
	if row.ParentID != parent.ID || row.Agent != "go-developer" || row.Depth != 1 {
		t.Errorf("live row = {parent %q, agent %q, depth %d}, want {%q, go-developer, 1}",
			row.ParentID, row.Agent, row.Depth, parent.ID)
	}
}

// TestBuildWorkerCmdCarriesTheRouterDialBack pins the router-side half of the
// D2 wiring: a worker is told where to dial back, and the bearer token reaches
// it through NEITHER argv NOR the environment.
//
// Both exclusions are real security properties, not style choices, and they
// have different threat models — which is why each is asserted separately:
//
//   - argv, because /proc/<pid>/cmdline is world-readable on Linux, so a token
//     on the command line publishes the daemon's RCE-equivalent credential to
//     every other local account.
//   - the ENVIRONMENT, because the AGENT reads it. The SDK's bash tool runs
//     exec.CommandContext with cmd.Env unset (agent-sdk-go/tool/bash.go), so a
//     tool call inherits the worker's whole environment and a model can print
//     the token with `env`. internal/sandbox scrubs nothing. This is the more
//     dangerous of the two: the agent is precisely the party the worker
//     boundary exists to contain.
func TestBuildWorkerCmdCarriesTheRouterDialBack(t *testing.T) {
	tests := []struct {
		name      string
		addr      string
		token     string
		wantArgs  bool
		wantToken bool
	}{
		{"no router configured", "", "", false, false},
		{"loopback router, no token", "127.0.0.1:7333", "", true, false},
		{"router with a token", "127.0.0.1:7333", "s3cret", true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			shortRuntimeDir(t)
			s, err := New(Config{
				Root:        t.TempDir(),
				SelfExe:     "/usr/local/bin/gofer",
				RouterAddr:  tc.addr,
				RouterToken: tc.token,
			})
			if err != nil {
				t.Fatalf("router.New: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })

			cmd := s.buildWorkerCmd(context.Background(), "sess-uuid", "faux", t.TempDir())
			hasRouter := slices.Contains(cmd.Args, "--router")
			if hasRouter != tc.wantArgs {
				t.Fatalf("argv %v carries --router = %v, want %v", cmd.Args, hasRouter, tc.wantArgs)
			}
			if tc.wantArgs && !slices.Contains(cmd.Args, tc.addr) {
				t.Errorf("argv %v does not carry the router addr %q", cmd.Args, tc.addr)
			}

			if tc.token != "" {
				for _, arg := range cmd.Args {
					if strings.Contains(arg, tc.token) {
						t.Fatalf("the bearer token leaked into argv (%v) — /proc/<pid>/cmdline is world-readable", cmd.Args)
					}
				}
				for _, kv := range cmd.Env {
					if strings.Contains(kv, tc.token) {
						t.Fatalf("the bearer token leaked into the worker's environment — the agent's own bash tool inherits it and can print it with `env`")
					}
				}
			}
			// A nil Env means "inherit the router's, unchanged", which is what
			// keeps this feature from touching a worker's environment at all.
			if cmd.Env != nil {
				t.Errorf("buildWorkerCmd set cmd.Env (%v); it must stay nil so the worker's environment is untouched", cmd.Env)
			}

			gotFlag := slices.Contains(cmd.Args, "--router-token-file")
			if gotFlag != tc.wantToken {
				t.Fatalf("argv %v carries --router-token-file = %v, want %v", cmd.Args, gotFlag, tc.wantToken)
			}
		})
	}
}

// TestWriteWorkerRouterTokenIsOwnerOnly pins the file half of the hand-off: the
// credential lands at mode 0600 and carries the token verbatim, and no file is
// created at all when there is no token to hand over.
func TestWriteWorkerRouterTokenIsOwnerOnly(t *testing.T) {
	shortRuntimeDir(t)
	dir, err := daemon.WorkersDir()
	if err != nil {
		t.Fatalf("WorkersDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	t.Run("no token writes no file", func(t *testing.T) {
		if err := writeWorkerRouterToken("no-token-sess", ""); err != nil {
			t.Fatalf("writeWorkerRouterToken: %v", err)
		}
		path, _ := WorkerRouterTokenPath("no-token-sess")
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a tokenless router wrote a credential file (stat err = %v)", err)
		}
	})

	t.Run("token file is 0600 and exact", func(t *testing.T) {
		const tok = "s3cret"
		if err := writeWorkerRouterToken("tok-sess", tok); err != nil {
			t.Fatalf("writeWorkerRouterToken: %v", err)
		}
		path, _ := WorkerRouterTokenPath("tok-sess")
		t.Cleanup(func() { _ = os.Remove(path) })
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat token file: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("token file mode = %o, want 0600", perm)
		}
		b, err := os.ReadFile(path) // #nosec G304 -- test-owned temp path
		if err != nil {
			t.Fatalf("read token file: %v", err)
		}
		if string(b) != tok {
			t.Errorf("token file = %q, want %q", b, tok)
		}
	})

	t.Run("removeWorkerArtifacts sweeps an unread token", func(t *testing.T) {
		if err := writeWorkerRouterToken("swept-sess", "s3cret"); err != nil {
			t.Fatalf("writeWorkerRouterToken: %v", err)
		}
		removeWorkerArtifacts("swept-sess")
		path, _ := WorkerRouterTokenPath("swept-sess")
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a worker's unread credential survived artifact cleanup (stat err = %v)", err)
		}
	})
}

// TestWorkerSpawnRoutesThroughRouter is the D2 decision, end to end: a spawn
// ORIGINATING IN A WORKER results in the ROUTER creating the child, in its own
// worker process, with the parent link the worker asked for. A worker never
// creates a session itself — its embedded daemon is capped at one — so this is
// the only path that can exist.
//
// It drives the real [worker.RouterSubagents] against a real router-hosted
// daemon over a TOKEN-REQUIRED listener. The token half matters: the loopback
// default masks auth in dev, so a dial-back that forgot to forward the token
// would pass every other test in this package and fail only on a hardened
// deployment.
func TestWorkerSpawnRoutesThroughRouter(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()
	cwd := t.TempDir()
	const token = "router-token"

	sup, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	t.Cleanup(func() {
		killWorkers(sup)
		_ = sup.Close()
	})

	d := daemon.New(sup, daemon.Config{DefaultModel: "faux", BearerToken: token})
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	parent, err := sup.Create(ctx, "", supervisor.CreateOptions{Cwd: cwd})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}

	// The seam exactly as `gofer session-worker --router <addr>` builds it. sup
	// is nil here: this test drives the seam from OUTSIDE a worker process, so
	// there is no local roster to inherit the parent's model/cwd from — which is
	// itself worth exercising, since that fallback must never block a spawn.
	seam := worker.NewRouterSubagents(addr, token, nil, nil)
	t.Cleanup(func() { _ = seam.Close() })

	childID, err := seam.Spawn(ctx, parent.ID, "go-developer", "investigate the flaky build")
	if err != nil {
		t.Fatalf("worker spawn through the router: %v", err)
	}
	if childID == "" || childID == parent.ID {
		t.Fatalf("spawn returned child id %q", childID)
	}

	// The ROUTER created it — it is one of the router's live workers, with the
	// link the worker's seam asked for and a depth the CHILD's own worker
	// derived from the shared store.
	rows, err := sup.Roster(ctx)
	if err != nil {
		t.Fatalf("Roster: %v", err)
	}
	child := findRouterInfo(rows, childID)
	if child == nil {
		t.Fatalf("the router has no live worker for the spawned child %s: %+v", childID, rows)
	}
	if child.ParentID != parent.ID {
		t.Errorf("child ParentID = %q, want %q", child.ParentID, parent.ID)
	}
	if child.Agent != "go-developer" {
		t.Errorf("child Agent = %q, want go-developer", child.Agent)
	}
	if child.Depth != 1 {
		t.Errorf("child Depth = %d, want 1", child.Depth)
	}

	// And the REPORT half of the same seam: a finished child's result routed
	// back to the parent's worker as its next prompt.
	if err := seam.Report(ctx, parent.ID, "subagent go-developer finished: the flake is a shared temp dir"); err != nil {
		t.Fatalf("worker report through the router: %v", err)
	}
	waitForHistoryContaining(t, ctx, sup, parent.ID, "the flake is a shared temp dir")
}

// TestWorkerSpawnWithoutTokenIsRefused is the negative of the auth half: the
// same dial-back with no token must fail rather than silently create a session,
// so a misconfigured worker is loud instead of mysteriously idle.
func TestWorkerSpawnWithoutTokenIsRefused(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()

	sup, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	d := daemon.New(sup, daemon.Config{DefaultModel: "faux", BearerToken: "router-token"})
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)

	seam := worker.NewRouterSubagents(strings.TrimPrefix(srv.URL, "http://"), "", nil, nil)
	t.Cleanup(func() { _ = seam.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := seam.Spawn(ctx, "some-parent", "go-developer", "go"); err == nil {
		t.Fatal("an unauthenticated dial-back spawned a session")
	}
}

// waitForHistoryContaining polls a session's folded history until it carries
// want. The report rides a fire-and-forget session/prompt (see
// RouterSubagents.firePrompt), so there is nothing to await synchronously —
// which is the point: a report must never block the reporting child's pump.
func waitForHistoryContaining(t *testing.T, ctx context.Context, s *Supervisor, id, want string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		msgs, err := s.History(ctx, id)
		if err == nil && messagesContain(msgs, want) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("session %s never received a prompt containing %q", id, want)
}

func messagesContain(msgs []provider.Message, want string) bool {
	for _, m := range msgs {
		if strings.Contains(m.Text(), want) {
			return true
		}
	}
	return false
}

// findRouterInfo returns the row for id, or nil.
func findRouterInfo(rows []supervisor.SessionInfo, id string) *supervisor.SessionInfo {
	for i := range rows {
		if rows[i].ID == id {
			return &rows[i]
		}
	}
	return nil
}
