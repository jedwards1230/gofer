package daemon_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jedwards1230/agent-sdk-go/acp"
)

// TestInitializeAdvertisesLoadAndListCapabilities asserts the initialize
// response's AgentCapabilities advertise both session/load (with replay) and
// session/list, per handleInitialize.
func TestInitializeAdvertisesLoadAndListCapabilities(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	resp := c.request(acp.MethodInitialize, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersion})
	if resp.Error != nil {
		t.Fatalf("initialize error: %+v", resp.Error)
	}

	raw := string(resp.Result)
	if !strings.Contains(raw, `"loadSession":true`) {
		t.Errorf("result missing loadSession:true: %s", raw)
	}
	if !strings.Contains(raw, `"sessionCapabilities":{"list":{}}`) {
		t.Errorf("result missing sessionCapabilities.list: %s", raw)
	}

	var got acp.InitializeResponse
	if err := json.Unmarshal(resp.Result, &got); err != nil {
		t.Fatalf("unmarshal InitializeResponse: %v", err)
	}
	if !got.AgentCapabilities.LoadSession {
		t.Error("AgentCapabilities.LoadSession = false, want true")
	}
	if got.AgentCapabilities.SessionCapabilities.List == nil {
		t.Error("AgentCapabilities.SessionCapabilities.List = nil, want non-nil")
	}
}

// rawFrame is the same shape as rpcFrame (see harness_test.go), duplicated
// locally only so this file's raw, single-goroutine frame reader stays
// self-contained and obviously doesn't share state with wsClient's demuxing
// goroutine.
type rawFrame = rpcFrame

// readRawFrame reads and decodes exactly one frame from conn.
func readRawFrame(t *testing.T, ctx context.Context, conn *websocket.Conn) rawFrame {
	t.Helper()
	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var f rawFrame
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	return f
}

// TestSessionLoadReplaysHistoryBeforeResponse is the ordering-contract test:
// it drives a session/load over its OWN dedicated connection, reading frames
// off the wire one at a time in strict arrival order (bypassing wsClient's
// notification/response demuxing, which would obscure relative ordering
// across its two separate channels). Every frame read before the
// session/load response must be a session/update replay notification; the
// response must be the last frame read. It also asserts the replayed
// notifications' content matches [acp.ReplayNotifications] over the
// session's folded history, in order.
func TestSessionLoadReplaysHistoryBeforeResponse(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")

	// First connection: create the session and drive one full prompt so the
	// journal has settled user + assistant history to replay.
	setupConn := dial(t, context.Background(), url, nil)
	cwd := t.TempDir()
	sid := newSession(t, setupConn, cwd)

	promptResp := setupConn.request(acp.MethodSessionPrompt, acp.PromptRequest{
		SessionID: sid,
		Prompt:    []acp.ContentBlock{acp.TextBlock("hi")},
	})
	if promptResp.Error != nil {
		t.Fatalf("session/prompt error: %+v", promptResp.Error)
	}

	// The oracle: compute the exact replay notifications ReplayNotifications
	// would build from this session's current folded history, independent of
	// the daemon's handler, so the wire assertions below aren't just checking
	// the handler against itself.
	//
	// session/prompt returns on the turn.finished EVENT, but the runner
	// journals the turn asynchronously — so the oracle (this History read) and
	// the session/load replay below must both observe a SETTLED journal to
	// agree. Reading mid-journal (e.g. only the user message present) is what
	// made this flake in CI. Poll until the folded history stops growing.
	msgs, err := sup.History(context.Background(), sid)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	for prev := -1; len(msgs) != prev; {
		prev = len(msgs)
		time.Sleep(20 * time.Millisecond)
		if msgs, err = sup.History(context.Background(), sid); err != nil {
			t.Fatalf("History: %v", err)
		}
	}
	wantNotifs := acp.ReplayNotifications(sid, msgs)
	if len(wantNotifs) == 0 {
		t.Fatal("expected at least one replay notification from the scripted turn")
	}

	// Second, dedicated raw connection for session/load: read frames myself,
	// in strict wire order, rather than going through wsClient's
	// notification/response split.
	loadConn, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = loadConn.Close(websocket.StatusNormalClosure, "") })
	ctx := context.Background()

	req := struct {
		JSONRPC string                 `json:"jsonrpc"`
		ID      int                    `json:"id"`
		Method  string                 `json:"method"`
		Params  acp.LoadSessionRequest `json:"params"`
	}{jsonrpcVersion, 1, acp.MethodSessionLoad, acp.LoadSessionRequest{SessionID: sid, Cwd: cwd}}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal session/load request: %v", err)
	}
	if err := loadConn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write session/load request: %v", err)
	}

	var gotNotifs []rawFrame
	var gotGoferEvents []rawFrame
	var resp rawFrame
	for {
		f := readRawFrame(t, ctx, loadConn)
		switch f.Method {
		case acp.MethodSessionUpdate:
			gotNotifs = append(gotNotifs, f)
			continue
		case "gofer/event":
			// The M3 lossless-attach replay (internal/daemon/handlers.go's
			// historyEvents) ALSO fans this session's history as gofer/event
			// frames alongside the ACP session/update replay this test's
			// oracle checks — this test cares about the ACP projection only,
			// so these are counted (proving they still precede the response)
			// but not otherwise asserted; see fanout_test.go for the
			// dedicated gofer/event history-replay proof.
			gotGoferEvents = append(gotGoferEvents, f)
			continue
		}
		// The first frame with no method is the session/load response — per
		// the ordering contract, it must be the LAST frame on the wire, so we
		// stop reading here.
		resp = f
		break
	}
	if len(gotGoferEvents) == 0 {
		t.Error("got 0 gofer/event replay frames, want at least one (historyEvents mirrors ReplayNotifications)")
	}

	if resp.Error != nil {
		t.Fatalf("session/load error: %+v", resp.Error)
	}
	var loadResult acp.LoadSessionResponse
	if err := json.Unmarshal(resp.Result, &loadResult); err != nil {
		t.Fatalf("unmarshal LoadSessionResponse: %v", err)
	}

	if len(gotNotifs) != len(wantNotifs) {
		t.Fatalf("got %d replay notifications, want %d (got=%+v)", len(gotNotifs), len(wantNotifs), gotNotifs)
	}
	for i, want := range wantNotifs {
		wantBytes, err := json.Marshal(want)
		if err != nil {
			t.Fatalf("marshal want[%d]: %v", i, err)
		}
		var wantAny, gotAny any
		if err := json.Unmarshal(wantBytes, &wantAny); err != nil {
			t.Fatalf("unmarshal want[%d]: %v", i, err)
		}
		if err := json.Unmarshal(gotNotifs[i].Params, &gotAny); err != nil {
			t.Fatalf("unmarshal got[%d]: %v", i, err)
		}
		if !reflect.DeepEqual(gotAny, wantAny) {
			t.Errorf("notification %d = %+v, want %+v", i, gotAny, wantAny)
		}
	}
}

