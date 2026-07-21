package daemon_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// settleSupervisor is a fake [daemon.Supervisor] that models the issue #137
// race at the seam handleSessionLoad crosses: History returns a SHORT fold (user
// + reasoning only) until AwaitSettled has run, and the COMPLETE fold (with the
// assistant's text block) afterwards. Because the settle wait is the ONLY thing
// that flips it, a session/load that folds the complete transcript proves the
// handler waited before reading — and reverting the AwaitSettled call in
// handleSessionLoad makes this test fail (the mutation check).
//
// It stands in for the async-journaling window a real runner opens (a turn's
// assistant/tool entries land after the turn.finished the client already saw),
// deterministically, so the assertion is a claim about the handler's ordering
// rather than a timing coincidence.
type settleSupervisor struct {
	mu      sync.Mutex
	settled bool
}

const (
	settleSessionID  = "sess-137"
	settleAnswerText = "the-answer-only-in-the-complete-fold"
)

// AwaitSettled records that the settle wait ran. handleSessionLoad must call it
// before History.
func (s *settleSupervisor) AwaitSettled(_ context.Context, _ string) error {
	s.mu.Lock()
	s.settled = true
	s.mu.Unlock()
	return nil
}

// History returns the complete fold only if AwaitSettled has already run,
// otherwise the short (mid-journal) one — the crux of the mutation check.
func (s *settleSupervisor) History(_ context.Context, _ string) ([]provider.Message, error) {
	s.mu.Lock()
	settled := s.settled
	s.mu.Unlock()

	assistant := provider.Message{Role: provider.RoleAssistant, Content: []provider.ContentBlock{
		{Type: provider.BlockReasoning, Text: "thinking"},
	}}
	if settled {
		assistant.Content = append(assistant.Content, provider.ContentBlock{Type: provider.BlockText, Text: settleAnswerText})
	}
	return []provider.Message{
		provider.UserText("hi"),
		assistant,
	}, nil
}

func (s *settleSupervisor) Resume(_ context.Context, id string, _ supervisor.ResumeOptions) (supervisor.SessionInfo, error) {
	return supervisor.SessionInfo{ID: id, Live: true, Status: supervisor.StatusNeedsInput}, nil
}

// The remaining interface methods are unused on the session/load path and are
// stubbed to satisfy [daemon.Supervisor].
func (s *settleSupervisor) Create(context.Context, string, supervisor.CreateOptions) (supervisor.SessionInfo, error) {
	return supervisor.SessionInfo{}, nil
}
func (s *settleSupervisor) Interrupt(context.Context, string) error { return nil }
func (s *settleSupervisor) List(context.Context) ([]supervisor.SessionInfo, error) {
	return nil, nil
}
func (s *settleSupervisor) Roster(context.Context) ([]supervisor.SessionInfo, error) {
	return nil, nil
}
func (s *settleSupervisor) SetModel(context.Context, string, string) error { return nil }
func (s *settleSupervisor) SubscribeLive(context.Context, string) (*event.Subscription, error) {
	return nil, nil
}
func (s *settleSupervisor) Send(context.Context, string, string) error           { return nil }
func (s *settleSupervisor) Reply(string, event.PermissionReply) error            { return nil }
func (s *settleSupervisor) EmitConfigOptions(string, []event.ConfigOption) error { return nil }
func (s *settleSupervisor) Kill(context.Context, string) error                   { return nil }
func (s *settleSupervisor) Archive(context.Context, string) error                { return nil }

// TestSessionLoadWaitsForCompleteHistory is the issue #137 regression test: a
// session/load must wait for the journaling window to close (AwaitSettled)
// BEFORE it folds and replays history, so a client loading right after
// turn.finished receives the COMPLETE transcript rather than a short one missing
// the assistant's text. Driven deterministically via [settleSupervisor], whose
// History is short until the settle wait runs — so the presence of the
// assistant's text block in the replayed session/update stream is proof the
// handler waited. Reverting the AwaitSettled call in handleSessionLoad turns
// this red.
func TestSessionLoadWaitsForCompleteHistory(t *testing.T) {
	sup := &settleSupervisor{}
	d := daemon.New(sup, daemon.Config{DefaultModel: "faux"})
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	url := "ws" + srv.URL[len("http"):]

	c := dial(t, context.Background(), url, nil)
	cwd := t.TempDir()

	resp := c.request(acp.MethodSessionLoad, acp.LoadSessionRequest{SessionID: settleSessionID, Cwd: cwd})
	if resp.Error != nil {
		t.Fatalf("session/load error: %+v", resp.Error)
	}

	// The replay notifications precede the response on the wire and are all
	// buffered by the time the synchronous request above returns (readLoop is the
	// connection's only reader — see the harness). Drain and look for the
	// assistant's text chunk, which only the COMPLETE fold carries.
	if !replayHasAgentText(t, c, settleAnswerText) {
		t.Fatalf("session/load replay missing the assistant text block %q — it folded a SHORT (mid-journal) history; AwaitSettled did not gate the read (issue #137)", settleAnswerText)
	}
}

// replayHasAgentText drains c's buffered session/update notifications and reports
// whether any is an agent_message_chunk carrying want.
func replayHasAgentText(t *testing.T, c *wsClient, want string) bool {
	t.Helper()
	for {
		select {
		case f, ok := <-c.notifications:
			if !ok {
				return false
			}
			if f.Method != acp.MethodSessionUpdate {
				continue
			}
			var n struct {
				Update struct {
					SessionUpdate string `json:"sessionUpdate"`
					Content       struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"update"`
			}
			if err := json.Unmarshal(f.Params, &n); err != nil {
				continue
			}
			if n.Update.SessionUpdate == "agent_message_chunk" && n.Update.Content.Text == want {
				return true
			}
		default:
			return false
		}
	}
}
