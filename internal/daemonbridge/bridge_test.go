package daemonbridge_test

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/provider/faux"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/daemonbridge"
	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/tui"
)

// defaultWait bounds every blocking wait in this package's tests, mirroring
// internal/daemon's test harness: generous enough for CI, short enough that a
// real regression fails fast.
const defaultWait = 5 * time.Second

// newTestSupervisor builds a Supervisor whose sessions are real
// [runner.Runner]s over a test-scripted [provider.Provider] — no network,
// fully deterministic. It mirrors internal/daemon's own harness_test.go,
// rebuilt here against exported API only (that harness's package is
// daemon_test, unexported to this package).
func newTestSupervisor(t *testing.T, newProvider func() provider.Provider) *supervisor.Supervisor {
	t.Helper()
	root := t.TempDir()
	store, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		t.Fatalf("session.NewFileStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := supervisor.Config{
		Root:  root,
		Store: store,
		NewSession: func(ctx context.Context, opts runner.Options) (supervisor.Session, error) {
			opts.Store = store
			opts.Model = "faux"
			opts.Provider = newProvider()
			return runner.New(ctx, opts)
		},
		ResumeSession: func(ctx context.Context, id string, opts runner.Options) (supervisor.Session, error) {
			opts.Store = store
			opts.Model = "faux"
			opts.Provider = newProvider()
			return runner.Resume(ctx, id, opts)
		},
	}
	sup, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })
	return sup
}

// newTestDaemon wires sup behind an in-process httptest.Server, returning its
// ws:// base URL. The server is closed on test cleanup.
func newTestDaemon(t *testing.T, sup *supervisor.Supervisor) string {
	t.Helper()
	d := daemon.New(sup, daemon.Config{DefaultModel: "faux"})
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	return "ws" + srv.URL[len("http"):]
}