// TestSessionList covers the session/list wire shape: a non-nil sessions
// array, a cwd filter, and opaque cursor pagination (page-size boundary and
// an invalid cursor).
func TestSessionList(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	t.Run("empty roster returns non-nil empty slice", func(t *testing.T) {
		resp := c.request(acp.MethodSessionList, acp.ListSessionsRequest{})
		if resp.Error != nil {
			t.Fatalf("session/list error: %+v", resp.Error)
		}
		if strings.Contains(string(resp.Result), `"sessions":null`) {
			t.Errorf("sessions marshaled as null, want []: %s", resp.Result)
		}
		var got acp.ListSessionsResponse
		if err := json.Unmarshal(resp.Result, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Sessions == nil || len(got.Sessions) != 0 {
			t.Errorf("Sessions = %+v, want empty non-nil slice", got.Sessions)
		}
	})

	cwdA := t.TempDir()
	cwdB := t.TempDir()
	sidA1 := newSession(t, c, cwdA)
	sidA2 := newSession(t, c, cwdA)
	sidB := newSession(t, c, cwdB)

	t.Run("lists sessionId and cwd for live sessions", func(t *testing.T) {
		resp := c.request(acp.MethodSessionList, acp.ListSessionsRequest{})
		if resp.Error != nil {
			t.Fatalf("session/list error: %+v", resp.Error)
		}
		var got acp.ListSessionsResponse
		if err := json.Unmarshal(resp.Result, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		ids := make(map[string]string, len(got.Sessions))
		for _, s := range got.Sessions {
			ids[s.SessionID] = s.Cwd
		}
		for _, sid := range []string{sidA1, sidA2, sidB} {
			if _, ok := ids[sid]; !ok {
				t.Errorf("session/list missing %s: %+v", sid, got.Sessions)
			}
		}
		if ids[sidA1] != cwdA {
			t.Errorf("sidA1 cwd = %q, want %q", ids[sidA1], cwdA)
		}
		if ids[sidB] != cwdB {
			t.Errorf("sidB cwd = %q, want %q", ids[sidB], cwdB)
		}
	})

	t.Run("req.Cwd is ignored: listing is fleet-global", func(t *testing.T) {
		// Listing is fleet-global (see handleSessionList): req.Cwd is accepted
		// for wire compatibility but no longer hides sessions in other
		// directories. A request naming cwdA still returns every session,
		// including sidB in cwdB, each with its own Cwd intact.
		resp := c.request(acp.MethodSessionList, acp.ListSessionsRequest{Cwd: cwdA})
		if resp.Error != nil {
			t.Fatalf("session/list error: %+v", resp.Error)
		}
		var got acp.ListSessionsResponse
		if err := json.Unmarshal(resp.Result, &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		ids := make(map[string]string, len(got.Sessions))
		for _, s := range got.Sessions {
			ids[s.SessionID] = s.Cwd
		}
		for _, sid := range []string{sidA1, sidA2, sidB} {
			if _, ok := ids[sid]; !ok {
				t.Errorf("session/list with req.Cwd=%s missing %s: %+v", cwdA, sid, got.Sessions)
			}
		}
		if ids[sidB] != cwdB {
			t.Errorf("sidB cwd = %q, want %q (its own cwd, not the filter)", ids[sidB], cwdB)
		}
	})

	t.Run("invalid cursor is an error", func(t *testing.T) {
		resp := c.request(acp.MethodSessionList, acp.ListSessionsRequest{Cursor: "not-valid-base64!!"})
		if resp.Error == nil {
			t.Fatal("invalid cursor: want an error, got none")
		}
		if resp.Error.Code != -32602 {
			t.Errorf("error code = %d, want -32602 (invalid params)", resp.Error.Code)
		}
	})
}

// TestSessionListPagination asserts a full page returns a NextCursor and the
// second page (requested via that cursor) resumes where the first left off,
// covering exactly [sessionListPageSize]+1 sessions so the boundary is
// exercised with a minimal roster.
func TestSessionListPagination(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	_, url := newTestDaemon(t, sup, "")
	c := dial(t, context.Background(), url, nil)

	const pageSize = 50
	all := make(map[string]bool, pageSize+1)
	for i := 0; i < pageSize+1; i++ {
		sid := newSession(t, c, t.TempDir())
		all[sid] = true
	}

	resp := c.request(acp.MethodSessionList, acp.ListSessionsRequest{})
	if resp.Error != nil {
		t.Fatalf("session/list page 1 error: %+v", resp.Error)
	}
	var page1 acp.ListSessionsResponse
	if err := json.Unmarshal(resp.Result, &page1); err != nil {
		t.Fatalf("unmarshal page1: %v", err)
	}
	if len(page1.Sessions) != pageSize {
		t.Fatalf("page1 sessions = %d, want %d", len(page1.Sessions), pageSize)
	}
	if page1.NextCursor == "" {
		t.Fatal("page1 NextCursor is empty, want a token for the remaining session")
	}

	resp = c.request(acp.MethodSessionList, acp.ListSessionsRequest{Cursor: page1.NextCursor})
	if resp.Error != nil {
		t.Fatalf("session/list page 2 error: %+v", resp.Error)
	}
	var page2 acp.ListSessionsResponse
	if err := json.Unmarshal(resp.Result, &page2); err != nil {
		t.Fatalf("unmarshal page2: %v", err)
	}
	if len(page2.Sessions) != 1 {
		t.Fatalf("page2 sessions = %d, want 1", len(page2.Sessions))
	}
	if page2.NextCursor != "" {
		t.Errorf("page2 NextCursor = %q, want empty (last page)", page2.NextCursor)
	}

	seen := make(map[string]bool, pageSize+1)
	for _, s := range page1.Sessions {
		seen[s.SessionID] = true
	}
	for _, s := range page2.Sessions {
		if seen[s.SessionID] {
			t.Errorf("session %s appeared on both pages", s.SessionID)
		}
		seen[s.SessionID] = true
	}
	if len(seen) != pageSize+1 {
		t.Errorf("total distinct sessions across pages = %d, want %d", len(seen), pageSize+1)
	}
	for sid := range all {
		if !seen[sid] {
			t.Errorf("session %s missing from both pages", sid)
		}
	}
}
