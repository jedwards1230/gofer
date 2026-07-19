package supervisor_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// TestCreateRegisterRejectsAfterClose exercises the Create-vs-Close race
// deterministically: a NewSession seam closes the supervisor mid-construction,
// so Close lands AFTER Create's entry isClosed() check but BEFORE register.
// register must reject the insert (ErrClosed) and Create must tear the
// just-built session down — no roster entry, no leaked pump goroutine.
func TestCreateRegisterRejectsAfterClose(t *testing.T) {
	var sup *supervisor.Supervisor
	var built *fakeSession
	closedOnce := false

	cfg := supervisor.Config{
		Root: t.TempDir(),
		NewSession: func(_ context.Context, opts runner.Options) (supervisor.Session, error) {
			built = newFakeSession("sess-race", filepath.Join(opts.Cwd, "sess-race.jsonl"))
			if !closedOnce {
				closedOnce = true
				// Close lands during construction — after Create's isClosed()
				// check, before register.
				_ = sup.Close()
			}
			return built, nil
		},
	}

	var err error
	sup, err = supervisor.New(cfg)
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}

	_, cerr := sup.Create(context.Background(), "hi", supervisor.CreateOptions{Cwd: t.TempDir(), Model: "m"})
	if !errors.Is(cerr, supervisor.ErrClosed) {
		t.Fatalf("Create racing Close: want ErrClosed, got %v", cerr)
	}

	// The built session must have been torn down by Create's cleanup, so no
	// broker/journal leaks and (since register never ran) no pump goroutine.
	built.mu.Lock()
	wasClosed := built.closed
	built.mu.Unlock()
	if !wasClosed {
		t.Fatal("Create must Close the session it built when register rejects")
	}

	roster, rerr := sup.Roster(context.Background())
	if rerr != nil {
		t.Fatalf("Roster: %v", rerr)
	}
	if len(roster) != 0 {
		t.Fatalf("roster must be empty after a rejected create, got %d entries", len(roster))
	}
}

// TestOpsAfterCloseRejected confirms the user-facing contract: Create and
// Resume both fail closed with ErrClosed once the supervisor is closed.
func TestOpsAfterCloseRejected(t *testing.T) {
	h := newHarness(t) // t.Cleanup already closes sup; a second Close is idempotent.
	if err := h.sup.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := h.sup.Create(context.Background(), "hi", supervisor.CreateOptions{Cwd: "/tmp", Model: "m"}); !errors.Is(err, supervisor.ErrClosed) {
		t.Fatalf("Create after Close: want ErrClosed, got %v", err)
	}
	if _, err := h.sup.Resume(context.Background(), "sess-x", supervisor.ResumeOptions{Model: "m", Cwd: "/tmp"}); !errors.Is(err, supervisor.ErrClosed) {
		t.Fatalf("Resume after Close: want ErrClosed, got %v", err)
	}
}
