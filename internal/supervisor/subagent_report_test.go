package supervisor_test

// subagent_report_test.go covers the child→parent report path: a finished
// subagent's result reaching the session that spawned it.
//
// The claim under test is an ORDERING one, and it was read off the code before
// anything exercised it: a parent's automatic compaction runs on its pump
// goroutine WITHOUT holding m.mu, while enqueue takes m.mu — so a report
// arriving mid-compaction should queue and dispatch the instant compaction
// returns, rather than being rejected, dropped, or run concurrently with the
// summarizer. That was a hypothesis; these tests are the observation.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/provider/faux"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// newReportHarness builds a harness with agent-initiated subagents ENABLED and
// a low compaction threshold, so a scripted over-threshold usage figure fires
// the automatic trigger deterministically (the recipe compact_test.go uses).
func newReportHarness(t *testing.T) *harness {
	t.Helper()
	threshold := 0.5
	return newHarnessWithConfig(t, func(cfg *supervisor.Config) {
		cfg.SubagentsConfig = func() config.Subagents { return config.Subagents{Enabled: true} }
		cfg.Compaction = func() config.Compaction { return config.Compaction{ThresholdFraction: &threshold} }
	})
}

// reportPrompts returns the prompts sess was driven with that are child→parent
// reports, in dispatch order.
func reportPrompts(sess *fakeSession) []string {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	var out []string
	for _, c := range sess.calls {
		if strings.HasPrefix(c, "subagent ") {
			out = append(out, c)
		}
	}
	return out
}

// waitForQueue polls id's pending-prompt queue until it has want entries,
// reporting whether it got there. It returns rather than fataling so a caller
// can attach a failure message describing the property it was actually testing
// — "the report was dropped" and "the queue never filled" are the same
// observation but very different findings.
func waitForQueue(t *testing.T, sup *supervisor.Supervisor, id string, want int) ([]string, bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		q, err := sup.QueueList(context.Background(), id)
		if err != nil {
			t.Fatalf("QueueList(%s): %v", id, err)
		}
		if len(q) == want {
			return q, true
		}
		time.Sleep(time.Millisecond)
	}
	return nil, false
}

