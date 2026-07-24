package tuibridge_test

// decision_test.go covers the Adapter's structured-decision pass-through: the
// in-process TUI path shares memory with the supervisor, so both methods are
// thin forwards. Their job here is narrower than internal/decision's own gate
// tests — prove they reach a LIVE session's gate (and surface its errors)
// rather than re-litigating the gate's semantics.

import (
	"context"
	"errors"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/acp"

	"github.com/jedwards1230/gofer/internal/decision"
	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tuibridge"
)

// TestAdapterDecisionsSubscribesToALiveSession covers the only thing this
// pass-through can get wrong that its own error path does not already cover:
// subscribing a session that exists must succeed. The routing property — that
// the subscription is the session's OWN gate — is proven end to end in
// internal/supervisor's decisions_test.go, against a real blocked ask_user
// call; re-asserting it here off a fresh session could only be done with checks
// that cannot fail.
func TestAdapterDecisionsSubscribesToALiveSession(t *testing.T) {
	sup := newTestSupervisor(t)
	a := tuibridge.New(sup, fixedModel("faux"))
	ctx := context.Background()

	info, err := a.Create(ctx, "", tui.CreateOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	sub, err := a.Decisions(ctx, info.ID)
	if err != nil {
		t.Fatalf("Decisions: %v", err)
	}
	sub.Close()
}

func TestAdapterAnswerDecisionSurfacesGateErrors(t *testing.T) {
	sup := newTestSupervisor(t)
	a := tuibridge.New(sup, fixedModel("faux"))
	ctx := context.Background()

	info, err := a.Create(ctx, "", tui.CreateOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Nothing is open, so the gate rejects — and the in-process path passes the
	// real sentinel through unwrapped (errors.Is works), unlike a daemon-backed
	// Supervisor.
	err = a.AnswerDecision(ctx, info.ID, "dec-1", []acp.DecisionAnswer{
		{QuestionID: "q1", Outcome: acp.DecisionOutcomeChat{}},
	})
	if !errors.Is(err, decision.ErrUnknownRequest) {
		t.Errorf("AnswerDecision(no such request) = %v, want ErrUnknownRequest", err)
	}
}

func TestAdapterDecisionsUnknownSessionErrors(t *testing.T) {
	sup := newTestSupervisor(t)
	a := tuibridge.New(sup, fixedModel("faux"))
	ctx := context.Background()

	if _, err := a.Decisions(ctx, "no-such-session"); err == nil {
		t.Error("Decisions for an unknown session: want an error, got nil")
	}
	if err := a.AnswerDecision(ctx, "no-such-session", "dec-1", nil); err == nil {
		t.Error("AnswerDecision for an unknown session: want an error, got nil")
	}
}
