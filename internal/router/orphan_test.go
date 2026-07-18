package router

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// TestOrphanEndpointAndLockDoNotBlockFreshCreate nails the intermediate-state
// window this slice introduces: after a router restart, a prior router's
// DETACHED workers are orphaned-but-alive — each still holding its <uuid>.lock
// and <uuid>.json endpoint — and this slice does not yet re-adopt them. The test
// proves those orphans are benign to a fresh router:
//
//   - [Supervisor.Create] draws a FRESH uuid, so it never touches (nor wedges on)
//     an orphan's held lock — the lock only blocks a duplicate worker for the
//     SAME session id.
//   - [Supervisor.List] reads live handles ∪ on-disk journals only, NOT endpoint
//     files, so an orphan's inert endpoint is never surfaced as a phantom live
//     session.
func TestOrphanEndpointAndLockDoNotBlockFreshCreate(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()

	// Simulate an orphan from a prior router: hold its <uuid>.lock and leave its
	// <uuid>.json endpoint on disk, for a DIFFERENT session uuid.
	orphanID := uuid.Must(uuid.NewV7()).String()
	release, err := daemon.LockWorker(orphanID)
	if err != nil {
		t.Fatalf("LockWorker(orphan %s): %v", orphanID, err)
	}
	t.Cleanup(func() { _ = release() })
	if err := daemon.WriteWorkerEndpoint(orphanID, daemon.WorkerEndpoint{
		Addr:          "unix:///nonexistent/" + orphanID + ".sock",
		PID:           os.Getpid(),
		BinaryVersion: "orphan",
		WireVersion:   daemon.WireVersion,
		StartedAt:     time.Now(),
	}); err != nil {
		t.Fatalf("WriteWorkerEndpoint(orphan): %v", err)
	}

	sup, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	t.Cleanup(func() {
		killWorkers(sup)
		_ = sup.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// A fresh Create succeeds despite the held orphan lock/endpoint on disk.
	info, err := sup.Create(ctx, "", supervisor.CreateOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Create with an orphan lock+endpoint present: %v", err)
	}
	if info.ID == orphanID {
		t.Fatalf("fresh Create reused the orphan uuid %q", orphanID)
	}

	// List surfaces the fresh live session but NOT the orphan — proof List does
	// not read endpoint files in this slice.
	rows, err := sup.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var sawNew, sawOrphan bool
	for _, r := range rows {
		switch r.ID {
		case info.ID:
			sawNew = true
		case orphanID:
			sawOrphan = true
		}
	}
	if !sawNew {
		t.Errorf("List missing the fresh session %q", info.ID)
	}
	if sawOrphan {
		t.Errorf("List surfaced orphan %q from its inert endpoint file", orphanID)
	}
}
