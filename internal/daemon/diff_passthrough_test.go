package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// editSession scripts one turn whose single tool call is a file edit: it emits
// tool.call.started then a tool.call.finished carrying structured
// [event.FileEdit]s (what the SDK v0.7.0 edit/write tools now produce), then
// turn.finished. It is the seam for the diff pass-through proof — the faux
// provider the other daemon tests use never emits an edit.
type editSession struct {
	id     string
	path   string
	broker *event.Broker
	callID string
	edits  []event.FileEdit
}

func newEditSession(id, path, callID string, edits []event.FileEdit) *editSession {
	return &editSession{broker: event.NewBroker(event.WithReplay(64)), id: id, path: path, callID: callID, edits: edits}
}

func (f *editSession) ID() string               { return f.id }
func (f *editSession) JournalPath() string      { return f.path }
func (f *editSession) Fold() []provider.Message { return nil }
func (f *editSession) Events() *event.Subscription {
	return f.broker.Subscribe(event.FilterAll, 64)
}
func (f *editSession) EventsLive() *event.Subscription {
	return f.broker.SubscribeLive(event.FilterAll, 64)
}
func (f *editSession) Emit(e event.Event)       { f.broker.Publish(e) }
func (f *editSession) Cost() session.CostReport { return session.CostReport{} }
func (f *editSession) Close() error             { f.broker.Close(); return nil }
func (f *editSession) SetModel(string) error    { return nil }
func (f *editSession) SetEffort(string) error   { return nil }

func (f *editSession) Prompt(_ context.Context, _ string) error {
	input := json.RawMessage(`{"path":"main.go"}`)
	f.broker.Publish(event.NewToolCallStarted(f.id, f.callID, "edit", input))
	// Edits ride on the built event (set at emit time, like the real edit tool),
	// not through the constructor — see event.ToolCallFinished.Edits.
	tc := event.NewToolCallFinished(f.id, f.callID, input, "edited main.go", false, nil)
	tc.Edits = f.edits
	f.broker.Publish(tc)
	f.broker.Publish(event.NewTurnFinished(f.id, "end_turn", provider.Usage{}))
	return nil
}

func newEditHarness(t *testing.T, edits []event.FileEdit) string {
	t.Helper()
	root := t.TempDir()
	var nextID int64
	build := func(id, cwd string) supervisor.Session {
		path := filepath.Join(root, "sessions", session.Slugify(cwd), id+".jsonl")
		return newEditSession(id, path, "call-1", edits)
	}
	cfg := supervisor.Config{
		Root: root,
		NewSession: func(_ context.Context, opts runner.Options) (supervisor.Session, error) {
			id := fmt.Sprintf("sess-%d", atomic.AddInt64(&nextID, 1))
			return build(id, opts.Cwd), nil
		},
		ResumeSession: func(_ context.Context, id string, opts runner.Options) (supervisor.Session, error) {
			return build(id, opts.Cwd), nil
		},
	}
	sup, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })
	d := daemon.New(sup, daemon.Config{DefaultModel: "faux"})
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	return "ws" + srv.URL[len("http"):]
}

// diffContent is the wire decode of a tool_call_update's content entries,
// enough to assert the ACP "diff" block reached the peer.
type diffContent struct {
	Type    string `json:"type"`
	Path    string `json:"path"`
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

// TestSessionPromptSurfacesDiffContent is the pass-through proof: a turn whose
// tool call carries structured file edits surfaces a "diff" ToolCallContent on
// the tool_call_update session/update the daemon fans to the peer — for free,
// via acp.ToSessionUpdate. gofer does not build the projection; this proves it
// forwards it end to end after the v0.7.0 re-pin.
func TestSessionPromptSurfacesDiffContent(t *testing.T) {
	edits := []event.FileEdit{
		{Path: "main.go", OldText: "package old", NewText: "package new"},
		{Path: "created.go", NewText: "package created"}, // no OldText: a creation
	}
	url := newEditHarness(t, edits)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	c := dial(t, ctx, url, nil)
	sid := newACPSession(t, c, t.TempDir())

	promptDone := make(chan rpcFrame, 1)
	go func() {
		promptDone <- c.request("session/prompt", map[string]any{"sessionId": sid, "text": "edit main.go"})
	}()

	var got []diffContent
	deadline := time.After(defaultWait)
	for got == nil {
		select {
		case n, ok := <-c.notifications:
			if !ok {
				t.Fatal("connection closed before a diff tool_call_update arrived")
			}
			if n.Method != "session/update" {
				continue
			}
			var up struct {
				Update struct {
					SessionUpdate string        `json:"sessionUpdate"`
					Content       []diffContent `json:"content"`
				} `json:"update"`
			}
			if err := json.Unmarshal(n.Params, &up); err != nil {
				continue // not a tool_call_update-shaped update (e.g. a message chunk)
			}
			if up.Update.SessionUpdate != "tool_call_update" || len(up.Update.Content) == 0 {
				continue
			}
			got = up.Update.Content
		case <-deadline:
			t.Fatal("timed out waiting for a diff tool_call_update")
		}
	}

	if len(got) != len(edits) {
		t.Fatalf("diff content entries = %d, want %d: %+v", len(got), len(edits), got)
	}
	for i, dc := range got {
		if dc.Type != "diff" {
			t.Errorf("content[%d].type = %q, want diff", i, dc.Type)
		}
		if dc.Path != edits[i].Path {
			t.Errorf("content[%d].path = %q, want %q", i, dc.Path, edits[i].Path)
		}
		if dc.NewText != edits[i].NewText {
			t.Errorf("content[%d].newText = %q, want %q", i, dc.NewText, edits[i].NewText)
		}
		if dc.OldText != edits[i].OldText {
			t.Errorf("content[%d].oldText = %q, want %q", i, dc.OldText, edits[i].OldText)
		}
	}

	if resp := <-promptDone; resp.Error != nil {
		t.Fatalf("session/prompt: %v", resp.Error)
	}
}
