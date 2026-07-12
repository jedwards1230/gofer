package supervisor_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/runner"
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

	mu     sync.Mutex
	calls  []string
	closed bool

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

func (f *fakeSession) ID() string                  { return f.id }
func (f *fakeSession) JournalPath() string         { return f.path }
func (f *fakeSession) Fold() []provider.Message    { return nil }
func (f *fakeSession) Events() *event.Subscription { return f.broker.Subscribe(event.FilterAll, 64) }
func (f *fakeSession) Emit(e event.Event)          { f.broker.Publish(e) }

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
	f.mu.Unlock()

	f.started <- text
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-f.advance:
		return err
	}
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
			return h.register(id, opts.Cwd), nil
		},
		ResumeSession: func(_ context.Context, id string, opts runner.Options) (supervisor.Session, error) {
			h.resN.Add(1)
			return h.register(id, opts.Cwd), nil
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

// waitForState polls Roster until id reaches want or the deadline passes.
// The supervisor's pump goroutine updates state asynchronously to Submit/
// Interrupt/finish, so tests that need to observe the settled state use this
// instead of asserting immediately.
func waitForState(t *testing.T, sup *supervisor.Supervisor, id string, want supervisor.State) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range sup.Roster() {
			if e.ID == id && e.State == want {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waitForState: %s did not reach %s within the deadline", id, want)
}
