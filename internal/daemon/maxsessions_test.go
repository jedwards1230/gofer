package daemon_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/acp"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// TestMaxSessionsCapRejectsSecond proves the M6 worker cap: with
// MaxSessions=1 a second session/new is refused with a clean application
// error while the first session stays live, and with MaxSessions=0 (the
// default) an unbounded number of sessions are created as before.
func TestMaxSessionsCapRejectsSecond(t *testing.T) {
	t.Run("cap of one rejects the second", func(t *testing.T) {
		sup := newTestSupervisor(t, fauxProvider)
		_, url := newTestDaemonWithConfig(t, sup, daemon.Config{DefaultModel: "faux", MaxSessions: 1})
		c := dial(t, context.Background(), url, nil)

		// First session/new succeeds and occupies the sole slot.
		sid := newSession(t, c, t.TempDir())
		if sid == "" {
			t.Fatal("first session/new returned an empty id")
		}

		// Second session/new is refused with an application error naming the cap.
		resp := c.request(acp.MethodSessionNew, acp.NewSessionRequest{Cwd: t.TempDir()})
		if resp.Error == nil {
			t.Fatalf("second session/new succeeded, want a session-limit error; result=%s", resp.Result)
		}
		if !strings.Contains(resp.Error.Message, "session limit reached") {
			t.Errorf("second session/new error = %q, want it to mention the session limit", resp.Error.Message)
		}

		// The first session is untouched: it remains the sole live roster entry
		// (the rejected second new added nothing).
		roster := c.request("gofer/roster", nil)
		if roster.Error != nil {
			t.Fatalf("gofer/roster error: %+v", roster.Error)
		}
		var rows []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(roster.Result, &rows); err != nil {
			t.Fatalf("decode gofer/roster result: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("roster has %d live sessions after the rejected second new, want exactly 1", len(rows))
		}
		if rows[0].ID != sid {
			t.Errorf("sole roster session id = %q, want the first session %q", rows[0].ID, sid)
		}
	})

	t.Run("default (zero) is unlimited", func(t *testing.T) {
		sup := newTestSupervisor(t, fauxProvider)
		_, url := newTestDaemonWithConfig(t, sup, daemon.Config{DefaultModel: "faux"})
		c := dial(t, context.Background(), url, nil)

		// Several session/new calls all succeed: the default cap of 0 imposes
		// no limit, matching today's `gofer daemon` behavior exactly.
		for i := 0; i < 3; i++ {
			if id := newSession(t, c, t.TempDir()); id == "" {
				t.Fatalf("session/new %d returned an empty id", i)
			}
		}
	})
}