// TestSubagentReportLandsAfterParentCompaction is the load-bearing test for the
// whole report path. It runs a CHILD session to completion while its PARENT is
// held inside an automatic compaction, and pins three separate properties:
//
//   - ORDERING. The report is dispatched on the parent strictly AFTER the
//     compaction returns — asserted against an ordered cross-goroutine call log
//     (see callTrace), not merely by observing that both eventually happened.
//     The report is also proven to have ARRIVED while the compaction was still
//     in flight, which is what makes the ordering claim non-vacuous: a report
//     that only showed up afterwards would satisfy "after" trivially.
//   - NOT DROPPED. The parent actually ran the report as a turn. This is a
//     distinct failure mode from the one below and gets its own assertion,
//     because "never arrived" and "arrived twice" are different bugs with
//     different causes and a single count check cannot tell them apart.
//   - EXACTLY ONCE. Driving the child through a SECOND settled turn produces no
//     second report.
func TestSubagentReportLandsAfterParentCompaction(t *testing.T) {
	h := newReportHarness(t)
	ctx := context.Background()
	trace := &callTrace{}

	parent, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "claude-haiku-4-5"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	child, err := h.sup.Create(ctx, "", supervisor.CreateOptions{
		Cwd: "/proj", Model: "claude-haiku-4-5", ParentID: parent.ID, Agent: "go-developer",
	})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	ps, cs := h.session(parent.ID), h.session(child.ID)
	ps.setTrace(trace)
	cs.setTrace(trace)

	// Hold the parent inside its automatic compaction. claude-haiku-4-5's
	// registered ContextWindow is 200_000, so 150_000 input tokens is 75% —
	// over the harness's scripted 50% threshold.
	entered, release := ps.holdCompact()
	ps.setLastUsage("claude-haiku-4-5", provider.Usage{InputTokens: 150_000})

	if err := h.sup.Send(ctx, parent.ID, "parent work"); err != nil {
		t.Fatalf("Send parent: %v", err)
	}
	ps.waitStarted(t)
	ps.finish(t, nil)

	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("parent never entered automatic compaction")
	}

	// The child finishes its brief WHILE the parent is stuck compacting.
	cs.setFold([]provider.Message{
		provider.UserText("investigate the flaky build"),
		provider.AssistantText("the flake is a shared temp dir"),
	})
	if err := h.sup.Send(ctx, child.ID, "investigate the flaky build"); err != nil {
		t.Fatalf("Send child: %v", err)
	}
	cs.waitStarted(t)
	cs.finish(t, nil)

	// The report reached the parent's QUEUE while the compaction was still in
	// flight. Both halves matter: the first is the arrival, the second is what
	// makes the ordering assertion below meaningful rather than tautological.
	queued, ok := waitForQueue(t, h.sup, parent.ID, 1)
	if !ok {
		// Two very different bugs land here, so name which one this is rather
		// than reporting the symptom they share.
		if ran := reportPrompts(ps); len(ran) > 0 {
			t.Fatalf("parent RAN the report while still inside its compaction (%v) — "+
				"the report must queue and wait, never overlap the summarizer; trace = %v",
				ran, trace.snapshot())
		}
		t.Fatalf("the child's report was DROPPED: it never reached the parent's queue, "+
			"and the parent never ran it; trace = %v", trace.snapshot())
	}
	if !strings.HasPrefix(queued[0], "subagent go-developer (session "+child.ID+") finished") {
		t.Fatalf("queued prompt = %q, want the child's report", queued[0])
	}
	if trace.has(parent.ID + ":compact:end") {
		t.Fatal("parent's compaction had already returned when the report was queued — this run proves nothing about ordering")
	}

	release()

	// Once compaction returns, the parent's pump picks the report up.
	dispatched := ps.waitStarted(t)
	if !strings.Contains(dispatched, "the flake is a shared temp dir") {
		t.Fatalf("parent's next turn = %q, want the child's report text", dispatched)
	}
	ps.finish(t, nil)
	waitForStatus(t, h.sup, parent.ID, supervisor.StatusNeedsInput)

	// Ordering, off the recorded log rather than off wall-clock luck.
	entries := trace.snapshot()
	endAt := trace.indexOf(parent.ID + ":compact:end")
	reportAt := trace.indexOf(parent.ID + ":prompt:" + dispatched)
	switch {
	case endAt < 0:
		t.Fatalf("parent's compaction never completed; trace = %v", entries)
	case reportAt < 0:
		t.Fatalf("parent never ran the report as a turn; trace = %v", entries)
	case reportAt < endAt:
		t.Fatalf("report dispatched BEFORE the compaction finished (report at %d, compact:end at %d); trace = %v",
			reportAt, endAt, entries)
	}

	// NOT DROPPED — a distinct property from the count below.
	got := reportPrompts(ps)
	if len(got) == 0 {
		t.Fatalf("the child's report was DROPPED: the parent ran no report turn at all; trace = %v", entries)
	}

	// EXACTLY ONCE, across a second settled child turn.
	if err := h.sup.Send(ctx, child.ID, "and again"); err != nil {
		t.Fatalf("Send child again: %v", err)
	}
	cs.waitStarted(t)
	cs.finish(t, nil)
	waitForStatus(t, h.sup, child.ID, supervisor.StatusNeedsInput)
	// The parent is idle with an empty queue, so a second report would have to
	// be queued by now for the child's turn to have already settled.
	if q, err := h.sup.QueueList(ctx, parent.ID); err != nil {
		t.Fatalf("QueueList parent: %v", err)
	} else if len(q) != 0 {
		t.Fatalf("child reported a SECOND time: parent queue = %v", q)
	}
	if got = reportPrompts(ps); len(got) != 1 {
		t.Fatalf("parent received %d reports, want exactly 1 (DUPLICATED): %v", len(got), got)
	}
}

