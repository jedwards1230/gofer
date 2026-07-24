package supervisor_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/runner"

	"github.com/jedwards1230/gofer/internal/decision"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// askUserContinuationProvider scripts a real runner turn that (1) calls
// ask_user and stops for tool use, then (2) — after the tool returns the
// human's answer — makes ONE more model call and ends the turn. It is a
// faithful stand-in for a real backend on exactly the path the bug report
// covers: the model asks, the human answers, and the loop must CONTINUE.
//
// It respects ctx like every real provider does (returns ctx.Err() before
// streaming), so if the turn's context were cancelled while the human was
// answering, the SECOND Stream call would fail with context.Canceled — which
// is the exact symptom under test. A ctx-ignoring fake would mask it.
type askUserContinuationProvider struct {
	mu       sync.Mutex
	calls    int
	call2Ctx error // ctx.Err() observed at the continuation call
}

func (p *askUserContinuationProvider) Stream(ctx context.Context, _ provider.Request) (provider.StreamHandle, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	if n == 2 {
		p.call2Ctx = ctx.Err()
	}
	p.mu.Unlock()

	// A real provider issues its HTTP request under ctx and fails fast if it is
	// already dead. Model the same so a cancelled continuation surfaces here.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	switch n {
	case 1:
		return provider.SliceStream(
			provider.StreamEvent{Type: provider.StreamToolCallStart, Tool: &provider.ToolCall{ID: "call-1", Name: "ask_user"}},
			provider.StreamEvent{Type: provider.StreamToolCallEnd, Tool: &provider.ToolCall{ID: "call-1", Name: "ask_user", Input: json.RawMessage(askTwoOptions)}},
			provider.StreamEvent{Type: provider.StreamFinished, StopReason: provider.StopToolUse, Usage: provider.Usage{InputTokens: 4, OutputTokens: 1}},
		), nil
	default:
		return provider.SliceStream(
			provider.StreamEvent{Type: provider.StreamTextDelta, Text: "acting on your answer"},
			provider.StreamEvent{Type: provider.StreamFinished, StopReason: provider.StopEndTurn, Usage: provider.Usage{InputTokens: 6, OutputTokens: 2}},
		), nil
	}
}

func (p *askUserContinuationProvider) Info() provider.ModelInfo {
	return provider.ModelInfo{ID: "cont-1", Provider: "cont", ContextWindow: 200_000, MaxOutput: 8192}
}

func (p *askUserContinuationProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *askUserContinuationProvider) continuationCtxErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.call2Ctx
}

// TestAskUserTurnContinuesAfterAnswer drives the ask_user round trip over a
// REAL *runner.Runner session (not the fakeSession) so the loop actually
// CONTINUES after the answer — the step TestAnswerDecisionRoundTrip never
// exercises. A human delay sits between the request and the answer, so a turn
// whose blocking-on-human wait was bounded by a compute timeout would have
// tripped context.Canceled and cancelled the continuation.
func TestAskUserTurnContinuesAfterAnswer(t *testing.T) {
	prov := &askUserContinuationProvider{}
	sup, err := supervisor.New(supervisor.Config{
		Root: t.TempDir(),
		NewSession: func(ctx context.Context, opts runner.Options) (supervisor.Session, error) {
			opts.Provider = prov
			return runner.New(ctx, opts)
		},
	})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	ctx := context.Background()
	info, err := sup.Create(ctx, "", supervisor.CreateOptions{Cwd: t.TempDir(), Model: "cont-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	sub, err := sup.SubscribeDecisions(info.ID, 4)
	if err != nil {
		t.Fatalf("SubscribeDecisions: %v", err)
	}
	defer sub.Close()

	if err := sup.Send(ctx, info.ID, "help me decide"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	up := waitForDecision(t, sub)
	if up.Kind != decision.UpdateRequested {
		t.Fatalf("update = %v, want requested", up.Kind)
	}

	// A human takes a moment to answer. This is the window a turn-scoped
	// compute timeout would fire in.
	time.Sleep(150 * time.Millisecond)

	if err := sup.AnswerDecision(info.ID, up.Request.ID, []acp.DecisionAnswer{
		{QuestionID: "q1", Outcome: acp.DecisionOutcomeSelected{OptionID: "q1o2"}},
	}); err != nil {
		t.Fatalf("AnswerDecision: %v", err)
	}

	// The turn is done only once it settles back to needs-input — which happens
	// AFTER the continuation model call and its journaling barrier.
	waitForStatus(t, sup, info.ID, supervisor.StatusNeedsInput)

	if got := prov.callCount(); got != 2 {
		t.Fatalf("provider was called %d times, want 2 — the turn did not continue after the answer", got)
	}
	if err := prov.continuationCtxErr(); err != nil {
		t.Fatalf("continuation model call saw a cancelled context (%v) — answering ask_user killed the turn", err)
	}
}
