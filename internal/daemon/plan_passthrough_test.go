package daemon_test

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/provider"
)

// planProvider is a scripted [provider.Provider] whose first turn calls the
// update_plan builtin with a fixed set of entries, then a second turn ends. The
// SDK loop executes update_plan (it is on tool.Builtins, which the runner wires
// by default when opts.Tools is nil — see runner.New), records the validated
// plan on the tool result's Metadata, and bridges it to a PlanUpdated event,
// which acp.ToSessionUpdate projects to a `plan` session/update. The faux
// provider the other daemon tests use never calls a tool, so this test scripts
// its own provider — the same idiom usageProvider (usage_test.go) uses for its
// SDK-driven path.
type planProvider struct {
	entries []planCallEntry
	turn    int
}

// planCallEntry is one entry the scripted model puts in its update_plan call —
// the wire shape of the tool's "entries" input array (content/priority/status).
type planCallEntry struct {
	Content  string `json:"content"`
	Priority string `json:"priority"`
	Status   string `json:"status"`
}

func (*planProvider) Info() provider.ModelInfo { return provider.ModelInfo{ID: "plan-test"} }

func (p *planProvider) Stream(context.Context, provider.Request) (provider.StreamHandle, error) {
	p.turn++
	if p.turn == 1 {
		input, err := json.Marshal(struct {
			Entries []planCallEntry `json:"entries"`
		}{p.entries})
		if err != nil {
			return nil, err
		}
		return &planStream{input: input}, nil
	}
	// Second turn (after the tool result feeds back): end the turn cleanly.
	return &planStream{done: true}, nil
}

// planStream emits a single update_plan tool call (Start+End carrying the full
// entries input) followed by a tool_use stop, or — once done is set — a bare
// end_turn so the loop terminates after the tool result feeds back.
type planStream struct {
	input json.RawMessage
	done  bool
	n     int
}

func (s *planStream) Next() (provider.StreamEvent, error) {
	s.n++
	if s.done {
		if s.n == 1 {
			return provider.StreamEvent{Type: provider.StreamFinished, StopReason: provider.StopEndTurn}, nil
		}
		return provider.StreamEvent{}, io.EOF
	}
	switch s.n {
	case 1:
		return provider.StreamEvent{
			Type: provider.StreamToolCallStart,
			Tool: &provider.ToolCall{ID: "call-1", Name: "update_plan"},
		}, nil
	case 2:
		return provider.StreamEvent{
			Type: provider.StreamToolCallEnd,
			Tool: &provider.ToolCall{ID: "call-1", Name: "update_plan", Input: s.input},
		}, nil
	case 3:
		return provider.StreamEvent{Type: provider.StreamFinished, StopReason: provider.StopToolUse}, nil
	default:
		return provider.StreamEvent{}, io.EOF
	}
}

func (s *planStream) Close() error { return nil }

// planUpdateParams is the wire decode of a `plan` session/update: the
// discriminator plus the full ordered entries the peer receives. acp has no
// client-direction decode helper (it is written for gofer to play the agent),
// so — like sessionUpdateParams — this decodes the shape loosely.
type planUpdateParams struct {
	SessionID string `json:"sessionId"`
	Update    struct {
		SessionUpdate string          `json:"sessionUpdate"`
		Entries       []planCallEntry `json:"entries"`
	} `json:"update"`
}

// TestSessionPromptSurfacesPlan is the plan pass-through proof: a turn whose
// tool call is update_plan surfaces a `plan` session/update carrying the exact
// entries (content/priority/status, in order) to the peer — for free, via
// acp.ToSessionUpdate after the v0.9.0 re-pin. gofer builds no projection and
// synthesizes nothing; it forwards whatever ToSessionUpdate returns ok=true for.
//
// update_plan is not a sandbox-containable tool (it mutates nothing but is not
// in sandbox.containableTools), so the daemon gates it to an "ask": the driver
// answers the session/request_permission with allow-once, exactly as a real ACP
// client would, and the plan snapshot follows once the tool runs.
func TestSessionPromptSurfacesPlan(t *testing.T) {
	entries := []planCallEntry{
		{Content: "Explore the codebase", Priority: "high", Status: "in_progress"},
		{Content: "Write the pass-through test", Priority: "medium", Status: "pending"},
		{Content: "Run the quality gates", Priority: "low", Status: "pending"},
	}
	sup := newTestSupervisor(t, func() provider.Provider {
		return &planProvider{entries: entries}
	})
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	sid := newSession(t, c, t.TempDir())

	respCh := make(chan rpcFrame, 1)
	go func() {
		respCh <- c.request(acp.MethodSessionPrompt, acp.PromptRequest{
			SessionID: sid,
			Prompt:    []acp.ContentBlock{acp.TextBlock("plan your work")},
		})
	}()

	// The update_plan call gates to an ask: answer the session/request_permission
	// with allow-once so the tool runs and emits its plan. Answering runs in the
	// background so the main goroutine keeps draining notifications concurrently.
	go func() {
		id, _ := awaitPermissionRequest(t, c)
		c.respond(id, selectedResponse(string(acp.PermissionAllowOnce)))
	}()

	// Scan the notification stream for the `plan` update, skipping every
	// orthogonal frame that interleaves (the gofer/permission_requested
	// notification, tool_call / tool_call_update updates for the call itself,
	// message chunks). The plan snapshot is what we assert on.
	var plan *planUpdateParams
	for plan == nil {
		n := c.waitNotification()
		if n.Method != acp.MethodSessionUpdate {
			continue // e.g. gofer/permission_requested — not a session/update
		}
		var up planUpdateParams
		if err := json.Unmarshal(n.Params, &up); err != nil {
			continue // not a plan-shaped update (e.g. a tool_call update)
		}
		if up.Update.SessionUpdate == "plan" {
			plan = &up
		}
	}

	if plan.SessionID != sid {
		t.Errorf("plan sessionId = %q, want %q", plan.SessionID, sid)
	}
	if len(plan.Update.Entries) != len(entries) {
		t.Fatalf("plan entries = %d, want %d: %+v", len(plan.Update.Entries), len(entries), plan.Update.Entries)
	}
	for i, got := range plan.Update.Entries {
		want := entries[i]
		if got.Content != want.Content {
			t.Errorf("entry[%d].content = %q, want %q", i, got.Content, want.Content)
		}
		if got.Priority != want.Priority {
			t.Errorf("entry[%d].priority = %q, want %q", i, got.Priority, want.Priority)
		}
		if got.Status != want.Status {
			t.Errorf("entry[%d].status = %q, want %q", i, got.Status, want.Status)
		}
	}

	if final := <-respCh; final.Error != nil {
		t.Fatalf("session/prompt error: %+v", final.Error)
	}
}
