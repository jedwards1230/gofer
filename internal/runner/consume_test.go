package runner_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/loop"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/runner"
)

// streamThenBlockProvider yields a fixed prefix of stream events, then blocks
// the final Next on ctx.Done() and returns ctx.Err() — modeling a model call
// killed mid-stream, specifically while a tool call is still streaming its
// input (announced via tool.call.started, with no end event and no result).
type streamThenBlockProvider struct {
	prefix []provider.StreamEvent
}

func (p *streamThenBlockProvider) Stream(ctx context.Context, _ provider.Request) (provider.StreamHandle, error) {
	return &blockingStream{events: p.prefix, ctx: ctx}, nil
}

func (p *streamThenBlockProvider) Info() provider.ModelInfo {
	return provider.ModelInfo{ID: testModel, Provider: "test"}
}

type blockingStream struct {
	events []provider.StreamEvent
	i      int
	ctx    context.Context
}

func (s *blockingStream) Next() (provider.StreamEvent, error) {
	if s.i < len(s.events) {
		e := s.events[s.i]
		s.i++
		return e, nil
	}
	<-s.ctx.Done()
	return provider.StreamEvent{}, s.ctx.Err()
}

func (s *blockingStream) Close() error { return nil }

// unusedTool satisfies loop.Tool for a registry whose tool is never executed
// (the run is killed before the loop reaches tool execution).
type unusedTool struct{}

func (unusedTool) Run(context.Context, json.RawMessage) (loop.ToolResult, error) {
	return loop.ToolResult{}, nil
}

// waitForKind drains sub until an event of kind arrives, failing the test on
// timeout or an early channel close.
func waitForKind(t *testing.T, sub *event.Subscription, kind string) {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case e, ok := <-sub.C:
			if !ok {
				t.Fatalf("subscription closed before observing %q", kind)
			}
			if e.Kind() == kind {
				return
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %q", kind)
		}
	}
}

// TestRunner_KillDuringToolStreaming is the regression for the HIGH sweep
// finding: a run killed AFTER a turn's assistant text/reasoning has settled but
// BEFORE a just-announced tool call streams to completion (started, but no end
// event and no result) must still journal the settled text. The prior
// accumulator gated the whole-turn flush on pending==0, so the tool.call.finished
// that never arrives stranded the settled text (silent on-disk data loss) and
// wedged the accumulator for the Runner's life.
func TestRunner_KillDuringToolStreaming(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()

	toolInput, err := json.Marshal(map[string]string{"path": "notes.txt"})
	if err != nil {
		t.Fatalf("marshal tool input: %v", err)
	}
	prov := &streamThenBlockProvider{prefix: []provider.StreamEvent{
		{Type: provider.StreamReasoningDelta, Text: "planning the read"},
		{Type: provider.StreamTextDelta, Text: "I will read the notes."},
		{Type: provider.StreamToolCallStart, Tool: &provider.ToolCall{ID: "t1", Name: "read", Input: toolInput}},
		// No end/finished: the next Next blocks until the test cancels ctx.
	}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r, err := runner.NewSession(context.Background(), runner.Options{
		Root: root, Cwd: cwd, Model: testModel, System: "test system",
		Provider: prov,
		Tools:    oneToolRegistry{name: "read", tool: unusedTool{}},
		IDGen:    seqIDGen(), Clock: seqClock(),
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	id := r.ID()

	sub := r.Events()
	promptDone := make(chan error, 1)
	go func() { promptDone <- r.Prompt(ctx, "read the notes") }()

	// Kill the moment the tool call is announced — i.e. after the assistant
	// text has settled (message.finished precedes tool.call.started) but before
	// any tool result.
	waitForKind(t, sub, event.KindToolCallStarted)
	cancel()

	if err := <-promptDone; err == nil {
		t.Fatal("Prompt returned nil, want a cancellation error")
	}
	sub.Close()

	// Close drains the journaling goroutine; assertions on disk come after it.
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen from a fresh store to prove durability on disk.
	store, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer func() { _ = store.Close() }()
	j, err := store.Open(context.Background(), id)
	if err != nil {
		t.Fatalf("Open(%s): %v", id, err)
	}

	entries := j.Entries()
	if len(entries) != 2 {
		t.Fatalf("Entries after kill mid tool-streaming: got %d, want 2 (user message + settled assistant message): %+v", len(entries), entries)
	}
	if entries[0].Type != session.EntryMessage {
		t.Fatalf("entries[0].Type = %s, want %s", entries[0].Type, session.EntryMessage)
	}
	if userMsg, err := entries[0].Message(); err != nil || userMsg.Content != "read the notes" {
		t.Fatalf("entries[0].Message() = %+v, %v", userMsg, err)
	}
	if entries[1].Type != session.EntryMessage {
		t.Fatalf("entries[1].Type = %s, want %s (the settled assistant text, NOT dropped, and NOT a dangling tool_round)", entries[1].Type, session.EntryMessage)
	}
	asst, err := entries[1].Message()
	if err != nil {
		t.Fatalf("entries[1].Message(): %v", err)
	}
	if asst.Role != "assistant" || asst.Content != "I will read the notes." {
		t.Errorf("assistant entry = %+v, want role assistant content %q", asst, "I will read the notes.")
	}
	if asst.Reasoning != "planning the read" {
		t.Errorf("assistant reasoning = %q, want %q", asst.Reasoning, "planning the read")
	}

	// The fold must round-trip cleanly: two messages, the assistant one carrying
	// the settled text/reasoning and NO orphaned tool call (no dangling tool_use
	// would corrupt the provider projection on resume).
	fold := j.Fold()
	if len(fold) != 2 {
		t.Fatalf("Fold: got %d, want 2: %+v", len(fold), fold)
	}
	if len(fold[1].ToolCalls) != 0 {
		t.Errorf("fold[1].ToolCalls = %+v, want none (orphaned call dropped)", fold[1].ToolCalls)
	}
	if !strings.Contains(fold[1].Content, "read the notes") {
		t.Errorf("fold[1].Content = %q, want the settled assistant text", fold[1].Content)
	}
}
