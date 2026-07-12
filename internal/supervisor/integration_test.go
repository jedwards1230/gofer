package supervisor_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider/faux"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/runner"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// TestSupervisor_Integration_RealRunner drives a supervisor whose sessions
// are real *runner.Runner instances (not the fakeSession used elsewhere in
// this package) over a scripted faux provider — no network, fully
// deterministic. It proves the supervisor's Create/Submit/Kill path works
// against the real runner.NewSession/Options.Store wiring end to end, and
// that Kill never deletes the on-disk journal.
func TestSupervisor_Integration_RealRunner(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()

	store, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		t.Fatalf("session.NewFileStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	sup, err := supervisor.New(supervisor.Config{
		Root:  root,
		Store: store,
		NewSession: func(ctx context.Context, opts runner.Options) (supervisor.Session, error) {
			opts.Store = store
			opts.Provider = faux.New(faux.Default())
			return runner.NewSession(ctx, opts)
		},
	})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	defer func() { _ = sup.Close() }()

	ctx := context.Background()
	entry, err := sup.Create(ctx, "", supervisor.CreateOptions{
		Cwd: cwd, Model: "faux-1", System: "test system",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if entry.Status != supervisor.StatusNeedsInput {
		t.Fatalf("initial status = %s, want needs-input", entry.Status)
	}
	if _, err := os.Stat(entry.JournalPath); err != nil {
		t.Fatalf("journal not created on disk: %v", err)
	}

	sub, err := sup.Subscribe(ctx, entry.ID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err := sup.Send(ctx, entry.ID, "hello"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Drain events until the scripted turn settles (turn.finished), with a
	// bound so a regression hangs the test instead of the suite.
	waitForTurnFinished(t, sub)
	waitForStatus(t, sup, entry.ID, supervisor.StatusNeedsInput)

	r, err := sup.Roster(ctx)
	if err != nil {
		t.Fatalf("Roster: %v", err)
	}
	if len(r) != 1 || r[0].ID != entry.ID || r[0].Status != supervisor.StatusNeedsInput {
		t.Fatalf("Roster after completed turn = %+v, want one needs-input entry for %s", r, entry.ID)
	}

	if err := sup.Kill(ctx, entry.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if _, err := os.Stat(entry.JournalPath); err != nil {
		t.Fatalf("journal file missing after Kill: %v", err)
	}
	if r, err := sup.Roster(ctx); err != nil || len(r) != 0 {
		t.Fatalf("Roster after Kill = %+v, %v, want empty", r, err)
	}
}

// waitForTurnFinished drains sub until it observes a turn.finished event or
// the bound elapses.
func waitForTurnFinished(t *testing.T, sub *event.Subscription) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e, ok := <-sub.C:
			if !ok {
				t.Fatal("subscription closed before turn.finished")
			}
			if _, ok := e.(event.TurnFinished); ok {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for turn.finished")
		}
	}
}
