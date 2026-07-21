package daemonbridge_test

// decision_stub_test.go pins the PR-1 daemon-path stub for #173: structured
// decisions need a daemon relay that does not exist yet, so the bridge must
// fail honestly (never fake success) while keeping the TUI's decision pump a
// harmless no-op on a daemon-backed attach.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jedwards1230/gofer/internal/daemonbridge"
)

func TestDecisionsReturnsAClosedSubscription(t *testing.T) {
	var s daemonbridge.Supervisor

	sub, err := s.Decisions(context.Background(), "sess-1")
	if err != nil {
		t.Fatalf("Decisions: %v", err)
	}
	select {
	case _, ok := <-sub.C:
		if ok {
			t.Fatal("stub subscription delivered an update, want a closed channel")
		}
	case <-time.After(time.Second):
		t.Fatal("stub subscription is open — the TUI pump would block on it forever")
	}
	sub.Close() // idempotent: closing an already-closed stub must not panic
}

func TestDecisionsHonorsContext(t *testing.T) {
	var s daemonbridge.Supervisor
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.Decisions(ctx, "sess-1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Decisions(cancelled) = %v, want context.Canceled", err)
	}
}

func TestAnswerDecisionIsUnsupported(t *testing.T) {
	var s daemonbridge.Supervisor

	err := s.AnswerDecision(context.Background(), "sess-1", "dec-1", nil)

	if !errors.Is(err, daemonbridge.ErrDecisionsUnsupported) {
		t.Fatalf("AnswerDecision = %v, want ErrDecisionsUnsupported", err)
	}
}
