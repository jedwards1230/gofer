package supervisor_test

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/runner"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// TestOnRegister_InvokedOnceBeforeReachableTeardownJoinedOnKill exercises
// Config.OnRegister end to end: it must be invoked exactly once, with the
// live session, before Create returns (i.e. before the session is reachable
// for a subsequent Kill/Archive to race against); its teardown must not run
// until the session actually stops; and Kill must not return until teardown
// has completed (mirroring the permDone join discipline).
func TestOnRegister_InvokedOnceBeforeReachableTeardownJoinedOnKill(t *testing.T) {
	root := t.TempDir()

	var registered atomic.Int64
	var torndown atomic.Int64
	var mu sync.Mutex
	var gotID string

	cfg := supervisor.Config{
		Root: root,
		NewSession: func(_ context.Context, opts runner.Options) (supervisor.Session, error) {
			return newFakeSession("sess-1", filepath.Join(root, "sessions", "proj", "sess-1.jsonl")), nil
		},
		OnRegister: func(sess supervisor.Session) func() {
			registered.Add(1)
			mu.Lock()
			gotID = sess.ID()
			mu.Unlock()
			return func() { torndown.Add(1) }
		},
	}
	sup, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	info, err := sup.Create(context.Background(), "", supervisor.CreateOptions{Cwd: root, Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Called exactly once, synchronously within Create, before it returns.
	if got := registered.Load(); got != 1 {
		t.Fatalf("OnRegister invoked %d times by the time Create returned, want 1", got)
	}
	mu.Lock()
	gotIDSnapshot := gotID
	mu.Unlock()
	if gotIDSnapshot != info.ID {
		t.Errorf("OnRegister saw sess.ID() = %q, want %q", gotIDSnapshot, info.ID)
	}
	if got := torndown.Load(); got != 0 {
		t.Fatalf("teardown invoked before the session stopped: %d", got)
	}

	if err := sup.Kill(context.Background(), info.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	// Kill's stop() joins the teardown before returning (mirrors permDone) —
	// no polling needed, the count must already be 1.
	if got := torndown.Load(); got != 1 {
		t.Errorf("teardown invoked %d times after Kill returned, want 1", got)
	}
}

// TestOnRegister_Nil asserts the supervisor stays fully buildable and
// operable with Config.OnRegister unset — the hook is strictly optional.
func TestOnRegister_Nil(t *testing.T) {
	root := t.TempDir()
	cfg := supervisor.Config{
		Root: root,
		NewSession: func(_ context.Context, opts runner.Options) (supervisor.Session, error) {
			return newFakeSession("sess-1", filepath.Join(root, "sessions", "proj", "sess-1.jsonl")), nil
		},
	}
	sup, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	info, err := sup.Create(context.Background(), "", supervisor.CreateOptions{Cwd: root, Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := sup.Kill(context.Background(), info.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}
}