// newBridge dials url and wraps the connection in a [daemonbridge.Supervisor],
// registering cleanup to Close it.
func newBridge(t *testing.T, url string) *daemonbridge.Supervisor {
	t.Helper()
	c, err := daemon.Dial(context.Background(), url, "")
	if err != nil {
		t.Fatalf("daemon.Dial: %v", err)
	}
	b := daemonbridge.New(c)
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// fauxProvider returns a provider.Provider constructor replaying
// faux.Default(): one turn, 2 reasoning deltas + 3 text deltas, end_turn.
func fauxProvider() provider.Provider { return faux.New(faux.Default()) }

// drainEvents reads exactly n events from sub, failing the test if it times
// out or the subscription closes early.
func drainEvents(t *testing.T, sub *event.Subscription, n int) []event.Event {
	t.Helper()
	out := make([]event.Event, 0, n)
	for i := 0; i < n; i++ {
		select {
		case e, ok := <-sub.C:
			if !ok {
				t.Fatalf("subscription closed after %d/%d events", i, n)
			}
			out = append(out, e)
		case <-time.After(defaultWait):
			t.Fatalf("timed out waiting for event %d/%d", i, n)
		}
	}
	return out
}

// TestRosterReflectsCreatedSession asserts a session created through the
// bridge shows up in a subsequent Roster call, idle (StatusNeedsInput) since
// no prompt was given.
func TestRosterReflectsCreatedSession(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	url := newTestDaemon(t, sup)
	b := newBridge(t, url)

	info, err := b.Create(context.Background(), "", tui.CreateOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if info.ID == "" {
		t.Fatal("Create: empty session id")
	}
	if info.Status != tui.StatusNeedsInput {
		t.Errorf("Create (no prompt): Status = %v, want StatusNeedsInput", info.Status)
	}

	roster, err := b.Roster(context.Background())
	if err != nil {
		t.Fatalf("Roster: %v", err)
	}
	if len(roster) != 1 || roster[0].ID != info.ID {
		t.Fatalf("Roster = %+v, want one entry for %s", roster, info.ID)
	}
	if roster[0].Status != tui.StatusNeedsInput {
		t.Errorf("Roster row Status = %v, want StatusNeedsInput", roster[0].Status)
	}
}

// TestSendReconstructsTranscript drives Create→Send against the faux
// provider's default script and asserts the exact reconstructed event
// sequence: TurnStarted, the reasoning message (started/2 deltas/finished),
// the text message (started/3 deltas/finished), then TurnFinished with the
// scripted stop reason — TurnFinished strictly after every delta.
func TestSendReconstructsTranscript(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	url := newTestDaemon(t, sup)
	b := newBridge(t, url)

	info, err := b.Create(context.Background(), "", tui.CreateOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	sub, err := b.Subscribe(context.Background(), info.ID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	if err := b.Send(context.Background(), info.ID, "hi"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// turn.started, message.started(reasoning), 2x message.delta,
	// message.finished(reasoning), message.started(text), 3x message.delta,
	// message.finished(text), turn.finished = 11 events.
	events := drainEvents(t, sub, 11)

	wantKinds := []string{
		"turn.started",
		"message.started", "message.delta", "message.delta", "message.finished",
		"message.started", "message.delta", "message.delta", "message.delta", "message.finished",
		"turn.finished",
	}
	for i, want := range wantKinds {
		if got := events[i].Kind(); got != want {
			t.Errorf("event %d: Kind() = %q, want %q", i, got, want)
		}
	}

	reasoning, ok := events[1].(event.MessageStarted)
	if !ok || reasoning.MessageKind != event.MessageReasoning {
		t.Errorf("event 1 = %+v, want MessageStarted(reasoning)", events[1])
	}
	if fin, ok := events[4].(event.MessageFinished); !ok || fin.Content != "The user said hello. I'll greet them back." {
		t.Errorf("event 4 (reasoning finished) = %+v, want the joined reasoning chunks", events[4])
	}
	textStart, ok := events[5].(event.MessageStarted)
	if !ok || textStart.MessageKind != event.MessageText {
		t.Errorf("event 5 = %+v, want MessageStarted(text)", events[5])
	}
	if fin, ok := events[9].(event.MessageFinished); !ok || fin.Content != "Hello! How can I help you today?" {
		t.Errorf("event 9 (text finished) = %+v, want the joined text chunks", events[9])
	}

	tf, ok := events[10].(event.TurnFinished)
	if !ok {
		t.Fatalf("event 10 = %+v, want TurnFinished", events[10])
	}
	if tf.StopReason != "end_turn" {
		t.Errorf("TurnFinished.StopReason = %q, want %q", tf.StopReason, "end_turn")
	}

	// No stray event arrives after TurnFinished for this one-turn script.
	select {
	case e, ok := <-sub.C:
		if ok {
			t.Errorf("unexpected extra event after TurnFinished: %+v", e)
		}
	case <-time.After(50 * time.Millisecond):
	}
}

// toolTurnProvider is a hand-scripted [provider.Provider] whose first turn
// requests an (unregistered — this test harness configures no tool
// registry) tool call and whose second turn finishes with plain text. The
// loop's "unknown/no registry" path (internal/loop) still emits a
// well-formed ToolCallFinished(IsError=true), which is enough to exercise
// the bridge's tool_call/tool_call_update reconstruction end to end.
type toolTurnProvider struct{ turn int }

func (p *toolTurnProvider) Info() provider.ModelInfo { return provider.ModelInfo{ID: "tool-test"} }

func (p *toolTurnProvider) Stream(context.Context, provider.Request) (provider.StreamHandle, error) {
	turn := p.turn
	p.turn++
	switch turn {
	case 0:
		return &toolTurnStream{events: []provider.StreamEvent{
			{Type: provider.StreamToolCallStart, Tool: &provider.ToolCall{ID: "tc-1", Name: "read_file"}},
			{Type: provider.StreamToolCallEnd, Tool: &provider.ToolCall{ID: "tc-1", Name: "read_file", Input: []byte(`{"path":"a.txt"}`)}},
			{Type: provider.StreamFinished, StopReason: provider.StopToolUse},
		}}, nil
	case 1:
		return &toolTurnStream{events: []provider.StreamEvent{
			{Type: provider.StreamTextDelta, Text: "done"},
			{Type: provider.StreamFinished, StopReason: provider.StopEndTurn},
		}}, nil
	default:
		return nil, errors.New("toolTurnProvider: script exhausted")
	}
}

type toolTurnStream struct {
	events []provider.StreamEvent
	i      int
}

func (s *toolTurnStream) Next() (provider.StreamEvent, error) {
	if s.i >= len(s.events) {
		return provider.StreamEvent{}, io.EOF
	}
	e := s.events[s.i]
	s.i++
	return e, nil
}

func (s *toolTurnStream) Close() error { return nil }

// TestToolCallReconstruction asserts a tool_call/tool_call_update pair
// reconstructs to ToolCallStarted/ToolCallFinished, sandwiched between the
// turn's TurnStarted and its terminal TurnFinished — two model calls in one
// Send, since the first turn's stop reason is tool_use.
func TestToolCallReconstruction(t *testing.T) {
	sup := newTestSupervisor(t, func() provider.Provider { return &toolTurnProvider{} })
	url := newTestDaemon(t, sup)
	b := newBridge(t, url)

	info, err := b.Create(context.Background(), "", tui.CreateOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sub, err := b.Subscribe(context.Background(), info.ID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	if err := b.Send(context.Background(), info.ID, "read a.txt"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// turn.started, tool.call.started, tool.call.finished,
	// message.started(text), message.delta, message.finished(text),
	// turn.finished = 7 events.
	events := drainEvents(t, sub, 7)

	if _, ok := events[0].(event.TurnStarted); !ok {
		t.Errorf("event 0 = %+v, want TurnStarted", events[0])
	}
	started, ok := events[1].(event.ToolCallStarted)
	if !ok {
		t.Fatalf("event 1 = %+v, want ToolCallStarted", events[1])
	}
	if started.ID != "tc-1" || started.Name != "read_file" {
		t.Errorf("ToolCallStarted = %+v, want ID=tc-1 Name=read_file", started)
	}
	finished, ok := events[2].(event.ToolCallFinished)
	if !ok {
		t.Fatalf("event 2 = %+v, want ToolCallFinished", events[2])
	}
	if finished.ID != "tc-1" || !finished.IsError {
		t.Errorf("ToolCallFinished = %+v, want ID=tc-1 IsError=true (no tool registry configured)", finished)
	}

	if _, ok := events[3].(event.MessageStarted); !ok {
		t.Errorf("event 3 = %+v, want MessageStarted", events[3])
	}
	if fin, ok := events[5].(event.MessageFinished); !ok || fin.Content != "done" {
		t.Errorf("event 5 = %+v, want MessageFinished(content=done)", events[5])
	}
	tf, ok := events[6].(event.TurnFinished)
	if !ok {
		t.Fatalf("event 6 = %+v, want TurnFinished", events[6])
	}
	if tf.StopReason != "end_turn" {
		t.Errorf("TurnFinished.StopReason = %q, want end_turn", tf.StopReason)
	}
}

// blockingProvider is a hand-scripted [provider.Provider] whose first model
// call blocks until its ctx is cancelled — the seam TestInterrupt uses to
// deterministically observe an in-flight turn being interrupted, mirroring
// internal/daemon's own blockingProvider (unexported to that package, so
// redeclared here).
type blockingProvider struct {
	started chan struct{}
}

func newBlockingProvider() *blockingProvider {
	return &blockingProvider{started: make(chan struct{})}
}

func (p *blockingProvider) Info() provider.ModelInfo { return provider.ModelInfo{ID: "block-test"} }

func (p *blockingProvider) Stream(ctx context.Context, _ provider.Request) (provider.StreamHandle, error) {
	return &blockingStream{p: p, ctx: ctx}, nil
}

type blockingStream struct {
	p   *blockingProvider
	ctx context.Context
	n   int
}

func (s *blockingStream) Next() (provider.StreamEvent, error) {
	s.n++
	if s.n == 1 {
		close(s.p.started)
		<-s.ctx.Done()
		return provider.StreamEvent{Type: provider.StreamTextDelta, Text: "hello"}, nil
	}
	return provider.StreamEvent{}, io.EOF
}

func (s *blockingStream) Close() error { return nil }

// TestInterrupt asserts Interrupt sends session/cancel and the in-flight
// turn resolves with TurnFinished(stop=cancelled) rather than hanging.
func TestInterrupt(t *testing.T) {
	bp := newBlockingProvider()
	sup := newTestSupervisor(t, func() provider.Provider { return bp })
	url := newTestDaemon(t, sup)
	b := newBridge(t, url)

	info, err := b.Create(context.Background(), "", tui.CreateOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sub, err := b.Subscribe(context.Background(), info.ID)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	if err := b.Send(context.Background(), info.ID, "hi"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case <-bp.started:
	case <-time.After(defaultWait):
		t.Fatal("timed out waiting for the turn to start")
	}

	if err := b.Interrupt(context.Background(), info.ID); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	// blockingStream unblocks on cancellation by returning one ordinary text
	// delta (see its doc — mirroring internal/daemon's own blockingProvider),
	// so the loop's own pre-Next ctx check catches the cancellation on its
	// NEXT iteration, not this one: turn.started, message.started(text),
	// message.delta, message.finished(text) [flushed by the cancellation
	// check], turn.finished(cancelled) = 5 events.
	events := drainEvents(t, sub, 5)
	if _, ok := events[0].(event.TurnStarted); !ok {
		t.Errorf("event 0 = %+v, want TurnStarted", events[0])
	}
	if fin, ok := events[3].(event.MessageFinished); !ok || fin.Content != "hello" {
		t.Errorf("event 3 = %+v, want MessageFinished(content=hello)", events[3])
	}
	tf, ok := events[4].(event.TurnFinished)
	if !ok {
		t.Fatalf("event 4 = %+v, want TurnFinished", events[4])
	}
	if tf.StopReason != "cancelled" {
		t.Errorf("TurnFinished.StopReason = %q, want cancelled", tf.StopReason)
	}
}

// TestKillArchive asserts Kill and Archive both call the right daemon method
// and drop the session from a subsequent Roster.
func TestKillArchive(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	url := newTestDaemon(t, sup)
	b := newBridge(t, url)

	killed, err := b.Create(context.Background(), "", tui.CreateOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.Kill(context.Background(), killed.ID); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	archived, err := b.Create(context.Background(), "", tui.CreateOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.Archive(context.Background(), archived.ID); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	roster, err := b.Roster(context.Background())
	if err != nil {
		t.Fatalf("Roster: %v", err)
	}
	if len(roster) != 0 {
		t.Errorf("Roster after kill+archive = %+v, want empty", roster)
	}
}

// TestArchiveRunningRejected covers the daemon's ErrRunning surfacing
// through the bridge as a plain error (the TUI just shows opDoneMsg.err —
// see internal/tui/app.go).
func TestArchiveRunningRejected(t *testing.T) {
	bp := newBlockingProvider()
	sup := newTestSupervisor(t, func() provider.Provider { return bp })
	url := newTestDaemon(t, sup)
	b := newBridge(t, url)

	info, err := b.Create(context.Background(), "", tui.CreateOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := b.Send(context.Background(), info.ID, "hi"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	<-bp.started

	if err := b.Archive(context.Background(), info.ID); err == nil {
		t.Fatal("Archive while running: want an error, got none")
	}

	if err := b.Interrupt(context.Background(), info.ID); err != nil {
		t.Fatalf("Interrupt (cleanup): %v", err)
	}
}
