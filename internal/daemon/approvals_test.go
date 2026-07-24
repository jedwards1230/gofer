package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
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
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// approvalSession is a supervisor.Session whose Prompt emits a real
// permission.requested onto its broker and blocks on the injected approver
// (the supervisor's per-session gate), resuming per the client's reply — the
// seam the daemon approval round-trip exercises. On reply it emits the matching
// permission.resolved and a terminal turn.finished so the driving session/prompt
// returns. The verdict it acted on is published to verdicts for assertions.
type approvalSession struct {
	id       string
	path     string
	broker   *event.Broker
	approver loop.Approver
	callID   string

	verdicts chan event.Verdict
	replies  chan loop.Reply
}

func newApprovalSession(id, path, callID string) *approvalSession {
	return &approvalSession{
		id:       id,
		path:     path,
		broker:   event.NewBroker(event.WithReplay(64)),
		callID:   callID,
		verdicts: make(chan event.Verdict, 1),
		replies:  make(chan loop.Reply, 1),
	}
}

func (f *approvalSession) ID() string               { return f.id }
func (f *approvalSession) JournalPath() string      { return f.path }
func (f *approvalSession) Fold() []provider.Message { return nil }
func (f *approvalSession) Events() *event.Subscription {
	return f.broker.Subscribe(event.FilterAll, 64)
}
func (f *approvalSession) EventsLive() *event.Subscription {
	return f.broker.SubscribeLive(event.FilterAll, 64)
}
func (f *approvalSession) Emit(e event.Event)       { f.broker.Publish(e) }
func (f *approvalSession) Cost() session.CostReport { return session.CostReport{} }

// SetModel is a no-op: this fake's Prompt scripts a fixed permission
// round-trip and never reads a model, so nothing observes the change.
func (f *approvalSession) SetModel(string) error  { return nil }
func (f *approvalSession) SetEffort(string) error { return nil }

func (f *approvalSession) Close() error {
	f.broker.Close()
	return nil
}

func (f *approvalSession) Prompt(ctx context.Context, text string) error {
	f.broker.Publish(event.NewPermissionRequested(f.id, f.callID, "bash", map[string]any{"command": text}, []string{"rule: ask"}))
	reply, err := f.approver.Await(ctx, f.callID)
	if err != nil {
		f.broker.Publish(event.NewPermissionResolved(f.id, f.callID, event.VerdictDeny, "cancelled"))
		// A terminal cancelled turn.finished so the driving session/prompt
		// returns instead of hanging (mirrors a real session on interrupt).
		f.broker.Publish(event.NewTurnFinished(f.id, "cancelled", provider.Usage{}))
		return err
	}
	f.broker.Publish(event.NewPermissionResolved(f.id, f.callID, reply.Verdict, "human"))
	f.verdicts <- reply.Verdict
	f.replies <- reply
	// Terminal turn.finished so the driving session/prompt handler returns.
	f.broker.Publish(event.NewTurnFinished(f.id, "end_turn", provider.Usage{}))
	return nil
}

// approvalHarness builds a Supervisor whose sessions are approvalSessions and
// wires it behind an in-process daemon, exposing the fakes so a test can read
// the verdict a turn's gate delivered.
type approvalHarness struct {
	sup *supervisor.Supervisor
	d   *daemon.Daemon
	url string

	mu     sync.Mutex
	fakes  map[string]*approvalSession
	nextID int64
}