// TestSubagentReportCarriesTheChildsAnswer pins the report's content: the
// child's own final assistant text, attributed to the child by id and agent so
// a parent with several children can tell them apart. A report that named
// neither would be indistinguishable from the user typing.
func TestSubagentReportCarriesTheChildsAnswer(t *testing.T) {
	h := newReportHarness(t)
	ctx := context.Background()

	parent, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "claude-haiku-4-5"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	child, err := h.sup.Create(ctx, "", supervisor.CreateOptions{
		Cwd: "/proj", Model: "claude-haiku-4-5", ParentID: parent.ID, Agent: "go-reviewer",
	})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	ps, cs := h.session(parent.ID), h.session(child.ID)
	cs.setFold([]provider.Message{
		provider.UserText("review the diff"),
		provider.AssistantText("two nits, no blockers"),
	})

	if err := h.sup.Send(ctx, child.ID, "review the diff"); err != nil {
		t.Fatalf("Send child: %v", err)
	}
	cs.waitStarted(t)
	cs.finish(t, nil)

	// The parent is idle, so its pump dispatches the report immediately rather
	// than leaving it queued — read it off the turn it actually ran.
	report := ps.waitStarted(t)
	ps.finish(t, nil)
	for _, want := range []string{"go-reviewer", child.ID, "two nits, no blockers"} {
		if !strings.Contains(report, want) {
			t.Errorf("report %q does not carry %q", report, want)
		}
	}
}

// TestSubagentReportDisabledWithoutConfig is the opt-in assertion for the
// REPORT half. A child created the way an operator always could — `gofer run
// --parent <id>` — on a gofer that never enabled subagents must behave exactly
// as it did before this feature existed: its parent is never prompted.
func TestSubagentReportDisabledWithoutConfig(t *testing.T) {
	h := newHarness(t) // no SubagentsConfig at all: the zero value, disabled
	ctx := context.Background()

	parent, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: "/proj", Model: "claude-haiku-4-5"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	child, err := h.sup.Create(ctx, "", supervisor.CreateOptions{
		Cwd: "/proj", Model: "claude-haiku-4-5", ParentID: parent.ID, Agent: "go-developer",
	})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	cs := h.session(child.ID)
	cs.setFold([]provider.Message{provider.AssistantText("done")})

	if err := h.sup.Send(ctx, child.ID, "go"); err != nil {
		t.Fatalf("Send child: %v", err)
	}
	cs.waitStarted(t)
	cs.finish(t, nil)
	waitForStatus(t, h.sup, child.ID, supervisor.StatusNeedsInput)

	if q, err := h.sup.QueueList(ctx, parent.ID); err != nil {
		t.Fatalf("QueueList parent: %v", err)
	} else if len(q) != 0 {
		t.Fatalf("parent was prompted with subagents disabled: %v", q)
	}
	if got := reportPrompts(h.session(parent.ID)); len(got) != 0 {
		t.Fatalf("parent ran a report turn with subagents disabled: %v", got)
	}
}

// recordingSubagents is a [supervisor.Subagents] that records every report it
// is handed, for a test whose parent is not itself a live session.
type recordingSubagents struct {
	mu      sync.Mutex
	reports []string
}

func (r *recordingSubagents) Spawn(context.Context, string, string, string) (string, error) {
	return "", errors.New("recordingSubagents: Spawn is not used by these tests")
}

func (r *recordingSubagents) Report(_ context.Context, _, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reports = append(r.reports, text)
	return nil
}

func (r *recordingSubagents) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reports)
}

