package supervisor_test

import (
	"context"
	"encoding/json"
	"fmt"
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

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// fakeSession is a scripted supervisor.Session for tests: its Prompt call
// blocks until the test either releases it via finish or the ctx it was
// given is cancelled, so tests can deterministically observe and control
// pump dispatch, interruption, and completion.
type fakeSession struct {
	id   string
	path string

	broker *event.Broker

	// approver is the per-session gate the supervisor injects via
	// runner.Options.Approver, captured by the harness's New/Resume seam. A
	// permission-driving Prompt (permReq set) blocks on it.
	approver loop.Approver
	// tools is the per-session registry the supervisor injects via
	// runner.Options.Tools, captured the same way. A decision-driving Prompt
	// (askInput set) resolves ask_user out of it.
	tools loop.ToolRegistry

	mu     sync.Mutex
	calls  []string
	closed bool
	fold   []provider.Message
	// permReq, when non-empty, makes Prompt emit a permission.requested with
	// this call id and block on approver.Await instead of the generic advance
	// channel — the seam the approval tests use to exercise the real gate.
	permReq string
	// askInput, when non-empty, makes Prompt resolve the ask_user tool out of
	// the injected registry and run it with this JSON input instead of
	// blocking on advance — the seam the decision tests use to drive a REAL
	// tool call (and therefore the real WrapRegistry + decision.Gate wiring)
	// rather than poking the gate directly.
	askInput string
	// askResult/askErr record what that ask_user call returned, for the test
	// to assert on once the turn has unwound.
	askResult loop.ToolResult
	askErr    error

	// setModelCalls records every model argument SetModel was called with, in
	// order — the seam TestSetModel-family tests use to assert the
	// supervisor actually reaches the SDK setter (and, for the
	// cross-provider case, that it does NOT).
	setModelCalls []string

	// setEffortCalls records every effort argument SetEffort was called with,
	// in order — setModelCalls' effort-axis twin, and the seam the
	// TestSetEffort-family tests use to assert the supervisor reaches (or,
	// for a rejected level, does NOT reach) the SDK setter.
	setEffortCalls []string
	// setEffortErr, when non-nil, is what SetEffort returns — standing in for
	// the SDK runner's own rejections (an unknown level, a non-reasoning
	// model), which the supervisor must surface rather than swallow. The call
	// is still recorded either way.
	setEffortErr error

	// started delivers the prompt text each time Prompt is entered — one
	// receive per dispatched turn. Buffered generously; a test only ever
	// needs to drain it in step with its own submissions.
	started chan string
	// advance releases the currently-blocked Prompt call with the given
	// error. Unbuffered by design: a send only succeeds once Prompt is
	// actually blocked waiting on it, at most one call in flight at a time
	// (the supervisor never runs two turns of one session concurrently).
	advance chan error
}

func newFakeSession(id, path string) *fakeSession {
	return &fakeSession{
		id:      id,
		path:    path,
		broker:  event.NewBroker(event.WithReplay(64)),
		started: make(chan string, 16),
		advance: make(chan error),
	}
}

func (f *fakeSession) ID() string          { return f.id }
func (f *fakeSession) JournalPath() string { return f.path }

// Fold returns the fake's canned fold, set via setFold. Defaults to nil —
// the tests that don't care about folded history never observe a change.
func (f *fakeSession) Fold() []provider.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fold
}

// setFold sets the messages a subsequent Fold call returns — the test seam
// [Supervisor.History] tests use.
func (f *fakeSession) setFold(msgs []provider.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fold = msgs
}

func (f *fakeSession) Events() *event.Subscription { return f.broker.Subscribe(event.FilterAll, 64) }

// EventsLive mirrors [runner.Runner.EventsLive]: f.broker is a real
// [event.Broker] (constructed with [event.WithReplay]), so this calls its
// SubscribeLive to get genuine no-replay semantics consistent with Events'
// real-broker Subscribe above — not a plain-channel stand-in, so the two
// stay behaviorally distinct here exactly as they are for a real Runner.
func (f *fakeSession) EventsLive() *event.Subscription {
	return f.broker.SubscribeLive(event.FilterAll, 64)
}

