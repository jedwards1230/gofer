package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/session"
)

// reportStub is the minimum [Session] reportToParentOnce touches: an id, a
// journal path (so the durable claim has a directory to live in), a fold, and a
// broker for the failure emit. Everything else panics if reached, which is the
// point — a change that made the report path call something else should be
// noticed here rather than silently tolerated.
type reportStub struct {
	id     string
	path   string
	fold   []provider.Message
	broker *event.Broker
}

func (s *reportStub) ID() string                  { return s.id }
func (s *reportStub) JournalPath() string         { return s.path }
func (s *reportStub) Fold() []provider.Message    { return s.fold }
func (s *reportStub) Events() *event.Subscription { return s.broker.Subscribe(event.FilterAll, 8) }
func (s *reportStub) EventsLive() *event.Subscription {
	return s.broker.SubscribeLive(event.FilterAll, 8)
}
func (s *reportStub) Emit(e event.Event)                    { s.broker.Publish(e) }
func (s *reportStub) Cost() session.CostReport              { return session.CostReport{} }
func (s *reportStub) Prompt(context.Context, string) error  { panic("reportStub.Prompt") }
func (s *reportStub) SetModel(string) error                 { panic("reportStub.SetModel") }
func (s *reportStub) SetEffort(string) error                { panic("reportStub.SetEffort") }
func (s *reportStub) Compact(context.Context, string) error { panic("reportStub.Compact") }
func (s *reportStub) LastUsage() (string, provider.Usage, bool) {
	return "", provider.Usage{}, false
}
func (s *reportStub) Close() error { s.broker.Close(); return nil }

// TestReportSurvivesASessionTeardown pins the report's context lifetime.
//
// A child's report is delivered from its pump AFTER its last turn settles, which
// is exactly when a Kill/Archive/Close is most likely to be racing it — and
// [managed.stop] cancels baseCtx as its first act. Deriving the report's context
// from baseCtx directly meant the LAST report of a session's life failed with
// context.Canceled, emitted a session.error onto a stream nobody was left
// watching, and burned the (already-persisted) at-most-once claim: the parent
// waits forever for a child that finished its work.
//
// So the report runs under context.WithoutCancel of baseCtx, with a deadline of
// its own. This drives the already-cancelled case directly rather than trying to
// win a race against a real teardown, which is what makes it deterministic.
func TestReportSurvivesASessionTeardown(t *testing.T) {
	dir := t.TempDir()
	sess := &reportStub{
		id:     "child-1",
		path:   filepath.Join(dir, "child-1.jsonl"),
		fold:   []provider.Message{provider.AssistantText("the flake is a shared temp dir")},
		broker: event.NewBroker(event.WithReplay(8)),
	}
	t.Cleanup(func() { _ = sess.Close() })

	// A baseCtx that is ALREADY cancelled — the state stop() leaves behind.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	type call struct {
		err      error
		deadline bool
		text     string
	}
	got := make(chan call, 2)
	m := &managed{
		sess:     sess,
		id:       sess.id,
		parentID: "parent-1",
		baseCtx:  ctx,
		reportParent: func(rctx context.Context, _, text string) error {
			_, hasDeadline := rctx.Deadline()
			got <- call{err: rctx.Err(), deadline: hasDeadline, text: text}
			return nil
		},
	}

	m.reportToParentOnce()

	select {
	case c := <-got:
		if c.err != nil {
			t.Fatalf("the report ran under a CANCELLED context (%v) — a child killed just after finishing would never reach its parent", c.err)
		}
		if !c.deadline {
			t.Error("the report's context carries no deadline; uncancellable must not mean unbounded")
		}
		if c.text == "" {
			t.Error("the report carried no text")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the report was never delivered")
	}

	// The claim still landed, so a resume after this teardown will not re-report.
	raw, err := os.ReadFile(filepath.Join(dir, "child-1.meta.json"))
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if !strings.Contains(string(raw), `"reported":true`) {
		t.Errorf("sidecar %s does not record the report claim", raw)
	}
}
