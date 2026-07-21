package supervisor_test

// decision_relay_test.go covers the standing per-session decision watcher: what
// a host installing a [supervisor.DecisionRelay] gets (every open/resolve
// relayed, for sessions created before AND after the install), what a host that
// installs none keeps (the ErrNoClient fast path an unattached daemonless
// session relies on), and that the watcher never outlives its session.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/acp"

	"github.com/jedwards1230/gofer/internal/decision"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// recordingRelay is a [supervisor.DecisionRelay] that records every call, so a
// test can assert what the standing watcher relayed and in what order.
type recordingRelay struct {
	mu        sync.Mutex
	requested []relayedRequest
	resolved  []relayedRequest
	// gotOne fires on the first RequestDecision, so a test can wait for the
	// relay rather than poll for it.
	gotOne chan struct{}
	once   sync.Once
}

type relayedRequest struct {
	session   string
	request   string
	questions []acp.DecisionQuestion
}

func newRecordingRelay() *recordingRelay {
	return &recordingRelay{gotOne: make(chan struct{})}
}

func (r *recordingRelay) RequestDecision(sessionID, requestID string, questions []acp.DecisionQuestion) {
	r.mu.Lock()
	r.requested = append(r.requested, relayedRequest{sessionID, requestID, questions})
	r.mu.Unlock()
	r.once.Do(func() { close(r.gotOne) })
}

func (r *recordingRelay) ResolveDecision(sessionID, requestID string) {
	r.mu.Lock()
	r.resolved = append(r.resolved, relayedRequest{session: sessionID, request: requestID})
	r.mu.Unlock()
}

func (r *recordingRelay) snapshot() (requested, resolved []relayedRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]relayedRequest(nil), r.requested...), append([]relayedRequest(nil), r.resolved...)
}

// awaitRelayedRequest blocks until the relay has seen its first request.
func (r *recordingRelay) awaitRelayedRequest(t *testing.T) relayedRequest {
	t.Helper()
	select {
	case <-r.gotOne:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the watcher to relay a decision request")
	}
	requested, _ := r.snapshot()
	return requested[0]
}

