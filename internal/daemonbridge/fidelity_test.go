package daemonbridge_test

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/loop"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/tui"
)

// diagnosticSpillTool is a hand-rolled [loop.Tool] (mirroring the SDK's own
// runner/runner_test.go oneToolRegistry pattern) whose Run returns
// Diagnostics — a field the builtin tool registry never populates (see
// loop/toolreg.go) — plus enough output content that the loop's spill sink
// (loop.runOneTool, always file-backed here since newTestSupervisorWithTools
// gives every session a real session.FileStore) writes a durable per-call
// file, so the resulting tool.call.finished carries SpillPath/SpillBytes/
// SpillSHA256 too. Both are entirely dropped by ACP's session/update
// projection (acp.ToSessionUpdate only carries a tool_call_update's bounded
// text content) — the fidelity test below proves they cross the wire
// losslessly via gofer/event.
type diagnosticSpillTool struct{}

func (diagnosticSpillTool) Run(context.Context, json.RawMessage) (loop.ToolResult, error) {
	return loop.ToolResult{
		Content:     spillContent,
		Diagnostics: []string{"lint: unused variable x", "vet: possible nil dereference at line 42"},
	}, nil
}

// spillContent is large enough (> the spill package's 2KiB+2KiB head/tail
// excerpt budget) that the resulting excerpt is genuinely elided, proving the
// spill reference is for real durable content, not just a technicality.
var spillContent = func() string {
	s := make([]byte, 6<<10)
	for i := range s {
		s[i] = byte('a' + i%26)
	}
	return string(s)
}()

// oneToolRegistry is a minimal loop.ToolRegistry offering a single named
// tool, mirroring the SDK's own runner/runner_test.go helper of the same
// name (unexported there).
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

// deltaToolTurnProvider is a hand-scripted [provider.Provider] whose first
// turn streams a short text delta, then a tool call whose streaming INPUT
// arrives as TWO provider.StreamToolCallDelta fragments (the shape
// event.ToolCallDelta projects from — see provider.StreamToolCallDelta's
// doc), then finishes tool_use; its second turn finishes with plain text.
// This is the provider seam TestFidelityToolCallDeltaAndSpill needs and
// neither the faux provider (text/reasoning-only) nor bridge_test.go's
// existing toolTurnProvider (Start→End with no Delta in between) provides.
type deltaToolTurnProvider struct{ turn int }

func (p *deltaToolTurnProvider) Info() provider.ModelInfo {
	return provider.ModelInfo{ID: "delta-tool-test"}
}

func (p *deltaToolTurnProvider) Stream(context.Context, provider.Request) (provider.StreamHandle, error) {
	turn := p.turn
	p.turn++
	switch turn {
	case 0:
		return provider.SliceStream(
			provider.StreamEvent{Type: provider.StreamTextDelta, Text: "Let me check that file. "},
			provider.StreamEvent{Type: provider.StreamToolCallStart, Tool: &provider.ToolCall{ID: "tc-1", Name: "diag_tool"}},
			provider.StreamEvent{Type: provider.StreamToolCallDelta, Tool: &provider.ToolCall{ID: "tc-1", Delta: `{"partial`}},
			provider.StreamEvent{Type: provider.StreamToolCallDelta, Tool: &provider.ToolCall{ID: "tc-1", Delta: `":"a.txt"}`}},
			provider.StreamEvent{Type: provider.StreamToolCallEnd, Tool: &provider.ToolCall{ID: "tc-1", Name: "diag_tool", Input: []byte(`{"partial":"a.txt"}`)}},
			provider.StreamEvent{Type: provider.StreamFinished, StopReason: provider.StopToolUse},
		), nil
	case 1:
		return provider.SliceStream(
			provider.StreamEvent{Type: provider.StreamTextDelta, Text: "done"},
			provider.StreamEvent{Type: provider.StreamFinished, StopReason: provider.StopEndTurn},
		), nil
	default:
		return nil, io.ErrUnexpectedEOF
	}
}

// stripEnvelopeSeqTime marshals e (invoking its own MarshalJSON — the exact
// wire bytes broadcastGoferEvent sends) and returns the envelope as a
// generic map with "seq"/"time" removed: the only fields a gofer/event round
// trip doesn't preserve byte-for-byte, because event.New* always builds
// seq=0/time=zero and the LOCAL broker on each side (source supervisor,
// reconstructing bridge) reassigns its OWN real seq/time at Publish — see
// internal/daemonbridge/reconstruct.go's package doc, "seq/time note": by
// design, not a fidelity gap. A field-for-field comparison of two envelopes
// strips them first.
func stripEnvelopeSeqTime(t *testing.T, e event.Event) map[string]any {
	t.Helper()
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal %T: %v", e, err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal %T: %v", e, err)
	}
	delete(m, "seq")
	delete(m, "time")
	return m
}

