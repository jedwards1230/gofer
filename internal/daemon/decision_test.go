package daemon_test

// decision_test.go covers the gofer-native half of the structured-decision
// relay: the gofer/decision_requested fan-out, the decision.answer op that
// routes an answer back into the session's gate, replay-on-attach, and route
// cleanup. The spec-ACP half (session/request_decision) is in
// acp_decision_test.go, modeled on the same split the permission suites use.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/loop"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/decision"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// askUserCall is the tool input every decisionSession asks with: one question
// with two options, so the gate mints the ids q1 / q1o1 / q1o2 (position
// derived — see decision.AssignIDs) and a test can name them literally.
const askUserCall = `{"questions":[{
	"title":"Migration strategy",
	"question":"Which approach should I take?",
	"options":[
		{"label":"In-place ALTER","rationale":"fastest, but locks the table"},
		{"label":"Shadow table + backfill","rationale":"online, but doubles disk","recommended":true}
	]
}]}`

// decisionSession is a supervisor.Session whose Prompt runs the REAL ask_user
// tool off the registry the supervisor built for it — so the request travels
// the real decision.Gate, with the real id assignment and the real answer
// validation, and blocks the turn exactly as a model-issued tool call would.
// The tool's outcome is published to results (or errs), and a terminal
// turn.finished is emitted either way so the driving session/prompt returns.
type decisionSession struct {
	id     string
	path   string
	broker *event.Broker
	ask    loop.Tool

	// results / errs carry the ask_user outcome to the test. Both are buffered
	// and sent to NON-blockingly: a test that does not read one (the interrupt
	// case reads only errs) must not wedge the supervisor's pump goroutine at
	// teardown.
	results chan loop.ToolResult
	errs    chan error
}

func newDecisionSession(id, path string, tools loop.ToolRegistry) (*decisionSession, error) {
	ask, ok := tools.Get(decision.ToolName)
	if !ok {
		return nil, fmt.Errorf("supervisor registry has no %s tool", decision.ToolName)
	}
	return &decisionSession{
		id:      id,
		path:    path,
		broker:  event.NewBroker(event.WithReplay(64)),
		ask:     ask,
		results: make(chan loop.ToolResult, 1),
		errs:    make(chan error, 1),
	}, nil
}

func (f *decisionSession) ID() string               { return f.id }
func (f *decisionSession) JournalPath() string      { return f.path }
func (f *decisionSession) Fold() []provider.Message { return nil }
func (f *decisionSession) Events() *event.Subscription {
	return f.broker.Subscribe(event.FilterAll, 64)
}
func (f *decisionSession) EventsLive() *event.Subscription {
	return f.broker.SubscribeLive(event.FilterAll, 64)
}
func (f *decisionSession) Emit(e event.Event)       { f.broker.Publish(e) }
func (f *decisionSession) Cost() session.CostReport { return session.CostReport{} }
func (f *decisionSession) SetModel(string) error    { return nil }
func (f *decisionSession) SetEffort(string) error   { return nil }

func (f *decisionSession) Close() error {
	f.broker.Close()
	return nil
}

func (f *decisionSession) Prompt(ctx context.Context, _ string) error {
	res, err := f.ask.Run(ctx, json.RawMessage(askUserCall))
	if err != nil {
		select {
		case f.errs <- err:
		default:
		}
		f.broker.Publish(event.NewTurnFinished(f.id, "cancelled", provider.Usage{}))
		return err
	}
	select {
	case f.results <- res:
	default:
	}
	f.broker.Publish(event.NewTurnFinished(f.id, "end_turn", provider.Usage{}))
	return nil
}

// decisionHarness is the approval harness's decision-side twin: a Supervisor
// whose sessions are decisionSessions, behind an in-process daemon, WITH the
// daemon installed as the supervisor's decision relay.
//
// Installing the relay is not incidental setup — it is the feature. A decision
// rides no event stream, so the supervisor's standing per-session gate watcher
// is the daemon's only way to observe one; without SetDecisionRelay every
// ask_user here would return "no client attached" instead of reaching the wire.
type decisionHarness struct {
	sup *supervisor.Supervisor
	d   *daemon.Daemon
	url string

	mu     sync.Mutex
	fakes  map[string]*decisionSession
	nextID int64
}