// TestSubagentReportSurvivesResumeExactlyOnce is the resume-boundary regression
// test, and it exists because an in-memory sync.Once cannot express the bound
// the docs promise.
//
// The Once belongs to a [managed]. Kill a child and `gofer resume` it — a real,
// user-reachable action from the CLI and the TUI's /resume — and the resumed
// child is a NEW managed with a NEW Once, so its next settled turn reported to
// the parent all over again. Every resume added another copy, which is the
// unbounded parent↔child prompting loop the bound exists to prevent.
//
// It runs against REAL *runner.Runner sessions over the faux provider on
// purpose: the fakeSession harness journals nothing, so the sidecar the durable
// claim lives in has no journal to sit beside and lookupDiskSession cannot find
// the session at all. A fake-based version of this test would be structurally
// blind to the bug.
func TestSubagentReportSurvivesResumeExactlyOnce(t *testing.T) {
	root, cwd := t.TempDir(), t.TempDir()
	store, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		t.Fatalf("session.NewFileStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	seam := &recordingSubagents{}
	sup, err := supervisor.New(supervisor.Config{
		Root:            root,
		Store:           store,
		Subagents:       seam,
		SubagentsConfig: func() config.Subagents { return config.Subagents{Enabled: true} },
		NewSession: func(ctx context.Context, opts runner.Options) (supervisor.Session, error) {
			opts.Store = store
			opts.Provider = faux.New(faux.Default())
			return runner.New(ctx, opts)
		},
		ResumeSession: func(ctx context.Context, id string, opts runner.Options) (supervisor.Session, error) {
			opts.Store = store
			opts.Provider = faux.New(faux.Default())
			return runner.Resume(ctx, id, opts)
		},
	})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	ctx := context.Background()
	parent, err := sup.Create(ctx, "", supervisor.CreateOptions{Cwd: cwd, Model: "faux-1"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	child, err := sup.Create(ctx, "", supervisor.CreateOptions{
		Cwd: cwd, Model: "faux-1", ParentID: parent.ID, Agent: "go-developer",
	})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}

	runChildTurn := func(t *testing.T) {
		t.Helper()
		sub, serr := sup.Subscribe(ctx, child.ID)
		if serr != nil {
			t.Fatalf("Subscribe: %v", serr)
		}
		if serr := sup.Send(ctx, child.ID, "do the work"); serr != nil {
			t.Fatalf("Send: %v", serr)
		}
		waitForTurnFinished(t, sub)
		waitForStatus(t, sup, child.ID, supervisor.StatusNeedsInput)
	}

	runChildTurn(t)
	waitForReports(t, seam, 1)

	// The claim is durable, so it is visible on disk before any resume happens.
	if !readReportedFlag(t, root, cwd, child.ID) {
		t.Fatal("the child reported but its sidecar records no claim — the bound is in-memory only and will not survive a resume")
	}

	// Kill and resume: a brand-new managed, a brand-new sync.Once.
	if err := sup.Kill(ctx, child.ID); err != nil {
		t.Fatalf("Kill child: %v", err)
	}
	if _, err := sup.Resume(ctx, child.ID, supervisor.ResumeOptions{Cwd: cwd, Model: "faux-1"}); err != nil {
		t.Fatalf("Resume child: %v", err)
	}
	runChildTurn(t)

	// Give a second report every chance to land before declaring it absent: the
	// report is delivered from the child's pump, which has already run its turn
	// to completion above.
	time.Sleep(150 * time.Millisecond)
	if got := seam.count(); got != 1 {
		t.Fatalf("parent received %d reports across a kill+resume, want exactly 1 (DUPLICATED — the bound did not survive the resume): %v",
			got, seam.reports)
	}
}

// waitForReports polls the seam until it has recorded want reports.
func waitForReports(t *testing.T, seam *recordingSubagents, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if seam.count() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the child's report was DROPPED: %d reports reached the parent, want %d", seam.count(), want)
}

// readReportedFlag reads the durable one-report claim out of the child's
// sidecar. It asserts on the FILE rather than on any in-process state, which is
// the whole point: the claim has to outlive the process.
func readReportedFlag(t *testing.T, root, cwd, id string) bool {
	t.Helper()
	path := filepath.Join(root, "sessions", session.Slugify(cwd), id+".meta.json")
	raw, err := os.ReadFile(path) //nolint:gosec // test-owned temp path
	if err != nil {
		t.Fatalf("read sidecar %s: %v", path, err)
	}
	var got struct {
		Reported bool `json:"reported"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal sidecar %s: %v", raw, err)
	}
	return got.Reported
}