// TestFidelityToolCallDeltaAndSpill is the acceptance centerpiece for M3
// lossless attach: it drives one turn whose SOURCE event stream (subscribed
// directly off the real *supervisor.Supervisor, over context.Background())
// contains both a tool.call.delta (the streaming INPUT fragment ACP's
// session/update drops entirely — the headline loss) and a tool.call.finished
// carrying Diagnostics + all three Spill* fields (also entirely dropped by
// the ACP projection), then asserts the daemonbridge-RECONSTRUCTED stream
// (subscribed over the real in-process daemon + WebSocket — no fake
// daemon.Client) is equal to the source, event-by-event, kind AND every
// payload field, ignoring only seq/time (reassigned locally on each side by
// design — see stripEnvelopeSeqTime).
func TestFidelityToolCallDeltaAndSpill(t *testing.T) {
	tools := oneToolRegistry{name: "diag_tool", tool: diagnosticSpillTool{}}
	sup := newTestSupervisorWithTools(t, func() provider.Provider { return &deltaToolTurnProvider{} }, tools)
	url := newTestDaemon(t, sup)
	b := newBridge(t, url)

	ctx := context.Background()
	info, err := b.Create(ctx, "", tui.CreateOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Subscribe to BOTH streams before driving the turn — the source directly
	// off the supervisor, the reconstructed one over the real daemon+bridge —
	// so neither misses this turn's opening events.
	sourceSub, err := sup.SubscribeLive(ctx, info.ID)
	if err != nil {
		t.Fatalf("sup.SubscribeLive: %v", err)
	}
	defer sourceSub.Close()

	bridgeSub, err := b.Subscribe(ctx, info.ID)
	if err != nil {
		t.Fatalf("b.Subscribe: %v", err)
	}
	defer bridgeSub.Close()

	if err := b.Send(ctx, info.ID, "read a.txt"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// session.info (the title the supervisor derives from this first prompt at
	// enqueue, before the turn's own events — see supervisor/managed.go),
	// message.started(user), message.finished(user), turn.started,
	// message.started(text), message.delta, message.finished(text),
	// tool.call.started, tool.call.delta, tool.call.delta,
	// turn.finished(tool_use), tool.call.finished, turn.started,
	// message.started(text), message.delta, message.finished(text),
	// turn.finished(end_turn) = 17 events. See TestToolCallReconstruction's
	// doc for why a round's turn.finished(tool_use) precedes its
	// tool.call.finished (the tool executes AFTER the requesting model-call
	// round settles, not during it).
	const wantEvents = 17
	sourceEvents := drainEvents(t, sourceSub, wantEvents)
	bridgeEvents := drainEvents(t, bridgeSub, wantEvents)

	var sawDelta, sawSpilledFinished bool
	for i, se := range sourceEvents {
		be := bridgeEvents[i]
		if se.Kind() != be.Kind() {
			t.Errorf("event %d: reconstructed Kind() = %q, want %q", i, be.Kind(), se.Kind())
			continue
		}
		if se.SessionID() != be.SessionID() {
			t.Errorf("event %d (%s): reconstructed SessionID() = %q, want %q", i, se.Kind(), be.SessionID(), se.SessionID())
		}
		wantFields := stripEnvelopeSeqTime(t, se)
		gotFields := stripEnvelopeSeqTime(t, be)
		if !reflect.DeepEqual(gotFields, wantFields) {
			t.Errorf("event %d (%s): reconstructed payload = %+v, want %+v", i, se.Kind(), gotFields, wantFields)
		}

		switch ev := se.(type) {
		case event.ToolCallDelta:
			sawDelta = true
		case event.ToolCallFinished:
			if ev.SpillPath != "" && ev.SpillBytes > 0 && ev.SpillSHA256 != "" && len(ev.Diagnostics) > 0 {
				sawSpilledFinished = true
			}
		}
	}

	if !sawDelta {
		t.Error("source stream never contained a tool.call.delta — test setup bug, not a fidelity failure")
	}
	if !sawSpilledFinished {
		t.Error("source stream never contained a spilled+diagnostic tool.call.finished — test setup bug, not a fidelity failure")
	}
}
