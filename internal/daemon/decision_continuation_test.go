package daemon_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/runner"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// continuationProvider scripts a real runner turn over the daemon path: call 1
// asks ask_user and stops for tool use; call 2 — the CONTINUATION the loop must
// make after the human's answer — ends the turn. It respects ctx exactly like a
// real backend, so a turn whose context was cancelled during the human wait
// fails the continuation with context.Canceled here, reproducing the report.
type continuationProvider struct {
	mu       sync.Mutex
	calls    int
	call2Err error
}

func (p *continuationProvider) Stream(ctx context.Context, _ provider.Request) (provider.StreamHandle, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	if n == 2 {
		p.call2Err = ctx.Err()
	}
	p.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	switch n {
	case 1:
		return provider.SliceStream(
			provider.StreamEvent{Type: provider.StreamToolCallStart, Tool: &provider.ToolCall{ID: "call-1", Name: "ask_user"}},
			provider.StreamEvent{Type: provider.StreamToolCallEnd, Tool: &provider.ToolCall{ID: "call-1", Name: "ask_user", Input: json.RawMessage(askUserCall)}},
			provider.StreamEvent{Type: provider.StreamFinished, StopReason: provider.StopToolUse, Usage: provider.Usage{InputTokens: 4, OutputTokens: 1}},
		), nil
	default:
		return provider.SliceStream(
			provider.StreamEvent{Type: provider.StreamTextDelta, Text: "acting on your answer"},
			provider.StreamEvent{Type: provider.StreamFinished, StopReason: provider.StopEndTurn, Usage: provider.Usage{InputTokens: 6, OutputTokens: 2}},
		), nil
	}
}

func (p *continuationProvider) Info() provider.ModelInfo {
	return provider.ModelInfo{ID: "cont-1", Provider: "cont", ContextWindow: 200_000, MaxOutput: 8192}
}

func (p *continuationProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func (p *continuationProvider) continuationErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.call2Err
}

// TestDaemonAskUserTurnContinuesAfterAnswer is the end-to-end regression guard
// for the bug report: over the REAL daemon path (ws client → session/prompt →
// gofer/decision_requested → decision.answer) and a REAL *runner.Runner
// session, answering an ask_user prompt must let the turn CONTINUE — the loop
// makes its next model call and the turn finishes end_turn — rather than dying
// with context.Canceled. A human delay sits between the request and the answer.
func TestDaemonAskUserTurnContinuesAfterAnswer(t *testing.T) {
	prov := &continuationProvider{}
	root := t.TempDir()
	sup, err := supervisor.New(supervisor.Config{
		Root: root,
		// Yolo so the ask_user tool RUNS regardless of whether a sandbox backend
		// is present: under contain-or-ask, a runner with no available backend
		// (macOS has seatbelt; a bare Linux CI runner may lack bwrap) cannot
		// contain any call, so the guard escalates ask_user to a permission "Ask"
		// and the turn blocks on an approval nobody answers instead of surfacing
		// the decision (CanContain = available && containable, see internal/sandbox).
		PermissionMode: func() config.PermissionMode { return config.PermissionModeYolo },
		NewSession: func(ctx context.Context, opts runner.Options) (supervisor.Session, error) {
			opts.Provider = prov
			return runner.New(ctx, opts)
		},
	})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	d := daemon.New(sup, daemon.Config{DefaultModel: "cont-1"})
	sup.SetDecisionRelay(d)
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	url := "ws" + srv.URL[len("http"):]

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cwd := t.TempDir()
	c := dial(t, ctx, url, nil)
	sid := newACPSession(t, c, cwd)

	promptDone := make(chan rpcFrame, 1)
	go func() {
		promptDone <- c.request("session/prompt", map[string]any{"sessionId": sid, "text": "help me decide"})
	}()

	req := awaitDecisionRequest(t, c)

	// A human takes a moment to answer — the window a turn-scoped timeout would
	// fire in.
	time.Sleep(300 * time.Millisecond)

	c.notify(daemon.MethodDecisionAnswer, selectAnswer(sid, req.ID, "q1", "q1o1", ""))

	select {
	case resp := <-promptDone:
		if resp.Error != nil {
			t.Fatalf("session/prompt returned an error after the answer: %v", resp.Error)
		}
		var pr struct {
			StopReason string `json:"stopReason"`
		}
		if err := json.Unmarshal(resp.Result, &pr); err != nil {
			t.Fatalf("decode session/prompt response: %v", err)
		}
		if pr.StopReason == "cancelled" {
			t.Fatalf("turn ended cancelled after the answer — the continuation was killed (provider calls=%d, continuation ctx err=%v)",
				prov.callCount(), prov.continuationErr())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("session/prompt did not return after the decision was answered")
	}

	if got := prov.callCount(); got != 2 {
		t.Fatalf("provider was called %d times, want 2 — the turn did not continue after the answer", got)
	}
	if err := prov.continuationErr(); err != nil {
		t.Fatalf("continuation model call saw a cancelled context (%v) — answering ask_user killed the turn", err)
	}
}
