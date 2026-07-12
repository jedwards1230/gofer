package runner_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/loop"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/session"
	"github.com/jedwards1230/agent-sdk-go/tool"

	"github.com/jedwards1230/gofer/internal/runner"
)

const testModel = "test-model"

// scriptedProvider is a gofer-local, deterministic provider.Provider: each
// call to Stream consumes the next scripted event sequence, in order. It
// never touches the network — the canonical fake for a hermetic loop.Run
// drive.
type scriptedProvider struct {
	calls  int
	events [][]provider.StreamEvent
}

func (p *scriptedProvider) Stream(_ context.Context, _ provider.Request) (provider.StreamHandle, error) {
	if p.calls >= len(p.events) {
		return nil, fmt.Errorf("scriptedProvider: unexpected call %d (scripted for %d)", p.calls+1, len(p.events))
	}
	evs := p.events[p.calls]
	p.calls++
	return provider.SliceStream(evs...), nil
}

func (p *scriptedProvider) Info() provider.ModelInfo {
	return provider.ModelInfo{ID: testModel, Provider: "test"}
}

// cancelAfterRead wraps a real *tool.Read: it runs the real tool (so the
// journaled result is genuine file content, not a fixture), then cancels the
// run synchronously — deterministically, in the same goroutine that drives
// loop.Run, before the tool round's result is even published — the moment
// the round settles, proving a kill mid-run only ever loses unsettled work.
type cancelAfterRead struct {
	read   *tool.Read
	cancel context.CancelFunc
	fired  atomic.Bool
}

func (c *cancelAfterRead) Run(ctx context.Context, input json.RawMessage) (loop.ToolResult, error) {
	res, err := c.read.Run(ctx, input)
	if err != nil {
		return loop.ToolResult{}, err
	}
	if !c.fired.Swap(true) {
		c.cancel()
	}
	return loop.ToolResult{Content: res.Content, IsError: res.IsError}, nil
}

// oneToolRegistry is a minimal loop.ToolRegistry offering a single named
// tool — enough to drive the loop without pulling in the full builtin set.
type oneToolRegistry struct {
	name string
	tool loop.Tool
}

func (r oneToolRegistry) Get(name string) (loop.Tool, bool) {
	if name != r.name {
		return nil, false
	}
	return r.tool, true
}

func (r oneToolRegistry) Specs() []provider.ToolSpec {
	return []provider.ToolSpec{{Name: r.name, Description: "test tool"}}
}

// seqClock and seqIDGen give tests deterministic, monotonic journal
// timestamps and ids without depending on wall-clock ordering.
func seqClock() func() time.Time {
	t := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return func() time.Time {
		t = t.Add(time.Second)
		return t
	}
}

func seqIDGen() func() string {
	n := 0
	return func() string {
		n++
		return fmt.Sprintf("id-%04d", n)
	}
}

