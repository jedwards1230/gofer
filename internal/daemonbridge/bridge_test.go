package daemonbridge_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/loop"
	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/provider/faux"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/daemonbridge"
	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
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
//
// It clears the Guard/Approver/Tools the supervisor now always injects (see
// internal/supervisor's sessionGuard, wired in as of the M3 approvals-relay
// work): this package's reconstruction tests exercise the WIRE projection
// (session/update, and now gofer/permission_requested/resolved — see
// TestPermissionRelayEndToEnd below for that one, deliberately left wired),
// not the SDK's own permission/sandbox decisions, which are internal/
// supervisor's and internal/sandbox's own test suites' job. Without this, a
// hand-scripted provider's tool call the sandbox can't prove contained (e.g.
// TestToolCallReconstruction's deliberately-unregistered "read_file") would
// hang the whole suite awaiting an approval no test here answers.
func newTestSupervisor(t *testing.T, newProvider func() provider.Provider) *supervisor.Supervisor {
	t.Helper()
	return newTestSupervisorGuarded(t, newProvider, false)
}

// newTestSupervisorGuarded is [newTestSupervisor] with the choice of whether
// to leave the supervisor's default Guard/Approver/Tools wiring intact —
// guarded=true for TestPermissionRelayEndToEnd, which needs the real
// permission pipeline; guarded=false (newTestSupervisor) strips it for every
// other test in this file, per newTestSupervisor's doc.
func newTestSupervisorGuarded(t *testing.T, newProvider func() provider.Provider, guarded bool) *supervisor.Supervisor {
	t.Helper()
	root := t.TempDir()
	store, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		t.Fatalf("session.NewFileStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	strip := func(opts runner.Options) runner.Options {
		if !guarded {
			opts.Guard, opts.Approver, opts.Tools = nil, nil, nil
		}
		return opts
	}
	cfg := supervisor.Config{
		Root:  root,
		Store: store,
		NewSession: func(ctx context.Context, opts runner.Options) (supervisor.Session, error) {
			opts.Store = store
			opts.Model = "faux"
			opts.Provider = newProvider()
			return runner.New(ctx, strip(opts))
		},
		ResumeSession: func(ctx context.Context, id string, opts runner.Options) (supervisor.Session, error) {
			opts.Store = store
			opts.Model = "faux"
			opts.Provider = newProvider()
			return runner.Resume(ctx, id, strip(opts))
		},
	}
	sup, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })
	return sup
}

