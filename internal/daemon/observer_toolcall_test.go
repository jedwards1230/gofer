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
	"github.com/jedwards1230/agent-sdk-go/loop"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// toolCallSession emits a realistic GATED tool-call sequence on its broker:
// tool.call.started, permission.requested, (await the gate), permission.resolved,
// tool.call.finished, turn.finished. It is the seam the observer-projection
// regression test drives — the faux provider the other daemon tests use emits
// only reasoning + text, never a tool call, so it never exercised the
// tool_call session/update fan-out to a non-driving (observer) peer.
type toolCallSession struct {
	id       string
	path     string
	broker   *event.Broker
	approver loop.Approver
	callID   string
}

func newToolCallSession(id, path, callID string) *toolCallSession {
	return &toolCallSession{id: id, path: path, broker: event.NewBroker(event.WithReplay(64)), callID: callID}
}

func (f *toolCallSession) ID() string               { return f.id }
func (f *toolCallSession) JournalPath() string      { return f.path }
func (f *toolCallSession) Fold() []provider.Message { return nil }
func (f *toolCallSession) Events() *event.Subscription {
	return f.broker.Subscribe(event.FilterAll, 64)
}
func (f *toolCallSession) EventsLive() *event.Subscription {
	return f.broker.SubscribeLive(event.FilterAll, 64)
}
func (f *toolCallSession) Emit(e event.Event)       { f.broker.Publish(e) }
func (f *toolCallSession) Cost() session.CostReport { return session.CostReport{} }
func (f *toolCallSession) Close() error             { f.broker.Close(); return nil }

func (f *toolCallSession) Prompt(ctx context.Context, text string) error {
	f.broker.Publish(event.NewToolCallStarted(f.id, f.callID, "bash", json.RawMessage(`{"command":"ls"}`)))
	f.broker.Publish(event.NewPermissionRequested(f.id, f.callID, "bash", map[string]any{"command": text}, []string{"rule: ask"}))
	reply, err := f.approver.Await(ctx, f.callID)
	if err != nil {
		f.broker.Publish(event.NewPermissionResolved(f.id, f.callID, event.VerdictDeny, "cancelled"))
		return err
	}
	f.broker.Publish(event.NewPermissionResolved(f.id, f.callID, reply.Verdict, "human"))
	f.broker.Publish(event.NewToolCallFinished(f.id, f.callID, json.RawMessage(`{"command":"ls"}`), "file1\nfile2\n", false, nil))
	f.broker.Publish(event.NewTurnFinished(f.id, "end_turn", provider.Usage{}))
	return nil
}

func newToolCallHarness(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	var nextID int64
	build := func(id, cwd string, approver loop.Approver) supervisor.Session {
		path := filepath.Join(root, "sessions", session.Slugify(cwd), id+".jsonl")
		fs := newToolCallSession(id, path, "call-1")
		fs.approver = approver
		return fs
	}
	cfg := supervisor.Config{
		Root: root,
		NewSession: func(_ context.Context, opts runner.Options) (supervisor.Session, error) {
			id := fmt.Sprintf("sess-%d", atomic.AddInt64(&nextID, 1))
			return build(id, opts.Cwd, opts.Approver), nil
		},
		ResumeSession: func(_ context.Context, id string, opts runner.Options) (supervisor.Session, error) {
			return build(id, opts.Cwd, opts.Approver), nil
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

// TestObserverReceivesToolCallUpdates is the regression guard for the reported
// symptom "a TUI-driven session's tool calls never appeared on the phone." An
// attached observer peer (not the one driving the turn) MUST receive the
// tool_call and tool_call_update session/update projections for a gated tool
// call another peer drives — the same fan-out path that already carries message
// chunks. The driver answers the permission so the tool proceeds to completion.
func TestObserverReceivesToolCallUpdates(t *testing.T) {
	url := newToolCallHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cwd := t.TempDir()
	driver := dial(t, ctx, url, nil)
	phone := dial(t, ctx, url, nil)

	sid := newACPSession(t, driver, cwd)
	if lr := phone.request("session/load", map[string]any{"sessionId": sid, "cwd": cwd}); lr.Error != nil {
		t.Fatalf("session/load: %v", lr.Error)
	}

	promptDone := make(chan rpcFrame, 1)
	go func() {
		promptDone <- driver.request("session/prompt", map[string]any{"sessionId": sid, "text": "ls"})
	}()

	// The driver answers the gate (gofer-native reply) so the tool proceeds.
	req := waitForNotificationMethod(t, driver, "gofer/permission_requested")
	var pr struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(req.Params, &pr); err != nil {
		t.Fatalf("decode permission_requested: %v", err)
	}
	driver.notify("permission.reply", map[string]any{"id": pr.ID, "verdict": "allow"})

	// The observer must see BOTH tool_call and tool_call_update session/updates.
	want := map[string]bool{"tool_call": false, "tool_call_update": false}
	deadline := time.After(defaultWait)
	remaining := len(want)
	for remaining > 0 {
		select {
		case n, ok := <-phone.notifications:
			if !ok {
				t.Fatalf("observer connection closed; still missing %v", want)
			}
			if n.Method != "session/update" {
				continue
			}
			// Decode only the update discriminator: a tool_call_update's content
			// is an array, which the message-chunk-shaped sessionUpdateParams
			// cannot hold.
			var up struct {
				Update struct {
					SessionUpdate string `json:"sessionUpdate"`
				} `json:"update"`
			}
			if err := json.Unmarshal(n.Params, &up); err != nil {
				t.Fatalf("decode session/update: %v", err)
			}
			if seen, tracked := want[up.Update.SessionUpdate]; tracked && !seen {
				want[up.Update.SessionUpdate] = true
				remaining--
			}
		case <-deadline:
			t.Fatalf("observer did not receive all tool_call session/updates: %v", want)
		}
	}

	if resp := <-promptDone; resp.Error != nil {
		t.Fatalf("session/prompt: %v", resp.Error)
	}
}