func newDecisionHarness(t *testing.T) *decisionHarness {
	t.Helper()
	root := t.TempDir()
	h := &decisionHarness{fakes: make(map[string]*decisionSession)}

	build := func(id, cwd string, tools loop.ToolRegistry) (supervisor.Session, error) {
		path := filepath.Join(root, "sessions", session.Slugify(cwd), id+".jsonl")
		fs, err := newDecisionSession(id, path, tools)
		if err != nil {
			return nil, err
		}
		h.mu.Lock()
		h.fakes[id] = fs
		h.mu.Unlock()
		return fs, nil
	}

	sup, err := supervisor.New(supervisor.Config{
		Root: root,
		NewSession: func(_ context.Context, opts runner.Options) (supervisor.Session, error) {
			id := fmt.Sprintf("sess-%d", atomic.AddInt64(&h.nextID, 1))
			return build(id, opts.Cwd, opts.Tools)
		},
		ResumeSession: func(_ context.Context, id string, opts runner.Options) (supervisor.Session, error) {
			return build(id, opts.Cwd, opts.Tools)
		},
	})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })
	h.sup = sup

	d := daemon.New(sup, daemon.Config{DefaultModel: "faux"})
	// Before any session exists, mirroring cmd/gofer's own ordering.
	sup.SetDecisionRelay(d)
	h.d = d
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	h.url = "ws" + srv.URL[len("http"):]
	return h
}

func (h *decisionHarness) fake(id string) *decisionSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fakes[id]
}

// decisionRequestFrame is the subset of a gofer/decision_requested notification
// a test asserts on and answers.
type decisionRequestFrame struct {
	SessionID string `json:"sessionId"`
	ID        string `json:"id"`
	Questions []struct {
		QuestionID    string `json:"questionId"`
		Title         string `json:"title"`
		Question      string `json:"question"`
		AllowFreeText bool   `json:"allowFreeText"`
		AllowChat     bool   `json:"allowChat"`
		Options       []struct {
			OptionID    string `json:"optionId"`
			Label       string `json:"label"`
			Rationale   string `json:"rationale"`
			Recommended bool   `json:"recommended"`
		} `json:"options"`
	} `json:"questions"`
}

// awaitDecisionRequest drains c's notifications until a gofer/decision_requested
// arrives and decodes it.
func awaitDecisionRequest(t *testing.T, c *wsClient) decisionRequestFrame {
	t.Helper()
	frame := waitForNotificationMethod(t, c, daemon.MethodGoferDecisionRequested)
	var req decisionRequestFrame
	if err := json.Unmarshal(frame.Params, &req); err != nil {
		t.Fatalf("decode %s params: %v", daemon.MethodGoferDecisionRequested, err)
	}
	return req
}

// selectAnswer builds a decision.answer params object selecting optionID for
// question questionID, with an optional free-text note attached.
func selectAnswer(sessionID, requestID, questionID, optionID, notes string) map[string]any {
	answer := map[string]any{
		"questionId": questionID,
		"outcome":    map[string]any{"outcome": "selected", "optionId": optionID},
	}
	if notes != "" {
		answer["notes"] = notes
	}
	return map[string]any{
		"sessionId": sessionID,
		"id":        requestID,
		"answers":   []any{answer},
	}
}