func (f *fakeSession) Emit(e event.Event) { f.broker.Publish(e) }

// Cost returns a canned tally so SessionInfo.Cost/Usage are populated
// deterministically without a real journal.
func (f *fakeSession) Cost() session.CostReport {
	return session.CostReport{
		Usage: provider.Usage{InputTokens: 10, OutputTokens: 5},
		Cost:  provider.Cost{USD: 0.01},
	}
}

func (f *fakeSession) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	f.broker.Close()
	return nil
}

func (f *fakeSession) Prompt(ctx context.Context, text string) error {
	f.mu.Lock()
	f.calls = append(f.calls, text)
	permReq := f.permReq
	askInput := f.askInput
	f.mu.Unlock()

	f.started <- text

	if askInput != "" {
		// Run the real ask_user tool out of the registry the supervisor built,
		// exactly as the SDK loop would on a model tool call. A cancelled turn
		// surfaces here as the tool's own ctx error.
		tl, ok := f.tools.Get("ask_user")
		if !ok {
			return fmt.Errorf("ask_user not registered")
		}
		res, err := tl.Run(ctx, json.RawMessage(askInput))
		f.mu.Lock()
		f.askResult, f.askErr = res, err
		f.mu.Unlock()
		return err
	}

	if permReq != "" {
		// Emit a real permission.requested onto the broker (so watchPermissions
		// counts it), then block on the injected approver exactly as the SDK
		// loop's guard would. On reply, emit the matching permission.resolved.
		f.broker.Publish(event.NewPermissionRequested(f.id, permReq, "bash", map[string]any{"command": text}, []string{"rule: ask"}))
		reply, err := f.approver.Await(ctx, permReq)
		if err != nil {
			f.broker.Publish(event.NewPermissionResolved(f.id, permReq, event.VerdictDeny, "cancelled"))
			return err
		}
		f.broker.Publish(event.NewPermissionResolved(f.id, permReq, reply.Verdict, "human"))
		return nil
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-f.advance:
		return err
	}
}

// setPermReq arms this fake to drive a permission request with call id on its
// next Prompt (see Prompt).
func (f *fakeSession) setPermReq(id string) {
	f.mu.Lock()
	f.permReq = id
	f.mu.Unlock()
}

// setAskInput arms this fake to run the ask_user tool with input on its next
// Prompt (see Prompt).
func (f *fakeSession) setAskInput(input string) {
	f.mu.Lock()
	f.askInput = input
	f.mu.Unlock()
}

// askOutcome returns what the armed ask_user call returned. It is only
// meaningful once the turn has unwound (waitForStatus back to needs-input).
func (f *fakeSession) askOutcome() (loop.ToolResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.askResult, f.askErr
}

// finish releases the currently-blocked Prompt call, letting it return err.
func (f *fakeSession) finish(t *testing.T, err error) {
	t.Helper()
	select {
	case f.advance <- err:
	case <-time.After(2 * time.Second):
		t.Fatal("finish: timed out sending on advance (Prompt not blocked?)")
	}
}

// waitStarted blocks until the pump has entered a Prompt call, returning its
// text.
func (f *fakeSession) waitStarted(t *testing.T) string {
	t.Helper()
	select {
	case text := <-f.started:
		return text
	case <-time.After(2 * time.Second):
		t.Fatal("waitStarted: timed out waiting for Prompt to start")
		return ""
	}
}

func (f *fakeSession) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// SetModel records model onto setModelCalls and always succeeds — this fake
// never second-guesses a model swap itself; [supervisor.Supervisor.SetModel]'s
// own cross-provider pre-check is what tests exercise, and this records
// whether the call reached the SDK seam at all.
func (f *fakeSession) SetModel(model string) error {
	f.mu.Lock()
	f.setModelCalls = append(f.setModelCalls, model)
	f.mu.Unlock()
	return nil
}

// setModelCallCount returns how many times SetModel was called.
func (f *fakeSession) setModelCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.setModelCalls)
}