func newApprovalHarness(t *testing.T) *approvalHarness {
	t.Helper()
	root := t.TempDir()
	h := &approvalHarness{fakes: make(map[string]*approvalSession)}

	build := func(id, cwd string, approver loop.Approver) supervisor.Session {
		path := filepath.Join(root, "sessions", session.Slugify(cwd), id+".jsonl")
		fs := newApprovalSession(id, path, "call-1")
		fs.approver = approver
		h.mu.Lock()
		h.fakes[id] = fs
		h.mu.Unlock()
		return fs
	}

	cfg := supervisor.Config{
		Root: root,
		NewSession: func(_ context.Context, opts runner.Options) (supervisor.Session, error) {
			id := fmt.Sprintf("sess-%d", atomic.AddInt64(&h.nextID, 1))
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
	h.sup = sup

	d := daemon.New(sup, daemon.Config{DefaultModel: "faux"})
	h.d = d
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	h.url = "ws" + srv.URL[len("http"):]
	return h
}

func (h *approvalHarness) fake(id string) *approvalSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fakes[id]
}

// waitForNotificationMethod drains a client's notification stream until it sees
// method, returning that frame. Other notifications (a resolved arriving after
// the requested, etc.) are skipped.
func waitForNotificationMethod(t *testing.T, c *wsClient, method string) rpcFrame {
	t.Helper()
	deadline := time.After(defaultWait)
	for {
		select {
		case f, ok := <-c.notifications:
			if !ok {
				t.Fatalf("connection closed waiting for %s", method)
			}
			if f.Method == method {
				return f
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s", method)
		}
	}
}

// TestApprovalRoundTripCrossPeer is the acceptance test: two peers attach to a
// session, one drives a turn that asks, BOTH peers receive the
// permission.requested notification (fan-out), and the NON-originating peer
// (the "phone") answers — the gate unblocks and the session proceeds. A deny
// reply routes a deny verdict the same way.
func TestApprovalRoundTripCrossPeer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		verdict string
		want    event.Verdict
	}{
		{"allow proceeds", "allow", event.VerdictAllow},
		{"deny blocks", "deny", event.VerdictDeny},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newApprovalHarness(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			cwd := t.TempDir()
			driver := dial(t, ctx, h.url, nil) // the "laptop" that drives the turn
			phone := dial(t, ctx, h.url, nil)  // the non-originating "phone" that approves

			// Create a session and attach the phone to it via session/load, so
			// the phone is in the fan-out set for a turn the driver drives.
			newResp := driver.request("session/new", map[string]any{"cwd": cwd})
			if newResp.Error != nil {
				t.Fatalf("session/new: %v", newResp.Error)
			}
			var created struct {
				SessionID string `json:"sessionId"`
			}
			if err := json.Unmarshal(newResp.Result, &created); err != nil {
				t.Fatalf("decode session/new: %v", err)
			}
			sid := created.SessionID

			if lr := phone.request("session/load", map[string]any{"sessionId": sid, "cwd": cwd}); lr.Error != nil {
				t.Fatalf("session/load: %v", lr.Error)
			}

			// Drive the turn from the driver in a goroutine — session/prompt
			// blocks until the gate is answered.
			promptDone := make(chan rpcFrame, 1)
			go func() {
				promptDone <- driver.request("session/prompt", map[string]any{"sessionId": sid, "text": "rm -rf /"})
			}()

			// Fan-out: BOTH peers must receive the permission.requested.
			driverReq := waitForNotificationMethod(t, driver, "gofer/permission_requested")
			phoneReq := waitForNotificationMethod(t, phone, "gofer/permission_requested")

			var pr struct {
				SessionID string `json:"sessionId"`
				ID        string `json:"id"`
				Tool      string `json:"tool"`
			}
			if err := json.Unmarshal(phoneReq.Params, &pr); err != nil {
				t.Fatalf("decode permission_requested: %v", err)
			}
			if pr.SessionID != sid || pr.ID == "" || pr.Tool != "bash" {
				t.Fatalf("permission_requested params = %+v, want session %s, non-empty id, tool bash", pr, sid)
			}
			// Both peers saw the same request id.
			var dpr struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(driverReq.Params, &dpr)
			if dpr.ID != pr.ID {
				t.Fatalf("driver saw request id %q, phone saw %q — fan-out must deliver the same request", dpr.ID, pr.ID)
			}

			// The NON-originating peer answers.
			phone.notify("permission.reply", map[string]any{"id": pr.ID, "verdict": tc.verdict})

			// The gate delivered the verdict the phone chose.
			select {
			case got := <-h.fake(sid).verdicts:
				if got != tc.want {
					t.Fatalf("gate delivered verdict %q, want %q", got, tc.want)
				}
			case <-time.After(defaultWait):
				t.Fatal("timed out waiting for the gate to unblock the turn")
			}

			// Both peers receive the resolution too.
			waitForNotificationMethod(t, driver, "gofer/permission_resolved")
			waitForNotificationMethod(t, phone, "gofer/permission_resolved")

			// The driving session/prompt returns now that the turn finished.
			select {
			case resp := <-promptDone:
				if resp.Error != nil {
					t.Fatalf("session/prompt: %v", resp.Error)
				}
			case <-time.After(defaultWait):
				t.Fatal("session/prompt did not return after the turn resolved")
			}
		})
	}
}

// TestPermissionReplyUnknownID rejects a reply whose call id matches no
// outstanding request rather than silently dropping it.
func TestPermissionReplyUnknownID(t *testing.T) {
	h := newApprovalHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := dial(t, ctx, h.url, nil)

	// Sent as a request (with an id) so we can observe the error reply; the
	// production client sends it as a notification.
	resp := c.request("permission.reply", map[string]any{"id": "nope", "verdict": "allow"})
	if resp.Error == nil {
		t.Fatal("permission.reply(unknown id): want error, got success")
	}
}

// TestPendingCountInRoster verifies the roster DTO carries the live pending
// approval count: 1 while a turn awaits a decision, back to 0 after it resolves.
func TestPendingCountInRoster(t *testing.T) {
	h := newApprovalHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cwd := t.TempDir()
	c := dial(t, ctx, h.url, nil)

	newResp := c.request("session/new", map[string]any{"cwd": cwd})
	if newResp.Error != nil {
		t.Fatalf("session/new: %v", newResp.Error)
	}
	var created struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(newResp.Result, &created); err != nil {
		t.Fatalf("decode session/new: %v", err)
	}
	sid := created.SessionID

	promptDone := make(chan rpcFrame, 1)
	go func() {
		promptDone <- c.request("session/prompt", map[string]any{"sessionId": sid, "text": "ls"})
	}()

	req := waitForNotificationMethod(t, c, "gofer/permission_requested")
	var pr struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(req.Params, &pr); err != nil {
		t.Fatalf("decode permission_requested: %v", err)
	}

	waitForRosterPending(t, c, sid, 1)

	c.notify("permission.reply", map[string]any{"id": pr.ID, "verdict": "allow"})

	select {
	case resp := <-promptDone:
		if resp.Error != nil {
			t.Fatalf("session/prompt: %v", resp.Error)
		}
	case <-time.After(defaultWait):
		t.Fatal("session/prompt did not return")
	}

	waitForRosterPending(t, c, sid, 0)
}

// waitForRosterPending polls gofer/roster until sid's pending count reaches
// want.
func waitForRosterPending(t *testing.T, c *wsClient, sid string, want int) {
	t.Helper()
	deadline := time.Now().Add(defaultWait)
	var last int
	for time.Now().Before(deadline) {
		resp := c.request("gofer/roster", nil)
		if resp.Error != nil {
			t.Fatalf("gofer/roster: %v", resp.Error)
		}
		var rows []struct {
			ID      string `json:"id"`
			Pending int    `json:"pending"`
		}
		if err := json.Unmarshal(resp.Result, &rows); err != nil {
			t.Fatalf("decode roster: %v", err)
		}
		for _, r := range rows {
			if r.ID == sid {
				last = r.Pending
				if r.Pending == want {
					return
				}
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("roster pending for %s = %d, want %d", sid, last, want)
}