// waitOutstandingDecisionReqs polls until the daemon's outstanding
// session/request_decision count reaches want (cancellation on resolution is
// asynchronous).
func waitOutstandingDecisionReqs(t *testing.T, h *decisionHarness, want int) {
	t.Helper()
	deadline := time.Now().Add(defaultWait)
	var last int
	for time.Now().Before(deadline) {
		last = h.d.OutstandingDecisionRequestCount()
		if last == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("outstanding decision requests = %d, want %d", last, want)
}

// waitOpenDecisions polls until the daemon's open-decision route count reaches
// want.
func waitOpenDecisions(t *testing.T, h *decisionHarness, want int) {
	t.Helper()
	deadline := time.Now().Add(defaultWait)
	var last int
	for time.Now().Before(deadline) {
		last = h.d.OpenDecisionCount()
		if last == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("open decision routes = %d, want %d", last, want)
}

// TestDecisionRoundTripCrossPeer is the acceptance test: two peers attach, one
// drives a turn that asks a structured question, BOTH peers receive the
// gofer/decision_requested (fan-out), and the NON-originating peer answers —
// the blocked ask_user returns THAT peer's answer, notes and all, and both peers
// see the resolution.
func TestDecisionRoundTripCrossPeer(t *testing.T) {
	h := newDecisionHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cwd := t.TempDir()
	driver := dial(t, ctx, h.url, nil) // the "laptop" that drives the turn
	phone := dial(t, ctx, h.url, nil)  // the non-originating "phone" that answers

	sid := newACPSession(t, driver, cwd)
	if lr := phone.request("session/load", map[string]any{"sessionId": sid, "cwd": cwd}); lr.Error != nil {
		t.Fatalf("session/load: %v", lr.Error)
	}

	promptDone := make(chan rpcFrame, 1)
	go func() {
		promptDone <- driver.request("session/prompt", map[string]any{"sessionId": sid, "text": "migrate the table"})
	}()

	// Fan-out: BOTH peers must receive the request, with the same id and the
	// gate-assigned question/option ids.
	driverReq := awaitDecisionRequest(t, driver)
	phoneReq := awaitDecisionRequest(t, phone)

	if phoneReq.SessionID != sid {
		t.Fatalf("decision_requested sessionId = %q, want %q", phoneReq.SessionID, sid)
	}
	if phoneReq.ID != "dec-1" {
		t.Fatalf("decision_requested id = %q, want the gate's first id %q", phoneReq.ID, "dec-1")
	}
	if driverReq.ID != phoneReq.ID {
		t.Fatalf("driver saw request id %q, phone saw %q — fan-out must deliver the same request", driverReq.ID, phoneReq.ID)
	}
	if len(phoneReq.Questions) != 1 {
		t.Fatalf("decision_requested carried %d questions, want 1", len(phoneReq.Questions))
	}
	q := phoneReq.Questions[0]
	if q.QuestionID != "q1" || q.Title != "Migration strategy" || q.Question != "Which approach should I take?" {
		t.Fatalf("question = %+v, want q1 / the harness's title and text", q)
	}
	// The escape hatches default to true and must survive the wire, or a client
	// would render a forced choice the agent never asked for.
	if !q.AllowFreeText || !q.AllowChat {
		t.Fatalf("allowFreeText=%v allowChat=%v, want both true (the opt-out defaults)", q.AllowFreeText, q.AllowChat)
	}
	if len(q.Options) != 2 {
		t.Fatalf("question carried %d options, want 2", len(q.Options))
	}
	if q.Options[0].OptionID != "q1o1" || q.Options[1].OptionID != "q1o2" {
		t.Fatalf("option ids = %q,%q, want q1o1,q1o2", q.Options[0].OptionID, q.Options[1].OptionID)
	}
	if q.Options[1].Label != "Shadow table + backfill" || !q.Options[1].Recommended ||
		q.Options[0].Rationale != "fastest, but locks the table" {
		t.Fatalf("options lost their labels/rationales/recommended flag: %+v", q.Options)
	}

	// The NON-originating peer answers, with a note attached.
	phone.notify(daemon.MethodDecisionAnswer, selectAnswer(sid, phoneReq.ID, "q1", "q1o2", "do it overnight"))

	// The blocked ask_user returned THAT answer.
	select {
	case res := <-h.fake(sid).results:
		if res.IsError {
			t.Fatalf("ask_user returned an error result: %s", res.Content)
		}
		if !strings.Contains(res.Content, "q1o2") || !strings.Contains(res.Content, "Shadow table + backfill") {
			t.Fatalf("tool result = %q, want the option the phone selected", res.Content)
		}
		if !strings.Contains(res.Content, "do it overnight") {
			t.Fatalf("tool result = %q, want the client's notes carried through", res.Content)
		}
	case <-time.After(defaultWait):
		t.Fatal("timed out waiting for the gate to unblock the turn")
	}

	// Both peers receive the resolution.
	waitForNotificationMethod(t, driver, daemon.MethodGoferDecisionResolved)
	waitForNotificationMethod(t, phone, daemon.MethodGoferDecisionResolved)

	select {
	case resp := <-promptDone:
		if resp.Error != nil {
			t.Fatalf("session/prompt: %v", resp.Error)
		}
	case <-time.After(defaultWait):
		t.Fatal("session/prompt did not return after the decision resolved")
	}

	// Nothing leaks past the resolution: no route, no retained payload, no
	// outstanding ACP request.
	waitOpenDecisions(t, h, 0)
	waitOutstandingDecisionReqs(t, h, 0)
	if got := h.d.RetainedDecisionCount(); got != 0 {
		t.Fatalf("retained decisions after resolution = %d, want 0", got)
	}
}

// TestDecisionAnswerUnknownRequest rejects an answer that names no outstanding
// request — a bad session id, a bad request id, or an id that already resolved —
// with a descriptive error rather than silently dropping it. A silent drop would
// leave a client believing it had unblocked a turn that is still waiting.
func TestDecisionAnswerUnknownRequest(t *testing.T) {
	h := newDecisionHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cwd := t.TempDir()
	c := dial(t, ctx, h.url, nil)
	sid := newACPSession(t, c, cwd)

	promptDone := make(chan rpcFrame, 1)
	go func() {
		promptDone <- c.request("session/prompt", map[string]any{"sessionId": sid, "text": "ask me"})
	}()
	req := awaitDecisionRequest(t, c)

	for _, tc := range []struct {
		name       string
		params     map[string]any
		wantSubstr string
	}{
		{
			name:       "unknown request id",
			params:     selectAnswer(sid, "dec-99", "q1", "q1o1", ""),
			wantSubstr: `no outstanding decision request with id "dec-99"`,
		},
		{
			// The session is what disambiguates a per-session request id: the same
			// "dec-1" under a session that never asked must not resolve this one.
			name:       "right request id, wrong session",
			params:     selectAnswer("sess-does-not-exist", req.ID, "q1", "q1o1", ""),
			wantSubstr: `session "sess-does-not-exist" has no outstanding decision request`,
		},
		{
			name:       "missing session id",
			params:     map[string]any{"id": req.ID, "answers": []any{}},
			wantSubstr: "sessionId is required",
		},
		{
			name:       "missing request id",
			params:     map[string]any{"sessionId": sid, "answers": []any{}},
			wantSubstr: "id is required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Sent as a request (with an id) so the error reply is observable; the
			// production client sends it as a notification.
			resp := c.request(daemon.MethodDecisionAnswer, tc.params)
			if resp.Error == nil {
				t.Fatalf("%s(%s): want error, got success", daemon.MethodDecisionAnswer, tc.name)
			}
			if !strings.Contains(resp.Error.Message, tc.wantSubstr) {
				t.Fatalf("error = %q, want it to name %q", resp.Error.Message, tc.wantSubstr)
			}
		})
	}

	// The real request is untouched by the rejections and still answerable.
	c.notify(daemon.MethodDecisionAnswer, selectAnswer(sid, req.ID, "q1", "q1o1", ""))
	select {
	case <-h.fake(sid).results:
	case <-time.After(defaultWait):
		t.Fatal("the genuine answer did not unblock the turn")
	}
	if resp := <-promptDone; resp.Error != nil {
		t.Fatalf("session/prompt: %v", resp.Error)
	}
}

// TestDecisionAnswerRejectedByGate: an answer the GATE rejects (an option the
// question does not offer) comes back as an error and leaves the request open,
// so the client can correct and retry rather than losing the prompt.
func TestDecisionAnswerRejectedByGate(t *testing.T) {
	h := newDecisionHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cwd := t.TempDir()
	c := dial(t, ctx, h.url, nil)
	sid := newACPSession(t, c, cwd)

	promptDone := make(chan rpcFrame, 1)
	go func() {
		promptDone <- c.request("session/prompt", map[string]any{"sessionId": sid, "text": "ask me"})
	}()
	req := awaitDecisionRequest(t, c)

	resp := c.request(daemon.MethodDecisionAnswer, selectAnswer(sid, req.ID, "q1", "q1o99", ""))
	if resp.Error == nil {
		t.Fatal("answer naming a non-existent option: want error, got success")
	}
	if !strings.Contains(resp.Error.Message, "q1o99") {
		t.Fatalf("error = %q, want it to name the bad option", resp.Error.Message)
	}

	// Still open, still answerable.
	if got := h.d.OpenDecisionCount(); got != 1 {
		t.Fatalf("open decision routes after a rejected answer = %d, want 1 (the request must stay open)", got)
	}
	c.notify(daemon.MethodDecisionAnswer, selectAnswer(sid, req.ID, "q1", "q1o1", ""))
	select {
	case res := <-h.fake(sid).results:
		if !strings.Contains(res.Content, "q1o1") {
			t.Fatalf("tool result = %q, want the corrected answer", res.Content)
		}
	case <-time.After(defaultWait):
		t.Fatal("the corrected answer did not unblock the turn")
	}
	if resp := <-promptDone; resp.Error != nil {
		t.Fatalf("session/prompt: %v", resp.Error)
	}
}

// TestDecisionReplayedOnAttach: a peer that attaches AFTER the question was
// asked receives it on session/load and can answer it. This is the only path by
// which a late client learns a turn is blocked on a decision — a decision is not
// an event, so it is in no replay backlog and no journal — which is why the
// replay is unconditional rather than behind a config flag.
func TestDecisionReplayedOnAttach(t *testing.T) {
	h := newDecisionHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cwd := t.TempDir()
	driver := dial(t, ctx, h.url, nil)
	sid := newACPSession(t, driver, cwd)

	promptDone := make(chan rpcFrame, 1)
	go func() {
		promptDone <- driver.request("session/prompt", map[string]any{"sessionId": sid, "text": "ask me"})
	}()
	driverReq := awaitDecisionRequest(t, driver)

	// A second client attaches only now, with the question already outstanding.
	late := dial(t, ctx, h.url, nil)
	if lr := late.request("session/load", map[string]any{"sessionId": sid, "cwd": cwd}); lr.Error != nil {
		t.Fatalf("session/load: %v", lr.Error)
	}

	lateReq := awaitDecisionRequest(t, late)
	if lateReq.ID != driverReq.ID || lateReq.SessionID != sid {
		t.Fatalf("replayed request = %s/%s, want %s/%s", lateReq.SessionID, lateReq.ID, sid, driverReq.ID)
	}
	if len(lateReq.Questions) != 1 || lateReq.Questions[0].QuestionID != "q1" ||
		len(lateReq.Questions[0].Options) != 2 {
		t.Fatalf("replayed request lost its questions/options: %+v", lateReq.Questions)
	}

	// The late peer answers what it was replayed.
	late.notify(daemon.MethodDecisionAnswer, selectAnswer(sid, lateReq.ID, "q1", "q1o2", ""))
	select {
	case res := <-h.fake(sid).results:
		if !strings.Contains(res.Content, "q1o2") {
			t.Fatalf("tool result = %q, want the late peer's answer", res.Content)
		}
	case <-time.After(defaultWait):
		t.Fatal("the late peer's answer did not unblock the turn")
	}
	if resp := <-promptDone; resp.Error != nil {
		t.Fatalf("session/prompt: %v", resp.Error)
	}

	// A peer attaching AFTER the resolution must not be shown the answered
	// question — the retained payload is dropped on resolve.
	waitOpenDecisions(t, h, 0)
	later := dial(t, ctx, h.url, nil)
	if lr := later.request("session/load", map[string]any{"sessionId": sid, "cwd": cwd}); lr.Error != nil {
		t.Fatalf("session/load: %v", lr.Error)
	}
	select {
	case f := <-later.notifications:
		if f.Method == daemon.MethodGoferDecisionRequested {
			t.Fatal("a resolved decision was replayed to a peer attaching afterwards")
		}
	case <-time.After(200 * time.Millisecond):
	}
}

// TestDecisionSurvivesAnsweringPeerDisconnect: the peer that would have answered
// hangs up while the question is outstanding. The request stays open and another
// peer can still answer it — a client going away must not strand a blocked turn.
func TestDecisionSurvivesAnsweringPeerDisconnect(t *testing.T) {
	h := newDecisionHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cwd := t.TempDir()
	driver := dial(t, ctx, h.url, nil)
	phone := dial(t, ctx, h.url, nil)

	sid := newACPSession(t, driver, cwd)
	if lr := phone.request("session/load", map[string]any{"sessionId": sid, "cwd": cwd}); lr.Error != nil {
		t.Fatalf("session/load: %v", lr.Error)
	}

	promptDone := make(chan rpcFrame, 1)
	go func() {
		promptDone <- driver.request("session/prompt", map[string]any{"sessionId": sid, "text": "ask me"})
	}()

	awaitDecisionRequest(t, driver)
	phoneReq := awaitDecisionRequest(t, phone)

	// The phone disconnects with the question outstanding.
	phone.close()

	// The driver answers instead, and the turn unblocks.
	driver.notify(daemon.MethodDecisionAnswer, selectAnswer(sid, phoneReq.ID, "q1", "q1o1", ""))
	select {
	case res := <-h.fake(sid).results:
		if !strings.Contains(res.Content, "q1o1") {
			t.Fatalf("tool result = %q, want the surviving peer's answer", res.Content)
		}
	case <-time.After(defaultWait):
		t.Fatal("a peer disconnect stranded the decision — the remaining peer could not answer it")
	}
	if resp := <-promptDone; resp.Error != nil {
		t.Fatalf("session/prompt: %v", resp.Error)
	}
	waitOutstandingDecisionReqs(t, h, 0)
}

// TestDecisionZeroPeersStaysOpen pins the ErrNoClient consequence of the
// daemon's standing watcher, because it is a real behavior change and not an
// accident: under a daemon the gate always has a subscriber (the watcher), so a
// question asked with NO peer attached does not fail fast with "no client
// attached" — it stays open until a peer attaches and is then answerable,
// exactly as a permission asked with zero peers already behaves.
func TestDecisionZeroPeersStaysOpen(t *testing.T) {
	h := newDecisionHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cwd := t.TempDir()

	// Create the session and start its turn, then hang up entirely: nothing is
	// attached while the question is asked.
	starter := dial(t, ctx, h.url, nil)
	sid := newACPSession(t, starter, cwd)
	if err := h.sup.Send(ctx, sid, "ask me"); err != nil {
		t.Fatalf("supervisor.Send: %v", err)
	}
	starter.close()

	// The request opens with no peer to receive its broadcast.
	waitOpenDecisions(t, h, 1)
	select {
	case res := <-h.fake(sid).results:
		t.Fatalf("ask_user returned %q with no client attached — want it to stay blocked", res.Content)
	case <-time.After(200 * time.Millisecond):
	}

	// A peer attaches and is shown the open question, which it answers.
	late := dial(t, ctx, h.url, nil)
	if lr := late.request("session/load", map[string]any{"sessionId": sid, "cwd": cwd}); lr.Error != nil {
		t.Fatalf("session/load: %v", lr.Error)
	}
	req := awaitDecisionRequest(t, late)
	late.notify(daemon.MethodDecisionAnswer, selectAnswer(sid, req.ID, "q1", "q1o1", ""))

	select {
	case res := <-h.fake(sid).results:
		if !strings.Contains(res.Content, "q1o1") {
			t.Fatalf("tool result = %q, want the late peer's answer", res.Content)
		}
	case <-time.After(defaultWait):
		t.Fatal("the question asked with zero peers was not answerable once one attached")
	}
}

// TestDecisionReleasedWhenSessionKilled: killing a session with a question
// outstanding closes its gate, which resolves every open request — releasing the
// daemon's route, retained payload, and outstanding ACP requests rather than
// leaking them for the life of the process.
func TestDecisionReleasedWhenSessionKilled(t *testing.T) {
	h := newDecisionHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cwd := t.TempDir()
	c := dial(t, ctx, h.url, nil)
	sid := newACPSession(t, c, cwd)

	promptDone := make(chan rpcFrame, 1)
	go func() {
		promptDone <- c.request("session/prompt", map[string]any{"sessionId": sid, "text": "ask me"})
	}()
	awaitDecisionRequest(t, c)
	waitOpenDecisions(t, h, 1)

	if r := c.request("gofer/kill", map[string]any{"sessionId": sid}); r.Error != nil {
		t.Fatalf("gofer/kill: %v", r.Error)
	}

	waitOpenDecisions(t, h, 0)
	waitOutstandingDecisionReqs(t, h, 0)
	if got := h.d.RetainedDecisionCount(); got != 0 {
		t.Fatalf("retained decisions after kill = %d, want 0", got)
	}
	select {
	case <-promptDone:
	case <-time.After(defaultWait):
		t.Fatal("session/prompt did not return after the session was killed")
	}
}

// TestDecisionsAreSessionScoped: two sessions both blocked on their own "dec-1"
// (the gate mints request ids per session) resolve independently. An answer
// carrying session A's id must not resolve session B's identically-named
// request — the reason the daemon keys its decision registries on the pair.
func TestDecisionsAreSessionScoped(t *testing.T) {
	h := newDecisionHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cwd := t.TempDir()
	c := dial(t, ctx, h.url, nil)

	sidA := newACPSession(t, c, cwd)
	sidB := newACPSession(t, c, cwd)

	doneA := make(chan rpcFrame, 1)
	doneB := make(chan rpcFrame, 1)
	go func() { doneA <- c.request("session/prompt", map[string]any{"sessionId": sidA, "text": "ask me"}) }()
	go func() { doneB <- c.request("session/prompt", map[string]any{"sessionId": sidB, "text": "ask me"}) }()

	// Both sessions ask; both requests are "dec-1".
	seen := map[string]string{}
	for range 2 {
		req := awaitDecisionRequest(t, c)
		if req.ID != "dec-1" {
			t.Fatalf("request id = %q, want the per-session first id dec-1", req.ID)
		}
		seen[req.SessionID] = req.ID
	}
	if len(seen) != 2 {
		t.Fatalf("saw requests for %d sessions, want 2 (%v)", len(seen), seen)
	}
	waitOpenDecisions(t, h, 2)

	// Answer A only.
	c.notify(daemon.MethodDecisionAnswer, selectAnswer(sidA, "dec-1", "q1", "q1o1", ""))
	select {
	case res := <-h.fake(sidA).results:
		if !strings.Contains(res.Content, "q1o1") {
			t.Fatalf("session A tool result = %q, want its own answer", res.Content)
		}
	case <-time.After(defaultWait):
		t.Fatal("session A did not unblock")
	}

	// B is untouched: still open, still blocked.
	waitOpenDecisions(t, h, 1)
	select {
	case res := <-h.fake(sidB).results:
		t.Fatalf("session B resolved on session A's answer (%q) — request ids are session-scoped", res.Content)
	case <-time.After(200 * time.Millisecond):
	}

	c.notify(daemon.MethodDecisionAnswer, selectAnswer(sidB, "dec-1", "q1", "q1o2", ""))
	select {
	case res := <-h.fake(sidB).results:
		if !strings.Contains(res.Content, "q1o2") {
			t.Fatalf("session B tool result = %q, want its own answer", res.Content)
		}
	case <-time.After(defaultWait):
		t.Fatal("session B did not unblock on its own answer")
	}
	if resp := <-doneA; resp.Error != nil {
		t.Fatalf("session A prompt: %v", resp.Error)
	}
	if resp := <-doneB; resp.Error != nil {
		t.Fatalf("session B prompt: %v", resp.Error)
	}
}
