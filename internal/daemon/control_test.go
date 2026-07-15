package daemon_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/provider"
)

// sessionInfoWire mirrors the daemon's gofer/roster and gofer/ps wire shape
// (see internal/daemon/wire.go's sessionInfoDTO), decoded loosely for
// assertions.
type sessionInfoWire struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Model  string `json:"model"`
	Live   bool   `json:"live"`
}

// TestGoferRosterAndKill covers gofer/roster (live sessions only) and
// gofer/kill: after killing a session it drops from the roster but still
// appears (Live=false) via gofer/ps, since the journal is never deleted.
func TestGoferRosterAndKill(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	sid := newSession(t, c, t.TempDir())

	resp := c.request("gofer/roster", nil)
	if resp.Error != nil {
		t.Fatalf("gofer/roster error: %+v", resp.Error)
	}
	var roster []sessionInfoWire
	if err := json.Unmarshal(resp.Result, &roster); err != nil {
		t.Fatalf("unmarshal roster: %v", err)
	}
	if len(roster) != 1 || roster[0].ID != sid || !roster[0].Live || roster[0].Status != "needs-input" {
		t.Fatalf("roster = %+v, want one live needs-input entry for %s", roster, sid)
	}

	killResp := c.request("gofer/kill", map[string]string{"sessionId": sid})
	if killResp.Error != nil {
		t.Fatalf("gofer/kill error: %+v", killResp.Error)
	}

	resp = c.request("gofer/roster", nil)
	if err := json.Unmarshal(resp.Result, &roster); err != nil {
		t.Fatalf("unmarshal roster after kill: %v", err)
	}
	if len(roster) != 0 {
		t.Fatalf("roster after kill = %+v, want empty", roster)
	}

	psResp := c.request("gofer/ps", nil)
	if psResp.Error != nil {
		t.Fatalf("gofer/ps error: %+v", psResp.Error)
	}
	var ps []sessionInfoWire
	if err := json.Unmarshal(psResp.Result, &ps); err != nil {
		t.Fatalf("unmarshal ps: %v", err)
	}
	var found bool
	for _, e := range ps {
		if e.ID == sid {
			found = true
			if e.Live {
				t.Errorf("ps entry for killed session Live = true, want false")
			}
		}
	}
	if !found {
		t.Fatalf("ps missing killed session %s: %+v", sid, ps)
	}
}

// TestGoferKillUnknownSession asserts killing an id the supervisor has never
// seen surfaces as an application error, not a silent success.
func TestGoferKillUnknownSession(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	resp := c.request("gofer/kill", map[string]string{"sessionId": "does-not-exist"})
	if resp.Error == nil {
		t.Fatal("kill unknown session: want an error, got none")
	}
}

// TestGoferArchiveIdle covers gofer/archive on an idle (needs-input) session:
// it succeeds and drops the session from the roster.
func TestGoferArchiveIdle(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	sid := newSession(t, c, t.TempDir())

	resp := c.request("gofer/archive", map[string]string{"sessionId": sid})
	if resp.Error != nil {
		t.Fatalf("gofer/archive error: %+v", resp.Error)
	}

	rosterResp := c.request("gofer/roster", nil)
	var roster []sessionInfoWire
	if err := json.Unmarshal(rosterResp.Result, &roster); err != nil {
		t.Fatalf("unmarshal roster: %v", err)
	}
	if len(roster) != 0 {
		t.Fatalf("roster after archive = %+v, want empty", roster)
	}
}

// TestGoferArchiveRunningRejected covers gofer/archive on a session with a
// turn in flight: it is rejected with a clear "running" application error
// rather than silently discarding the in-flight work (mirrors
// [supervisor.ErrRunning]).
func TestGoferArchiveRunningRejected(t *testing.T) {
	bp := newBlockingProvider()
	sup := newTestSupervisor(t, func() provider.Provider { return bp })
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	sid := newSession(t, c, t.TempDir())

	respCh := make(chan rpcFrame, 1)
	go func() {
		respCh <- c.request(acp.MethodSessionPrompt, acp.PromptRequest{
			SessionID: sid,
			Prompt:    []acp.ContentBlock{acp.TextBlock("hi")},
		})
	}()
	<-bp.started // the turn is genuinely in flight

	archResp := c.request("gofer/archive", map[string]string{"sessionId": sid})
	if archResp.Error == nil {
		t.Fatal("archive while running: want an error, got none")
	}
	if !strings.Contains(archResp.Error.Message, "running") {
		t.Errorf("archive-while-running error = %q, want it to mention \"running\"", archResp.Error.Message)
	}

	// Unblock the turn so the request goroutine settles cleanly.
	c.notify(acp.MethodSessionCancel, acp.CancelNotification{SessionID: sid})
	<-respCh
}