// awaitRelayedResolve polls until the relay has seen at least one resolution.
// Resolution is delivered on the watcher goroutine, so it trails the call that
// caused it.
func (r *recordingRelay) awaitRelayedResolve(t *testing.T) relayedRequest {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, resolved := r.snapshot(); len(resolved) > 0 {
			return resolved[0]
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the watcher to relay a decision resolution")
	return relayedRequest{}
}

// TestDecisionRelayRelaysOpenAndResolve: with a relay installed, a session's
// ask_user reaches the host on open and again on resolve, carrying the
// gate-assigned ids and the full question set.
func TestDecisionRelayRelaysOpenAndResolve(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	relay := newRecordingRelay()
	h.sup.SetDecisionRelay(relay)

	info, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: h.root, Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(info.ID)
	fs.setAskInput(askTwoOptions)
	if err := h.sup.Send(ctx, info.ID, "migrate the table"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fs.waitStarted(t)

	got := relay.awaitRelayedRequest(t)
	if got.session != info.ID || got.request != "dec-1" {
		t.Fatalf("relayed %s/%s, want %s/dec-1", got.session, got.request, info.ID)
	}
	if len(got.questions) != 1 || got.questions[0].QuestionID != "q1" || len(got.questions[0].Options) != 2 {
		t.Fatalf("relayed questions = %+v, want one stamped question with two options", got.questions)
	}

	if err := h.sup.AnswerDecision(info.ID, "dec-1", []acp.DecisionAnswer{
		{QuestionID: "q1", Outcome: acp.DecisionOutcomeSelected{OptionID: "q1o1"}},
	}); err != nil {
		t.Fatalf("AnswerDecision: %v", err)
	}

	res := relay.awaitRelayedResolve(t)
	if res.session != info.ID || res.request != "dec-1" {
		t.Fatalf("relayed resolve %s/%s, want %s/dec-1", res.session, res.request, info.ID)
	}
}

// TestDecisionRelayInstalledAfterSessionExists: a relay installed while a
// session is already live still gets that session's decisions. The retro-start
// exists so the contract does not silently depend on the host installing before
// its first session.
func TestDecisionRelayInstalledAfterSessionExists(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	info, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: h.root, Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	relay := newRecordingRelay()
	h.sup.SetDecisionRelay(relay)

	fs := h.session(info.ID)
	fs.setAskInput(askTwoOptions)
	if err := h.sup.Send(ctx, info.ID, "migrate the table"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fs.waitStarted(t)

	if got := relay.awaitRelayedRequest(t); got.session != info.ID {
		t.Fatalf("relayed session %q, want %q", got.session, info.ID)
	}
}

// TestDecisionRelayStartsOneWatcherPerSession: register and SetDecisionRelay can
// both reach a session, and a second install must not stack a second watcher —
// two watchers on one gate would relay (and so broadcast) every request twice.
func TestDecisionRelayStartsOneWatcherPerSession(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	relay := newRecordingRelay()
	h.sup.SetDecisionRelay(relay)

	info, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: h.root, Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// A second install over the same live session.
	h.sup.SetDecisionRelay(relay)

	fs := h.session(info.ID)
	fs.setAskInput(askTwoOptions)
	if err := h.sup.Send(ctx, info.ID, "migrate the table"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fs.waitStarted(t)
	relay.awaitRelayedRequest(t)

	// Give a hypothetical second watcher time to relay the same request.
	time.Sleep(50 * time.Millisecond)
	requested, _ := relay.snapshot()
	if len(requested) != 1 {
		t.Fatalf("relay saw %d requests for one ask_user, want exactly 1 (a duplicated watcher double-broadcasts)", len(requested))
	}
}

// TestNoDecisionRelayKeepsErrNoClient pins the daemonless behavior the relay
// deliberately changes: with NO relay installed, nothing subscribes to a
// session's gate on its behalf, so an ask_user on a session no client is
// watching fails fast with ErrNoClient and the tool tells the model to continue
// in prose — rather than blocking a turn on a question nobody can see.
func TestNoDecisionRelayKeepsErrNoClient(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	info, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: h.root, Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(info.ID)
	fs.setAskInput(askTwoOptions)
	if err := h.sup.Send(ctx, info.ID, "migrate the table"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fs.waitStarted(t)
	waitForStatus(t, h.sup, info.ID, supervisor.StatusNeedsInput)

	res, runErr := fs.askOutcome()
	if runErr != nil {
		t.Fatalf("ask_user err = %v, want the no-client tool result", runErr)
	}
	if !res.IsError {
		t.Fatalf("ask_user result = %+v, want the no-client error result", res)
	}
}

// TestDecisionRelaySeesSessionKillResolutions: killing a session with a request
// outstanding relays the resolution before the watcher unwinds, so the host
// releases its per-request state instead of leaking it — and Kill itself does
// not hang joining the watcher.
func TestDecisionRelaySeesSessionKillResolutions(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	relay := newRecordingRelay()
	h.sup.SetDecisionRelay(relay)

	info, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: h.root, Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(info.ID)
	fs.setAskInput(askTwoOptions)
	if err := h.sup.Send(ctx, info.ID, "migrate the table"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	fs.waitStarted(t)
	relay.awaitRelayedRequest(t)

	killed := make(chan error, 1)
	go func() { killed <- h.sup.Kill(ctx, info.ID) }()
	select {
	case err := <-killed:
		if err != nil {
			t.Fatalf("Kill: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Kill hung — the decision watcher was joined before its gate was closed")
	}

	res := relay.awaitRelayedResolve(t)
	if res.request != "dec-1" {
		t.Fatalf("relayed resolve %q, want dec-1", res.request)
	}
}

// TestSetDecisionRelayIgnoresNil: a nil relay must be a no-op rather than
// starting a watcher that would panic on its first update — "no relay" is a
// configuration, not a mistake.
func TestSetDecisionRelayIgnoresNil(t *testing.T) {
	h := newHarness(t)
	h.sup.SetDecisionRelay(nil)

	info, err := h.sup.Create(context.Background(), "", supervisor.CreateOptions{Cwd: h.root, Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// With no watcher, the gate still reports no client to a would-be requester.
	sub, err := h.sup.SubscribeDecisions(info.ID, 1)
	if err != nil {
		t.Fatalf("SubscribeDecisions: %v", err)
	}
	sub.Close()
	if err := h.sup.AnswerDecision(info.ID, "dec-1", nil); !errors.Is(err, decision.ErrUnknownRequest) {
		t.Fatalf("AnswerDecision on a session with no open request = %v, want ErrUnknownRequest", err)
	}
}