// TestRunner_TextTurn drives a single plain (no tool call) turn and asserts
// the user prompt and the settled assistant reply both land as message
// entries, and that Fold projects them back losslessly.
func TestRunner_TextTurn(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()

	prov := &scriptedProvider{events: [][]provider.StreamEvent{
		{
			{Type: provider.StreamReasoningDelta, Text: "thinking"},
			{Type: provider.StreamTextDelta, Text: "hi "},
			{Type: provider.StreamTextDelta, Text: "there"},
			{Type: provider.StreamFinished, StopReason: provider.StopEndTurn, Usage: provider.Usage{InputTokens: 3, OutputTokens: 2}},
		},
	}}

	r, err := runner.NewSession(context.Background(), runner.Options{
		Root: root, Cwd: cwd, Model: testModel, System: "test system",
		Provider: prov,
		Tools:    oneToolRegistry{}, // no tools needed; Get always misses
		IDGen:    seqIDGen(),
		Clock:    seqClock(),
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	id := r.ID()

	if err := r.Prompt(context.Background(), "hello"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	// Journaling streams into the journal on its own goroutine as the turn
	// settles; Close is the documented synchronization point that waits for
	// it to drain, so assertions on journaled content must come after it.
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	fold := r.Fold()
	if len(fold) != 2 {
		t.Fatalf("Fold: got %d messages, want 2: %+v", len(fold), fold)
	}
	if fold[0].Role != "user" || fold[0].Content != "hello" {
		t.Errorf("fold[0] = %+v, want user %q", fold[0], "hello")
	}
	if fold[1].Role != "assistant" || fold[1].Content != "hi there" {
		t.Errorf("fold[1] = %+v, want assistant %q", fold[1], "hi there")
	}
	if fold[1].Reasoning != "thinking" {
		t.Errorf("fold[1].Reasoning = %q, want %q", fold[1].Reasoning, "thinking")
	}

	// Reopen from a fresh store (no in-process cache) to prove the turn is
	// durable on disk, not just held in memory.
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
		t.Fatalf("Entries: got %d, want 2: %+v", len(entries), entries)
	}
	if entries[0].Type != session.EntryMessage || entries[1].Type != session.EntryMessage {
		t.Errorf("entry types = %s, %s, want message, message", entries[0].Type, entries[1].Type)
	}
}

// TestRunner_KillAndResume is the M1 milestone proof: it shows a tool
// actually executes, that the journal is durable at the moment a run is
// killed mid-flight (a settled tool round survives cancellation), and that
// Resume folds that prior context back into the provider's messages and
// continues the conversation.
func TestRunner_KillAndResume(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	notesPath := filepath.Join(cwd, "notes.txt")
	if err := os.WriteFile(notesPath, []byte("hello world"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// --- Phase 1: run until the tool round settles, then kill. ---

	toolInput, err := json.Marshal(map[string]string{"path": "notes.txt"})
	if err != nil {
		t.Fatalf("marshal tool input: %v", err)
	}
	prov1 := &scriptedProvider{events: [][]provider.StreamEvent{
		{
			{Type: provider.StreamToolCallStart, Tool: &provider.ToolCall{ID: "t1", Name: "read"}},
			{Type: provider.StreamToolCallEnd, Tool: &provider.ToolCall{ID: "t1", Name: "read", Input: toolInput}},
			{Type: provider.StreamFinished, StopReason: provider.StopToolUse, Usage: provider.Usage{InputTokens: 10, OutputTokens: 5}},
		},
		// A second call is scripted defensively; the cancellation below must
		// pre-empt loop.Run before it is ever reached.
		{
			{Type: provider.StreamTextDelta, Text: "should not run"},
			{Type: provider.StreamFinished, StopReason: provider.StopEndTurn, Usage: provider.Usage{InputTokens: 1, OutputTokens: 1}},
		},
	}}

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	tools1 := oneToolRegistry{name: "read", tool: &cancelAfterRead{read: tool.NewRead(cwd), cancel: cancel1}}

	r1, err := runner.NewSession(context.Background(), runner.Options{
		Root: root, Cwd: cwd, Model: testModel, System: "test system",
		Provider: prov1, Tools: tools1,
		IDGen: seqIDGen(), Clock: seqClock(),
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	id := r1.ID()

	promptErr := r1.Prompt(ctx1, "read notes.txt")
	if !errors.Is(promptErr, context.Canceled) {
		t.Fatalf("Prompt: got %v, want context.Canceled", promptErr)
	}
	if prov1.calls != 1 {
		t.Fatalf("scriptedProvider: got %d calls, want exactly 1 (the second iteration must not run)", prov1.calls)
	}

	// Close waits for the journaling goroutine to drain — required before any
	// assertion on-disk, since journaling happens on its own goroutine.
	if err := r1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen from a brand-new store (bypassing any in-process cache) to prove
	// the settled prefix is durable, not merely resident in r1's memory.
	verifyStore, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	verifyJournal, err := verifyStore.Open(context.Background(), id)
	if err != nil {
		t.Fatalf("Open(%s): %v", id, err)
	}
	entries := verifyJournal.Entries()
	if len(entries) != 2 {
		t.Fatalf("Entries after kill: got %d, want 2 (user message + settled tool round): %+v", len(entries), entries)
	}
	if entries[0].Type != session.EntryMessage {
		t.Fatalf("entries[0].Type = %s, want %s", entries[0].Type, session.EntryMessage)
	}
	if msg, err := entries[0].Message(); err != nil || msg.Content != "read notes.txt" {
		t.Fatalf("entries[0].Message() = %+v, %v", msg, err)
	}
	if entries[1].Type != session.EntryToolRound {
		t.Fatalf("entries[1].Type = %s, want %s", entries[1].Type, session.EntryToolRound)
	}
	round, err := entries[1].ToolRound()
	if err != nil {
		t.Fatalf("entries[1].ToolRound(): %v", err)
	}
	if len(round.Calls) != 1 {
		t.Fatalf("ToolRound.Calls: got %d, want 1: %+v", len(round.Calls), round.Calls)
	}
	call := round.Calls[0]
	if call.ID != "t1" || call.Name != "read" {
		t.Errorf("call = %+v, want id t1, name read", call)
	}
	if !strings.Contains(call.Result, "hello world") {
		t.Errorf("call.Result = %q, want it to contain the real file contents %q", call.Result, "hello world")
	}
	if call.IsError {
		t.Errorf("call.IsError = true, want false")
	}
	if err := verifyStore.Close(); err != nil {
		t.Fatalf("verifyStore.Close: %v", err)
	}

	// --- Phase 2: resume the same session id and continue. ---

	prov2 := &scriptedProvider{events: [][]provider.StreamEvent{
		{
			{Type: provider.StreamTextDelta, Text: "done"},
			{Type: provider.StreamFinished, StopReason: provider.StopEndTurn, Usage: provider.Usage{InputTokens: 20, OutputTokens: 3}},
		},
	}}

	r2, err := runner.Resume(context.Background(), id, runner.Options{
		Root: root, Cwd: cwd, Model: testModel, System: "test system",
		Provider: prov2, Tools: oneToolRegistry{},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	// The resumed runner's fold must already carry the prior tool result —
	// proof the fold/project round-trip preserves it across a process
	// boundary (a fresh store reopened it from disk in Resume itself).
	preFold := r2.Fold()
	if len(preFold) != 2 {
		t.Fatalf("preFold: got %d messages, want 2: %+v", len(preFold), preFold)
	}
	if len(preFold[1].ToolCalls) != 1 || !strings.Contains(preFold[1].ToolCalls[0].Result, "hello world") {
		t.Fatalf("preFold[1].ToolCalls = %+v, want the prior read result", preFold[1].ToolCalls)
	}

	// session.resumed is must-deliver, so the broker's replay buffer hands it
	// to this subscription immediately even though it was published before
	// Events was called.
	sub := r2.Events()
	select {
	case e, ok := <-sub.C:
		if !ok {
			t.Fatal("subscription closed before observing session.resumed")
		}
		if _, ok := e.(event.SessionResumed); !ok {
			t.Fatalf("first replayed event = %T, want event.SessionResumed", e)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for session.resumed")
	}
	sub.Close()

	if err := r2.Prompt(context.Background(), "continue"); err != nil {
		t.Fatalf("Prompt (resumed): %v", err)
	}

	// As above: wait for the journaling goroutine to drain before reading the
	// continuation's settled output.
	if err := r2.Close(); err != nil {
		t.Fatalf("Close (resumed): %v", err)
	}

	postFold := r2.Fold()
	if len(postFold) != 4 {
		t.Fatalf("postFold: got %d messages, want 4: %+v", len(postFold), postFold)
	}
	last := postFold[len(postFold)-1]
	if last.Role != "assistant" || last.Content != "done" {
		t.Fatalf("postFold last = %+v, want assistant %q", last, "done")
	}

	finalStore, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	defer func() { _ = finalStore.Close() }()
	finalJournal, err := finalStore.Open(context.Background(), id)
	if err != nil {
		t.Fatalf("Open(%s): %v", id, err)
	}
	if got := len(finalJournal.Entries()); got != 4 {
		t.Fatalf("final Entries: got %d, want 4 (the journal grew with the continuation)", got)
	}
}