// newTestSupervisorWithTools is [newTestSupervisor] with a caller-supplied
// tool registry threaded into runner.Options.Tools instead of the stripped
// nil newTestSupervisor leaves it at — the seam TestFidelityToolCallDeltaAndSpill
// (fidelity_test.go) uses to exercise a hand-rolled loop.Tool that returns
// Diagnostics + enough output to spill, neither of which the builtin tool
// registry (or an unregistered-tool error path) ever populates. Guard/
// Approver are still stripped (nil), same as newTestSupervisor: this test
// exercises the WIRE projection, not the sandbox/permission pipeline.
func newTestSupervisorWithTools(t *testing.T, newProvider func() provider.Provider, tools loop.ToolRegistry) *supervisor.Supervisor {
	t.Helper()
	root := t.TempDir()
	store, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		t.Fatalf("session.NewFileStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	build := func(opts runner.Options) runner.Options {
		opts.Store = store
		opts.Model = "faux"
		opts.Guard, opts.Approver = nil, nil
		opts.Tools = tools
		return opts
	}
	cfg := supervisor.Config{
		Root:  root,
		Store: store,
		NewSession: func(ctx context.Context, opts runner.Options) (supervisor.Session, error) {
			opts.Provider = newProvider()
			return runner.New(ctx, build(opts))
		},
		ResumeSession: func(ctx context.Context, id string, opts runner.Options) (supervisor.Session, error) {
			opts.Provider = newProvider()
			return runner.Resume(ctx, id, build(opts))
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

// twoTurnFauxProvider returns a provider.Provider constructor replaying
// faux.Default()'s single turn TWICE — one Stream call per turn, per
// [faux.Script]'s doc — so a session driven by it can take a first turn (used
// to seed history), then, once resumed on a fresh connection, a second,
// live-reconstructed turn with byte-identical content to the first.
func twoTurnFauxProvider() provider.Provider {
	turn := faux.Default().Turns[0]
	return faux.New(faux.Script{Turns: []faux.Turn{turn, turn}})
}

// drainEvents reads exactly n events from sub, failing the test if it times
// out or the subscription closes early.
func drainEvents(t *testing.T, sub *event.Subscription, n int) []event.Event {
	t.Helper()
	return drainEventsDiag(t, sub, n, nil)
}

// drainEventsDiag is [drainEvents] with an optional diagnosis hook appended to
// its failure messages. A history-replay drain passes a [foldProbe]'s
// diagnosis so a short or stalled replay reports what the fold contained AT
// READ TIME — which distinguishes a replay that was short at the SOURCE from
// one whose events went astray in DELIVERY. diag is called only on the failure
// path, and receives the events drained so far.
func drainEventsDiag(t *testing.T, sub *event.Subscription, n int, diag func([]event.Event) string) []event.Event {
	t.Helper()
	out := make([]event.Event, 0, n)
	note := func() string {
		if diag == nil {
			return ""
		}
		return diag(out)
	}
	for i := 0; i < n; i++ {
		select {
		case e, ok := <-sub.C:
			if !ok {
				t.Fatalf("subscription closed after %d/%d events%s", i, n, note())
			}
			out = append(out, e)
		case <-time.After(defaultWait):
			t.Fatalf("timed out waiting for event %d/%d%s", i, n, note())
		}
	}
	return out
}

// drainFirstTurnEvents drains the leading session.info event the supervisor
// emits on a session's FIRST prompt — asserting it carries wantTitle, the title
// gofer derives from that prompt (see supervisor/managed.go's enqueue) — then n
// turn events, returning just the turn events. It lets a first-prompt test keep
// its turn-event indices unchanged while still proving the title reconstructs
// over the bridge like every other event kind. The title is a one-shot,
// first-prompt-only event: a session's later turns (and a fresh mid-session
// attach, which recovers the title from the roster snapshot instead) never
// re-see it, so only first-prompt drains use this variant.
func drainFirstTurnEvents(t *testing.T, sub *event.Subscription, wantTitle string, n int) []event.Event {
	t.Helper()
	all := drainEvents(t, sub, n+1)
	info, ok := all[0].(event.SessionInfoUpdated)
	if !ok {
		t.Fatalf("first event = %+v, want session.info(title=%q)", all[0], wantTitle)
	}
	if info.Title != wantTitle {
		t.Errorf("session.info title = %q, want %q", info.Title, wantTitle)
	}
	return all[1:]
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
// provider's default script and asserts the exact replayed event sequence:
// the user's own prompt (started/finished, no deltas — see
// event.MessageUser's doc), THEN TurnStarted (the SDK's runner.Prompt
// publishes the user message before driving the loop, which is what emits
// TurnStarted — see runner.Runner.Prompt's doc; this is the source order,
// which lossless attach now replays verbatim instead of the M2 bridge's old
// synthesized TurnStarted-then-user-pair order), the reasoning message
// (started/2 deltas/finished), the text message (started/3 deltas/finished),
// then TurnFinished with the scripted stop reason — TurnFinished strictly
// after every delta.
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

	// session.info (the title derived from this first prompt, "hi"), then:
	// message.started(user), message.finished(user), turn.started,
	// message.started(reasoning), 2x message.delta, message.finished(reasoning),
	// message.started(text), 3x message.delta, message.finished(text),
	// turn.finished = 13 turn events after the leading title event.
	events := drainFirstTurnEvents(t, sub, "hi", 13)

	wantKinds := []string{
		"message.started", "message.finished",
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

	userStart, ok := events[0].(event.MessageStarted)
	if !ok || userStart.MessageKind != event.MessageUser {
		t.Errorf("event 0 = %+v, want MessageStarted(user)", events[0])
	}
	if fin, ok := events[1].(event.MessageFinished); !ok || fin.MessageKind != event.MessageUser || fin.Content != "hi" {
		t.Errorf("event 1 (user finished) = %+v, want MessageFinished(user, content=hi)", events[1])
	}

	reasoning, ok := events[3].(event.MessageStarted)
	if !ok || reasoning.MessageKind != event.MessageReasoning {
		t.Errorf("event 3 = %+v, want MessageStarted(reasoning)", events[3])
	}
	if fin, ok := events[6].(event.MessageFinished); !ok || fin.Content != "The user said hello. I'll greet them back." {
		t.Errorf("event 6 (reasoning finished) = %+v, want the joined reasoning chunks", events[6])
	}
	textStart, ok := events[7].(event.MessageStarted)
	if !ok || textStart.MessageKind != event.MessageText {
		t.Errorf("event 7 = %+v, want MessageStarted(text)", events[7])
	}
	if fin, ok := events[11].(event.MessageFinished); !ok || fin.Content != "Hello! How can I help you today?" {
		t.Errorf("event 11 (text finished) = %+v, want the joined text chunks", events[11])
	}

	tf, ok := events[12].(event.TurnFinished)
	if !ok {
		t.Fatalf("event 12 = %+v, want TurnFinished", events[12])
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

// wantMessage asserts events[start:] begins with a MessageStarted(kind), one
// or more MessageDelta events, then a MessageFinished(kind, content) —
// failing with ctx as a label prefix on any mismatch — and returns the index
// of the first event after that run. The delta count is intentionally
// unchecked (and so unbounded) here: a history replay always folds a
// message into exactly one delta (see [acp.ReplayNotifications]), while a
// live turn deltas exactly as the provider script does (see
// TestSendReconstructsTranscript), so a caller comparing the two shapes for
// the SAME scripted content chains wantMessage calls rather than hardcoding
// either count.
func wantMessage(t *testing.T, ctx string, events []event.Event, start int, kind event.MessageKind, content string) int {
	t.Helper()
	i := start
	started, ok := events[i].(event.MessageStarted)
	if !ok || started.MessageKind != kind {
		t.Fatalf("%s: event %d = %+v, want MessageStarted(kind=%v)", ctx, i, events[i], kind)
	}
	i++
	for i < len(events) {
		if _, ok := events[i].(event.MessageDelta); !ok {
			break
		}
		i++
	}
	if i >= len(events) {
		t.Fatalf("%s: ran out of events after %d deltas, want a trailing MessageFinished", ctx, i-start-1)
	}
	fin, ok := events[i].(event.MessageFinished)
	if !ok {
		t.Fatalf("%s: event %d = %+v, want MessageFinished", ctx, i, events[i])
	}
	if fin.MessageKind != kind || fin.Content != content {
		t.Errorf("%s: event %d = %+v, want MessageFinished(kind=%v, content=%q)", ctx, i, fin, kind, content)
	}
	return i + 1
}

// TestAttachReplaysHistory covers the bug this change fixes: attaching to a
// session over the daemon rendered a blank transcript even when the session
// had prior turns, because daemonbridge only ever reconstructed a session's
// event stream from LIVE notifications. It drives one full turn through a
// first bridge connection (seeding history the daemon's supervisor keeps
// live across connections — see [supervisor.Supervisor.Resume]'s
// already-live no-op), closes that connection, then opens a SECOND bridge —
// the same shape a fresh `gofer attach <id>` takes — and asserts its
// Subscribe replays that history (via the triggered session/load) BEFORE a
// subsequently-sent live turn's events, with nothing duplicated.
func TestAttachReplaysHistory(t *testing.T) {
	sup := newTestSupervisor(t, twoTurnFauxProvider)
	url := newTestDaemon(t, sup)

	b1 := newBridge(t, url)
	info, err := b1.Create(context.Background(), "", tui.CreateOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sub1, err := b1.Subscribe(context.Background(), info.ID)
	if err != nil {
		t.Fatalf("Subscribe (b1): %v", err)
	}
	if err := b1.Send(context.Background(), info.ID, "hi"); err != nil {
		t.Fatalf("Send (b1): %v", err)
	}
	drainFirstTurnEvents(t, sub1, "hi", 13) // first turn settles fully (with its leading title) — see TestSendReconstructsTranscript
	sub1.Close()
	if err := b1.Close(); err != nil {
		t.Fatalf("b1.Close: %v", err)
	}

	// A brand-new bridge connection sees none of the above as live events
	// (each daemonbridge.Supervisor reconstructs only from its OWN
	// connection's notifications) — its Subscribe must trigger a
	// session/load to backfill them, since the session itself stayed live
	// server-side (b1.Close only tore down the client connection).
	//
	// This test advances on the EVENT stream, which reaches turn.finished before
	// the runner's consume goroutine has necessarily appended the turn — so wait
	// for the turn to be journaled before reattaching, and the session/load that
	// Subscribe triggers provably reads a COMPLETE fold (the journal is
	// append-only, so a fold observed whole stays whole). The three blocks are
	// the user's text, the assistant's reasoning and its text — exactly the six
	// events asserted below. See awaitFoldComplete's doc.
	awaitFoldComplete(t, sup, info.ID, 3)

	// Snapshot the fold immediately before that Subscribe, so a short or stalled
	// replay below still reports whether the history was incomplete AT READ TIME.
	// The wait above should make that impossible; the bracket stays so that if it
	// ever happens anyway, the failure is conclusive on sight. See foldProbe's doc.
	probe := newFoldProbe(t, sup, info.ID)

	b2 := newBridge(t, url)
	sub2, err := b2.Subscribe(context.Background(), info.ID)
	if err != nil {
		t.Fatalf("Subscribe (b2): %v", err)
	}
	defer sub2.Close()

	// History replay: the prior turn's user prompt (started/finished, no
	// deltas — event.MessageUser is never streamed), then its reasoning
	// message, then its text message — with verbatim gofer/event replay
	// (internal/daemon's historyEvents), each is JUST its settled
	// started/finished pair, no synthesized deltas — 6 events. No
	// TurnStarted/TurnFinished (a history replay carries no turn-lifecycle
	// boundary of its own — see loadHistory's doc).
	history := drainEventsDiag(t, sub2, 6, func(got []event.Event) string {
		return probe.diagnosis(eventKinds(got))
	})
	next := wantMessage(t, "history", history, 0, event.MessageUser, "hi")
	next = wantMessage(t, "history", history, next, event.MessageReasoning, "The user said hello. I'll greet them back.")
	next = wantMessage(t, "history", history, next, event.MessageText, "Hello! How can I help you today?")
	if next != len(history) {
		t.Errorf("history: %d trailing event(s) after both messages, want none%s",
			len(history)-next, probe.diagnosis(eventKinds(history)))
	}

	// A live turn sent on the reattached bridge lands strictly after the
	// history above: drainEvents already consumed exactly those 6 events, in
	// that order, before Send is even issued below.
	if err := b2.Send(context.Background(), info.ID, "again"); err != nil {
		t.Fatalf("Send (b2): %v", err)
	}
	// message.started(user)/message.finished(user), THEN turn.started (source
	// order — see TestSendReconstructsTranscript's doc), then the reasoning
	// and text messages, then the terminal turn.finished.
	live := drainEvents(t, sub2, 13)
	next = wantMessage(t, "live", live, 0, event.MessageUser, "again")
	if _, ok := live[next].(event.TurnStarted); !ok {
		t.Errorf("live event %d = %+v, want TurnStarted", next, live[next])
	}
	next++
	next = wantMessage(t, "live", live, next, event.MessageReasoning, "The user said hello. I'll greet them back.")
	next = wantMessage(t, "live", live, next, event.MessageText, "Hello! How can I help you today?")
	if next != len(live)-1 {
		t.Errorf("live: %d trailing event(s) before the terminal TurnFinished, want none", len(live)-1-next)
	}
	tf, ok := live[len(live)-1].(event.TurnFinished)
	if !ok {
		t.Fatalf("live event %d = %+v, want TurnFinished", len(live)-1, live[len(live)-1])
	}
	if tf.StopReason != "end_turn" {
		t.Errorf("live TurnFinished.StopReason = %q, want end_turn", tf.StopReason)
	}

	// No stray or duplicate event beyond the 6 history + 13 live: history
	// isn't replayed twice, and the live turn's own events don't repeat.
	select {
	case e, ok := <-sub2.C:
		if ok {
			t.Errorf("unexpected extra event after the live turn: %+v", e)
		}
	case <-time.After(50 * time.Millisecond):
	}
}

// TestGoldenAttachHistoryReplayRendersUserTurn is TestAttachReplaysHistory's
// sibling for bug 1's ACTUAL rendering: it drives the exact same
// first-connection-then-reattach shape (seed one turn, close the
// connection, reattach on a fresh bridge so Subscribe triggers a
// session/load replay), then feeds the reconstructed history straight into
// a real [tui.Model] — the same Ingest an attached App uses — and asserts
// the rendered transcript shows the user's prompt above the agent's reply.
// This is the end-to-end proof for the gofer/event history replay (see
// internal/daemon's historyEvents and reconstruct.go's handleGoferEvent): a
// real daemon round trip, over the real wire, replayed through the real
// render path, not just an event-kind assertion.
func TestGoldenAttachHistoryReplayRendersUserTurn(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	url := newTestDaemon(t, sup)

	b1 := newBridge(t, url)
	info, err := b1.Create(context.Background(), "", tui.CreateOptions{Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sub1, err := b1.Subscribe(context.Background(), info.ID)
	if err != nil {
		t.Fatalf("Subscribe (b1): %v", err)
	}
	if err := b1.Send(context.Background(), info.ID, "hi"); err != nil {
		t.Fatalf("Send (b1): %v", err)
	}
	drainFirstTurnEvents(t, sub1, "hi", 13) // see TestSendReconstructsTranscript
	sub1.Close()
	if err := b1.Close(); err != nil {
		t.Fatalf("b1.Close: %v", err)
	}

	b2 := newBridge(t, url)
	sub2, err := b2.Subscribe(context.Background(), info.ID)
	if err != nil {
		t.Fatalf("Subscribe (b2): %v", err)
	}
	defer sub2.Close()

	history := drainEvents(t, sub2, 6) // see TestAttachReplaysHistory

	m := tui.New(theme.Test())
	for _, e := range history {
		m = m.Ingest(e)
	}
	got := testkit.Render(m, testkit.Width, testkit.Height)
	testkit.AssertGolden(t, "attach_history_replay_user_turn", got)
}

// TestCreateSkipsHistoryLoad asserts that a session THIS bridge just created
// never triggers a session/load: [Supervisor.Create] pre-registers it as
// history-free via registerFresh, so its subsequent Subscribe/Send see only
// the live turn's own events — no extra replay-shaped events in front of
// them (which would silently double a Create-then-Send session's transcript
// on every attach, not just a resumed one).
func TestCreateSkipsHistoryLoad(t *testing.T) {
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

	// drainFirstTurnEvents asserts the ONLY event ahead of the live turn is the
	// first-prompt title (session.info), not a spurious session/load history
	// replay — the exact regression this test guards.
	events := drainFirstTurnEvents(t, sub, "hi", 13)
	// The live turn's own first events (message.started(user)/message.finished
	// (user, "hi"), then turn.started — see TestSendReconstructsTranscript's
	// doc for why the user pair leads) must be immediately adjacent, with
	// nothing else in front of them: a spurious session/load replay would
	// insert its own message.started/finished pairs (with no turn.started
	// between them — a history replay carries no turn-lifecycle boundary, see
	// loadHistory's doc) ahead of this shape.
	userStart, ok := events[0].(event.MessageStarted)
	if !ok || userStart.MessageKind != event.MessageUser {
		t.Fatalf("event 0 = %+v, want MessageStarted(user) (a session/load replay would have inserted events ahead of it)", events[0])
	}
	if fin, ok := events[1].(event.MessageFinished); !ok || fin.MessageKind != event.MessageUser || fin.Content != "hi" {
		t.Fatalf("event 1 = %+v, want MessageFinished(user, content=hi)", events[1])
	}
	if _, ok := events[2].(event.TurnStarted); !ok {
		t.Fatalf("event 2 = %+v, want TurnStarted (a session/load replay would have inserted events between the user pair and it)", events[2])
	}

	select {
	case e, ok := <-sub.C:
		if ok {
			t.Errorf("unexpected extra event: %+v", e)
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

// TestToolCallReconstruction asserts a tool call replays to
// ToolCallStarted/ToolCallFinished, with the source's TRUE turn boundaries
// around it: the SDK's loop publishes a round's turn.finished(tool_use) as
// soon as the MODEL CALL settles (loop.callModel), and only THEN executes
// the requested tool (loop.runOneTool, called from Run after callModel
// returns) — so ToolCallFinished lands AFTER that round's turn.finished, and
// a fresh turn.started opens the next round. ACP's session/update has no
// turn.started/turn.finished projection at all, so the M2 reconstruction
// never surfaced this true ordering; lossless attach now replays it
// verbatim.
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

	// session.info (title "read a.txt"), then: message.started(user),
	// message.finished(user), turn.started, tool.call.started,
	// turn.finished(tool_use), tool.call.finished, turn.started,
	// message.started(text), message.delta, message.finished(text),
	// turn.finished(end_turn) = 11 turn events after the leading title event.
	events := drainFirstTurnEvents(t, sub, "read a.txt", 11)

	if fin, ok := events[1].(event.MessageFinished); !ok || fin.MessageKind != event.MessageUser || fin.Content != "read a.txt" {
		t.Errorf("event 1 (user finished) = %+v, want MessageFinished(user, content=read a.txt)", events[1])
	}
	if _, ok := events[2].(event.TurnStarted); !ok {
		t.Errorf("event 2 = %+v, want TurnStarted", events[2])
	}
	started, ok := events[3].(event.ToolCallStarted)
	if !ok {
		t.Fatalf("event 3 = %+v, want ToolCallStarted", events[3])
	}
	if started.ID != "tc-1" || started.Name != "read_file" {
		t.Errorf("ToolCallStarted = %+v, want ID=tc-1 Name=read_file", started)
	}
	if tf, ok := events[4].(event.TurnFinished); !ok || tf.StopReason != "tool_use" {
		t.Fatalf("event 4 = %+v, want TurnFinished(stop_reason=tool_use)", events[4])
	}
	finished, ok := events[5].(event.ToolCallFinished)
	if !ok {
		t.Fatalf("event 5 = %+v, want ToolCallFinished", events[5])
	}
	if finished.ID != "tc-1" || !finished.IsError {
		t.Errorf("ToolCallFinished = %+v, want ID=tc-1 IsError=true (no tool registry configured)", finished)
	}
	if _, ok := events[6].(event.TurnStarted); !ok {
		t.Errorf("event 6 = %+v, want TurnStarted (the second model-call round)", events[6])
	}

	if _, ok := events[7].(event.MessageStarted); !ok {
		t.Errorf("event 7 = %+v, want MessageStarted", events[7])
	}
	if fin, ok := events[9].(event.MessageFinished); !ok || fin.Content != "done" {
		t.Errorf("event 9 = %+v, want MessageFinished(content=done)", events[9])
	}
	tf, ok := events[10].(event.TurnFinished)
	if !ok {
		t.Fatalf("event 10 = %+v, want TurnFinished", events[10])
	}
	if tf.StopReason != "end_turn" {
		t.Errorf("TurnFinished.StopReason = %q, want end_turn", tf.StopReason)
	}
}

// TestPermissionRelayEndToEnd is this package's live proof of the M3
// approvals-relay pipeline in full: a real supervisor (guarded — unlike
// every other test in this file, see newTestSupervisor's doc) evaluates
// toolTurnProvider's unregistered "read_file" call, can't prove it contained
// (see internal/sandbox's containableTool), and asks — the daemon fans that
// event.PermissionRequested out as a gofer/permission_requested notification
// (internal/daemon/handlers.go), this bridge reconstructs it (reconstruct.go's
// handlePermissionRequested), the test answers it through the SAME bridge
// method the TUI's approval dialog calls (Supervisor.Reply → contract #1's
// "permission.reply" op), and the resulting event.PermissionResolved +
// ToolCallFinished (still IsError=true: "read_file" is deliberately not a
// registered tool — see toolTurnProvider's doc) reconstruct right behind it.
func TestPermissionRelayEndToEnd(t *testing.T) {
	sup := newTestSupervisorGuarded(t, func() provider.Provider { return &toolTurnProvider{} }, true)
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

	// The turn blocks server-side once the guard asks — see runOneTool's
	// gate(), called only AFTER the requesting round's own turn.finished(
	// tool_use) has already been published (loop.callModel publishes it
	// before loop.Run ever calls runOneTool — see TestToolCallReconstruction's
	// doc) — so these 6 events arrive until this test replies:
	// message.started(user), message.finished(user), turn.started,
	// tool.call.started, turn.finished(tool_use), permission.requested — after
	// the leading session.info title event (title "read a.txt").
	before := drainFirstTurnEvents(t, sub, "read a.txt", 6)
	if _, ok := before[2].(event.TurnStarted); !ok {
		t.Fatalf("event 2 = %+v, want TurnStarted", before[2])
	}
	if _, ok := before[3].(event.ToolCallStarted); !ok {
		t.Fatalf("event 3 = %+v, want ToolCallStarted", before[3])
	}
	if tf, ok := before[4].(event.TurnFinished); !ok || tf.StopReason != "tool_use" {
		t.Fatalf("event 4 = %+v, want TurnFinished(stop_reason=tool_use)", before[4])
	}
	pr, ok := before[5].(event.PermissionRequested)
	if !ok {
		t.Fatalf("event 5 = %+v, want PermissionRequested", before[5])
	}
	if pr.Tool != "read_file" {
		t.Errorf("PermissionRequested.Tool = %q, want %q", pr.Tool, "read_file")
	}

	// Reply through the exact bridge method the TUI's approval dialog calls
	// (see internal/tui/dialog.go's doReply) — the client-side half of
	// contract #1, not a supervisor-internal shortcut.
	if err := b.Reply(context.Background(), info.ID, pr.ID, tui.PermissionDecision{Allow: true}); err != nil {
		t.Fatalf("Reply: %v", err)
	}

	// permission.resolved, tool.call.finished, turn.started (the second
	// model-call round), message.started(text), message.delta,
	// message.finished(text), turn.finished(end_turn) = 7 events.
	after := drainEvents(t, sub, 7)
	resolved, ok := after[0].(event.PermissionResolved)
	if !ok {
		t.Fatalf("event 0 (post-reply) = %+v, want PermissionResolved", after[0])
	}
	if resolved.ID != pr.ID || resolved.Verdict != event.VerdictAllow {
		t.Errorf("PermissionResolved = %+v, want ID=%q Verdict=allow", resolved, pr.ID)
	}
	finished, ok := after[1].(event.ToolCallFinished)
	if !ok {
		t.Fatalf("event 1 (post-reply) = %+v, want ToolCallFinished", after[1])
	}
	if finished.ID != "tc-1" || !finished.IsError {
		t.Errorf("ToolCallFinished = %+v, want ID=tc-1 IsError=true (read_file is deliberately unregistered)", finished)
	}
	if _, ok := after[2].(event.TurnStarted); !ok {
		t.Errorf("event 2 (post-reply) = %+v, want TurnStarted", after[2])
	}
	if fin, ok := after[5].(event.MessageFinished); !ok || fin.Content != "done" {
		t.Errorf("event 5 (post-reply) = %+v, want MessageFinished(content=done)", after[5])
	}
	tf, ok := after[6].(event.TurnFinished)
	if !ok {
		t.Fatalf("event 6 (post-reply) = %+v, want TurnFinished", after[6])
	}
	if tf.StopReason != "end_turn" {
		t.Errorf("TurnFinished.StopReason = %q, want end_turn", tf.StopReason)
	}
}

// TestAmendedPermissionReplyReachesTheGate is TestPermissionRelayEndToEnd for
// an AMENDED allow: the reply carries replacement tool input, and the call the
// SDK then runs must be the human's, not the model's.
//
// The proof is the post-reply ToolCallFinished. The SDK's loop substitutes
// event.PermissionReply.Input into call.Input inside awaitApproval and emits
// that call's input on tool.call.finished (loop.runOneTool), so an
// unsubstituted (or dropped) input shows up here as the model's original
// {"path":"a.txt"}. That single assertion covers every layer between: the
// bridge's wire params, the daemon's permissionReplyParams, and the gate.
func TestAmendedPermissionReplyReachesTheGate(t *testing.T) {
	sup := newTestSupervisorGuarded(t, func() provider.Provider { return &toolTurnProvider{} }, true)
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

	before := drainFirstTurnEvents(t, sub, "read a.txt", 6)
	pr, ok := before[5].(event.PermissionRequested)
	if !ok {
		t.Fatalf("event 5 = %+v, want PermissionRequested", before[5])
	}

	// The amended input the human "typed": the full original spec with the
	// path replaced, exactly as the TUI's editor builds it (see
	// tui.Model.AmendedInput).
	amended := json.RawMessage(`{"path":"b.txt"}`)
	d := tui.PermissionDecision{Allow: true, Input: amended}
	if err := b.Reply(context.Background(), info.ID, pr.ID, d); err != nil {
		t.Fatalf("Reply: %v", err)
	}

	after := drainEvents(t, sub, 7)
	resolved, ok := after[0].(event.PermissionResolved)
	if !ok {
		t.Fatalf("event 0 (post-reply) = %+v, want PermissionResolved", after[0])
	}
	if resolved.Verdict != event.VerdictAllow {
		t.Errorf("PermissionResolved.Verdict = %q, want allow", resolved.Verdict)
	}
	finished, ok := after[1].(event.ToolCallFinished)
	if !ok {
		t.Fatalf("event 1 (post-reply) = %+v, want ToolCallFinished", after[1])
	}
	if got := string(finished.Input); got != string(amended) {
		t.Errorf("the executed call's input = %s, want the amended %s — the replacement input did not reach the gate", got, amended)
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
	// NEXT iteration, not this one: message.started(user), message.finished
	// (user), turn.started (source order — see TestSendReconstructsTranscript's
	// doc), message.started(text), message.delta, message.finished(text)
	// [flushed by the cancellation check], session.error(fatal) [the loop's
	// own emitError on the cancelled path — a kind ACP's session/update has
	// no projection for at all, so this is only visible via gofer/event],
	// turn.finished(cancelled) = 8 turn events after the leading session.info
	// title event (title "hi").
	events := drainFirstTurnEvents(t, sub, "hi", 8)
	if fin, ok := events[1].(event.MessageFinished); !ok || fin.MessageKind != event.MessageUser || fin.Content != "hi" {
		t.Errorf("event 1 (user finished) = %+v, want MessageFinished(user, content=hi)", events[1])
	}
	if _, ok := events[2].(event.TurnStarted); !ok {
		t.Errorf("event 2 = %+v, want TurnStarted", events[2])
	}
	if fin, ok := events[5].(event.MessageFinished); !ok || fin.Content != "hello" {
		t.Errorf("event 5 = %+v, want MessageFinished(content=hello)", events[5])
	}
	if se, ok := events[6].(event.SessionError); !ok || !se.Fatal {
		t.Errorf("event 6 = %+v, want a fatal SessionError", events[6])
	}
	tf, ok := events[7].(event.TurnFinished)
	if !ok {
		t.Fatalf("event 7 = %+v, want TurnFinished", events[7])
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

// TestSubscribeAfterCloseErrors asserts that a Subscribe after Close returns an
// error rather than silently creating a fresh broker — one that nothing would
// ever close or publish to, hanging the subscription forever and leaking the
// broker.
func TestSubscribeAfterCloseErrors(t *testing.T) {
	sup := newTestSupervisor(t, fauxProvider)
	url := newTestDaemon(t, sup)
	b := newBridge(t, url)

	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	sub, err := b.Subscribe(context.Background(), "no-such-session")
	if err == nil {
		t.Fatal("Subscribe after Close: want an error, got nil (would leak a broker)")
	}
	if sub != nil {
		t.Fatal("Subscribe after Close: want a nil subscription")
	}
}
