package router

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// TestCreateRefusesSessionIDMismatch is issue #144: the router pre-generates a
// session uuid and requires the worker to echo it back on session/new (design
// Option A, router.go's Create). Nothing previously exercised the branch that
// catches a worker echoing back a DIFFERENT id — the exact failure mode that
// would otherwise adopt a worker under the wrong identity and route a client's
// traffic into someone else's session.
//
// The faux worker here is spawned as a REAL child process, keyed correctly on
// disk by the router-pinned uuid (its socket/endpoint/lock — see
// fauxWorkerSeamOpts), but its session/new response echoes a different,
// unrelated uuid (SessionIDOverride) — reproducing a worker whose pinning
// bridge is broken while everything else about discovery/dial succeeds.
func TestCreateRefusesSessionIDMismatch(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()

	// A fixed, unrelated uuid the faux worker will claim as its session id
	// instead of whatever the router pins. It cannot collide with the router's
	// own fresh uuid.NewV7() draw.
	wrongID := uuid.Must(uuid.NewV7()).String()

	sup, err := New(Config{
		Root:         root,
		NewWorkerCmd: fauxWorkerSeamOpts(root, fauxWorkerOptions{SessionIDOverride: wrongID}),
	})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	before := endpointFileCount(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	info, err := sup.Create(ctx, "", supervisor.CreateOptions{})

	// The refusal itself: Create must fail rather than hand back a live
	// session, and its error must name the mismatch (not some unrelated
	// failure further down the same call).
	if err == nil {
		t.Fatalf("Create succeeded with info %+v despite a session id mismatch; want a refusal", info)
	}
	if !strings.Contains(err.Error(), "session id") || !strings.Contains(err.Error(), "!= pinned") {
		t.Fatalf("err = %q, want it to report the pinned/echoed session id mismatch", err)
	}
	// The error must report the id the worker actually echoed, confirming the
	// check compared against the WRONG id and not some other value.
	if !strings.Contains(err.Error(), wrongID) {
		t.Fatalf("err = %q, want it to mention the mismatched id %q", err, wrongID)
	}

	// No adoption occurred: neither the pinned id (info.ID may be empty on
	// failure, but the router must not have registered ANY handle) nor the
	// worker's claimed wrongID ended up live.
	if got := liveWorkerCount(sup); got != 0 {
		t.Fatalf("live worker count = %d after a refused Create, want 0 (no adoption)", got)
	}
	if _, ok := sup.get(wrongID); ok {
		t.Fatalf("a handle was registered under the worker's claimed id %q; the router adopted the wrong identity", wrongID)
	}

	// The refusal must not leak the worker's on-disk endpoint artifact — a
	// mismatch is a full teardown, not a partial one that a later adoption
	// scan could trip over.
	if got := endpointFileCount(t); got != before {
		t.Fatalf("endpoint files = %d after a refused Create, want %d (no leaked artifact)", got, before)
	}
}
