package router

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// subagent_test.go covers the M6 half of the parent/child session primitive: the
// router forwards a create's subagent link to the worker that actually hosts the
// session, and — the part that is easy to get wrong, because the router keeps its
// own offline-row builder — reports the link for sessions whose workers are gone.

// TestListOfflineSubagentLinkFromSidecar is the regression test for the router's
// parallel List. Under M6 the router IS the daemon a TUI or `gofer ps` talks to,
// so if its own diskSessionInfo skips the sidecar a subagent tree collapses into
// a flat list of roots the moment its workers exit — on the PRIMARY deployment
// path, while the in-process supervisor's List keeps working and hides it.
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

// findRouterInfo returns the row for id, or nil.
func findRouterInfo(rows []supervisor.SessionInfo, id string) *supervisor.SessionInfo {
	for i := range rows {
		if rows[i].ID == id {
			return &rows[i]
		}
	}
	return nil
}