// lastSetModel returns the most recent SetModel argument, or "" if it was
// never called.
func (f *fakeSession) lastSetModel() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.setModelCalls) == 0 {
		return ""
	}
	return f.setModelCalls[len(f.setModelCalls)-1]
}

// SetEffort records effort onto setEffortCalls and returns setEffortErr —
// [SetModel]'s twin. The fake never validates a level itself: what the tests
// exercise is [supervisor.Supervisor.SetEffort]'s own ValidEffort pre-check
// (which must reject BEFORE reaching this seam) and its propagation of an SDK
// rejection (which setEffortErr stands in for).
func (f *fakeSession) SetEffort(effort string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setEffortCalls = append(f.setEffortCalls, effort)
	return f.setEffortErr
}

// setEffortCallCount returns how many times SetEffort was called.
func (f *fakeSession) setEffortCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.setEffortCalls)
}

// lastSetEffort returns the most recent SetEffort argument, or "" if it was
// never called. Note the ambiguity that forces every caller to pair it with
// setEffortCallCount: "" is also a legitimate ARGUMENT (clear the level).
func (f *fakeSession) lastSetEffort() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.setEffortCalls) == 0 {
		return ""
	}
	return f.setEffortCalls[len(f.setEffortCalls)-1]
}

// failEffort makes the fake's SetEffort return err, standing in for an SDK
// runner rejection.
func (f *fakeSession) failEffort(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setEffortErr = err
}

// harness wires a *supervisor.Supervisor to fakeSession construction, so
// tests get a handle on the exact fake backing each roster entry and can
// assert how many times the New/Resume seams were invoked.
type harness struct {
	t    *testing.T
	sup  *supervisor.Supervisor
	root string

	nextID int64
	newN   atomic.Int64
	resN   atomic.Int64

	mu       sync.Mutex
	sessions map[string]*fakeSession
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, root: t.TempDir(), sessions: make(map[string]*fakeSession)}

	var clockN int64
	cfg := supervisor.Config{
		Root: h.root,
		Clock: func() time.Time {
			n := atomic.AddInt64(&clockN, 1)
			return time.Unix(n, 0)
		},
		NewSession: func(_ context.Context, opts runner.Options) (supervisor.Session, error) {
			h.newN.Add(1)
			id := fmt.Sprintf("sess-%d", atomic.AddInt64(&h.nextID, 1))
			fs := h.register(id, opts.Cwd)
			fs.approver = opts.Approver
			fs.tools = opts.Tools
			return fs, nil
		},
		ResumeSession: func(_ context.Context, id string, opts runner.Options) (supervisor.Session, error) {
			h.resN.Add(1)
			fs := h.register(id, opts.Cwd)
			fs.approver = opts.Approver
			fs.tools = opts.Tools
			return fs, nil
		},
	}

	sup, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	h.sup = sup
	t.Cleanup(func() { _ = sup.Close() })
	return h
}

// register builds and records a fakeSession at the on-disk path a real
// FileStore would use for id under cwd's project slug.
func (h *harness) register(id, cwd string) *fakeSession {
	path := filepath.Join(h.root, "sessions", session.Slugify(cwd), id+".jsonl")
	fs := newFakeSession(id, path)
	h.mu.Lock()
	h.sessions[id] = fs
	h.mu.Unlock()
	return fs
}

func (h *harness) session(id string) *fakeSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[id]
}

// waitForStatus polls Roster until id reaches want or the deadline passes.
// The supervisor's pump goroutine updates state asynchronously to Send/
// Interrupt/finish, so tests that need to observe the settled status use this
// instead of asserting immediately.
func waitForStatus(t *testing.T, sup *supervisor.Supervisor, id string, want supervisor.SessionStatus) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		roster, err := sup.Roster(context.Background())
		if err != nil {
			t.Fatalf("Roster: %v", err)
		}
		for _, e := range roster {
			if e.ID == id && e.Status == want {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waitForStatus: %s did not reach %s within the deadline", id, want)
}
