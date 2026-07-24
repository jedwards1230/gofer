package supervisor_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/runner"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// recordingProvider captures the tool specs the loop advertises on the first
// model call, then ends the turn. It is the seam this test uses to observe the
// exact tool surface a real session presents to a provider — the same surface a
// real OpenAI/Anthropic backend would receive.
type recordingProvider struct {
	mu    sync.Mutex
	tools []provider.ToolSpec
	seen  chan struct{}
	once  sync.Once
}

func (p *recordingProvider) Stream(_ context.Context, req provider.Request) (provider.StreamHandle, error) {
	p.mu.Lock()
	p.tools = append([]provider.ToolSpec(nil), req.Tools...)
	p.mu.Unlock()
	p.once.Do(func() { close(p.seen) })
	return provider.SliceStream(provider.StreamEvent{
		Type:       provider.StreamFinished,
		StopReason: provider.StopEndTurn,
		Usage:      provider.Usage{InputTokens: 1, OutputTokens: 1},
	}), nil
}

func (p *recordingProvider) Info() provider.ModelInfo {
	return provider.ModelInfo{ID: "rec", Provider: "rec", ContextWindow: 200_000, MaxOutput: 8192}
}

func (p *recordingProvider) names() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.tools))
	for i, t := range p.tools {
		out[i] = t.Name
	}
	return out
}

// TestSessionAdvertisesAskUser is the regression guard for "the ask_user tool
// is not available to the agent": it drives a real *runner.Runner session over
// a recording provider and asserts that the tool specs the loop advertises to
// the provider include gofer's own ask_user tool alongside the SDK builtins.
//
// It is stronger than TestYoloKeepsTheAskUserTool (which asserts the registry
// the supervisor hands the runner contains ask_user): this one follows the tool
// surface all the way to the provider boundary — sessionGuard → runner.New →
// loop → Stream — so a regression anywhere on that path (a dropped extra, a
// registry that advertises only builtins) fails here.
func TestSessionAdvertisesAskUser(t *testing.T) {
	rec := &recordingProvider{seen: make(chan struct{})}
	sup, err := supervisor.New(supervisor.Config{
		Root: t.TempDir(),
		NewSession: func(ctx context.Context, opts runner.Options) (supervisor.Session, error) {
			opts.Provider = rec
			return runner.New(ctx, opts)
		},
	})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	if _, err := sup.Create(context.Background(), "list your tools", supervisor.CreateOptions{
		Model: "rec-1", Cwd: t.TempDir(),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	select {
	case <-rec.seen:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the provider to be called")
	}

	got := rec.names()
	has := func(name string) bool {
		for _, n := range got {
			if n == name {
				return true
			}
		}
		return false
	}
	if !has("ask_user") {
		t.Errorf("advertised tools %v do not include ask_user — the agent cannot ask the user a structured question", got)
	}
	// Guard against the inverse regression: a change that advertises ask_user
	// but drops the builtins would also be wrong.
	if !has("bash") {
		t.Errorf("advertised tools %v do not include bash", got)
	}
}
