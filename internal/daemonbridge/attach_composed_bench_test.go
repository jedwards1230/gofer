package daemonbridge_test

// attach_composed_bench_test.go measures the COMPOSED attach path — everything
// between "attach is requested" and "the transcript is ready to render" —
// in-process, over the real wire, through the real client.
//
// # Why a composed benchmark, when every component already had one
//
// gofer#313 (a cold `gofer attach` took 2.02s to show a transcript) existed
// even though every component on that path had a green benchmark of its own:
// internal/supervisor's roster benchmarks, internal/tui's
// BenchmarkTranscriptIngest/View, internal/daemon's BenchmarkBroadcastRawEvent.
// None of them attaches. The cost lived in the SEAM between them — the daemon's
// session/load waiting on a settle condition its own Resume had already made
// unsatisfiable — and a seam is exactly what a per-package benchmark cannot
// see. So this one composes the whole path instead of reconstructing what it is
// believed to do:
//
//	daemon.Dial + daemonbridge.New          — the client half of `gofer attach`
//	  -> Reconstructor.Subscribe            — first reference triggers the load
//	    -> gofer/roster + gofer/overview    — sessionCwd's two RPCs
//	    -> session/load                     — supervisor.Resume, AwaitSettled,
//	                                          History fold, ReplayNotifications,
//	                                          historyEvents, N wire frames
//	  -> demuxer -> per-session broker      — client-side reconstruction
//	    -> tui.Model.Ingest per event       — the transcript the operator sees
//
// It drives the same entry point the TUI does: cmd/gofer's runAttach dials,
// builds a daemonbridge.Supervisor, and hands it to tui.NewApp, whose Init
// calls App.subscribe -> Supervisor.Subscribe (internal/tui/app.go). The load
// is triggered by that first reference, not by an explicit Resume call — see
// wirestream.Reconstructor.session.
//
// # What this benchmark CANNOT see
//
// It is gated on allocations (scripts/bench.sh gates allocs/op and B/op and
// deliberately never gates ns/op), and an allocation-gated benchmark is
// STRUCTURALLY BLIND TO TIME SPENT WAITING. gofer#313's actual bug was a
// 2-second timer: handleSessionLoad waited for a status the session it had just
// resumed could never reach, and burned the full LoadSettleTimeout on every
// cold attach. A blocked timer allocates nothing. Every number below would have
// been IDENTICAL with the bug present and with it fixed — this benchmark could
// never have caught it, and cannot catch its recurrence.
//
// That is not a gap to paper over here: allocation counts are exact and
// portable at -benchtime 1x, which is the entire reason they are what gates.
// Wall-clock on a shared runner is not, which is the entire reason it is not.
// The waiting half is covered by a separate wall-clock regression test; this
// file covers the work-that-scales half, and the two are not substitutes.
//
// Two further blind spots worth stating, since a composed benchmark invites
// being read as "the whole attach":
//
//   - No worker PROCESSES. `--workers` defaults to false (cmd/gofer/daemon.go),
//     so the in-process supervisor IS the default production path; forking real
//     workers would also make allocation counts non-deterministic and defeat the
//     gate. Process-isolated attach is internal/router's fleet harness's job.
//   - No bubbletea. Ingest is driven straight into a tui.Model, as
//     TestGoldenAttachHistoryReplayRendersUserTurn does, so the tea.Cmd/Msg
//     plumbing around App.ingestAttach is not measured.

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/daemonbridge"
	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// composedAttachWait bounds one attach's whole replay drain. It is a FAILURE
// timeout, not a measurement: an attach that does not deliver its history in
// this long has stopped working, and failing beats reporting a short drain as a
// fast one. Generous because the largest sweep point replays 4,000 events
// through a tui.Model whose Ingest is still quadratic in bytes (gofer#308).
const composedAttachWait = 60 * time.Second

// composedEventsPerTurn is how many reconstructed events one seeded turn
// replays as: each turn is a user message and an assistant message, one text
// block apiece, and a history replay emits a MessageStarted/MessageFinished
// PAIR per block with no deltas between them (internal/daemon's historyEvents
// replays each settled message verbatim — see TestAttachReplaysHistory). So
// 2 messages x 2 events = 4.
//
// composedVerifyReplayShape asserts this against a live attach before the
// timed loop runs, so a change to the replay shape fails loudly instead of
// leaving the benchmark quietly measuring a fraction of the work.
const composedEventsPerTurn = 4

// composedStack is one in-process daemon serving one supervisor over a seeded
// store root: the server half of an attach. Built fresh per iteration (under a
// stopped timer) so every measured attach is a COLD one — a supervisor that has
// already resumed the session answers session/load from a live session and
// skips the runner rebuild, which is a different, cheaper path than the one an
// operator's `gofer attach` takes.
type composedStack struct {
	url   string
	close func()
}

// newComposedStack builds the daemon stack over root. It mirrors this package's
// newTestSupervisor/newTestDaemon (bridge_test.go) — same faux provider, same
// stripped Guard/Approver/Tools, same daemon.Config — but takes a testing.B and
// returns an explicit close func instead of registering b.Cleanup: a
// per-iteration stack must be torn down at the END OF ITS ITERATION, not
// accumulated until the benchmark ends.
func newComposedStack(b *testing.B, root string) composedStack {
	b.Helper()
	store, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		b.Fatalf("session.NewFileStore: %v", err)
	}
	build := func(opts runner.Options) runner.Options {
		opts.Store = store
		opts.Model = "faux"
		opts.Guard, opts.Approver, opts.Tools = nil, nil, nil
		opts.Provider = fauxProvider()
		return opts
	}
	sup, err := supervisor.New(supervisor.Config{
		Root:  root,
		Store: store,
		NewSession: func(ctx context.Context, opts runner.Options) (supervisor.Session, error) {
			return runner.New(ctx, build(opts))
		},
		ResumeSession: func(ctx context.Context, id string, opts runner.Options) (supervisor.Session, error) {
			return runner.Resume(ctx, id, build(opts))
		},
	})
	if err != nil {
		b.Fatalf("supervisor.New: %v", err)
	}
	d := daemon.New(sup, daemon.Config{DefaultModel: "faux"})
	srv := httptest.NewServer(d.Handler())
	return composedStack{
		url: "ws" + srv.URL[len("http"):],
		close: func() {
			srv.Close()
			_ = sup.Close()
			_ = store.Close()
		},
	}
}

// composedSeedRoot writes sessions journals of turns exchanges each into a
// fresh temp root and returns the root, the session ids in creation order, and
// the cwd their meta entries carry.
//
// The journals go through the real [session.FileStore], like
// internal/supervisor's roster_bench_test.go, so resume and fold read exactly
// the on-disk format production writes — a hand-rolled format could parse
// faster or slower and quietly measure the wrong thing. The cwd is a REAL
// directory because it round-trips: the client reads it off gofer/overview
// (wirestream's sessionCwd) and sends it back as session/load's required cwd,
// which the daemon resumes the session into.
func composedSeedRoot(b *testing.B, sessions, turns int) (root string, ids []string, cwd string) {
	b.Helper()
	root, cwd = b.TempDir(), b.TempDir()

	store, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		b.Fatalf("session.NewFileStore: %v", err)
	}
	defer func() { _ = store.Close() }()
	ctx := context.Background()

	ids = make([]string, 0, sessions)
	for i := range sessions {
		id := fmt.Sprintf("0192a1b2-0000-7000-8000-%012d", i)
		j, err := store.CreateWithID(ctx, "bench", id)
		if err != nil {
			b.Fatalf("create journal %d: %v", i, err)
		}
		if _, err := j.Append(session.NewMetaEntry(cwd)); err != nil {
			b.Fatalf("append meta %d: %v", i, err)
		}
		for t := range turns {
			user := provider.Message{
				Role:    provider.RoleUser,
				Content: []provider.ContentBlock{{Type: provider.BlockText, Text: fmt.Sprintf("turn %d: wire the websocket ACP listener and get the handshake streaming", t)}},
			}
			if _, err := j.Append(session.NewMessageEntry(user)); err != nil {
				b.Fatalf("append user %d/%d: %v", i, t, err)
			}
			asst := provider.Message{
				Role:    provider.RoleAssistant,
				Content: []provider.ContentBlock{{Type: provider.BlockText, Text: fmt.Sprintf("turn %d: the listener already accepts upgrades; it just never forwards the session events, so I'll wire the fan-out and add a test for the late subscriber", t)}},
			}
			if _, err := j.Append(session.NewMessageEntry(asst)); err != nil {
				b.Fatalf("append assistant %d/%d: %v", i, t, err)
			}
		}
		if err := j.Close(); err != nil {
			b.Fatalf("close journal %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	return root, ids, cwd
}

// composedAttach is the MEASURED body: the whole client-side attach, from
// dialing the daemon to a tui.Model holding the replayed transcript.
//
// It asserts the drain reached wantEvents inside the timed region (a benchmark
// whose subject silently stops replaying reads as a spectacular optimisation),
// and returns the model so the caller can assert it renders — outside the
// timer, since rendering belongs to internal/tui's BenchmarkTranscriptView.
//
// The drain shares ONE deadline timer across every event rather than a
// time.After per event: at the largest sweep point that would be 4,000 timers
// of harness allocation folded into the number being gated.
func composedAttach(b *testing.B, url, sessionID string, wantEvents int) tui.Model {
	b.Helper()
	ctx := context.Background()

	c, err := daemon.Dial(ctx, url, "")
	if err != nil {
		b.Fatalf("daemon.Dial: %v", err)
	}
	bridge := daemonbridge.New(c)
	defer func() { _ = bridge.Close() }()

	// First reference: this is what triggers the session/load whose replay the
	// drain below consumes (wirestream.Reconstructor.session -> loadHistory).
	sub, err := bridge.Subscribe(ctx, sessionID)
	if err != nil {
		b.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	deadline := time.NewTimer(composedAttachWait)
	defer deadline.Stop()

	m := tui.New(theme.Test())
	for i := range wantEvents {
		select {
		case ev, ok := <-sub.C:
			if !ok {
				b.Fatalf("subscription closed after %d/%d replayed events", i, wantEvents)
			}
			m = m.Ingest(ev)
		case <-deadline.C:
			b.Fatalf("timed out after %s waiting for replayed event %d/%d", composedAttachWait, i, wantEvents)
		}
	}
	return m
}

// composedVerifyReplayShape attaches once and asserts the replay is EXACTLY
// composedEventsPerTurn events per turn — no fewer (the drain would then hang
// and the benchmark would fail as a timeout, which is at least loud) and no
// MORE, which is the silent failure: a replay that grew a fifth event per turn
// would leave every measurement below quietly under-counting the work, and the
// resulting drop would read as an optimisation.
//
// It runs against its own stack, before the timed loop, so the warm-up attach
// it performs never leaves the session live for a measured iteration.
func composedVerifyReplayShape(b *testing.B, root, sessionID string, turns int) {
	b.Helper()
	stack := newComposedStack(b, root)
	defer stack.close()

	ctx := context.Background()
	c, err := daemon.Dial(ctx, stack.url, "")
	if err != nil {
		b.Fatalf("daemon.Dial: %v", err)
	}
	bridge := daemonbridge.New(c)
	defer func() { _ = bridge.Close() }()

	sub, err := bridge.Subscribe(ctx, sessionID)
	if err != nil {
		b.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	// Two halves, because the two failure directions fail differently.
	//
	// TOO FEW is caught by draining the expected count against the same failure
	// timeout the measured drain uses: a short replay simply never arrives and
	// this fails as a timeout naming the event it stopped at.
	want := turns * composedEventsPerTurn
	deadline := time.NewTimer(composedAttachWait)
	defer deadline.Stop()
	for i := range want {
		select {
		case _, ok := <-sub.C:
			if !ok {
				b.Fatalf("subscription closed after %d/%d replayed events — the replay shape changed", i, want)
			}
		case <-deadline.C:
			b.Fatalf("timed out after %s at replayed event %d/%d — the replay delivers FEWER than %d events per turn, "+
				"so every measurement in this file would be counting the wrong amount of work",
				composedAttachWait, i, want, composedEventsPerTurn)
		}
	}

	// TOO MANY is the silent direction, and needs a positive observation: a
	// replay that grew a fifth event per turn would leave the measured drain
	// stopping early, under-counting the work, and the drop would read as an
	// optimisation. An extra event is produced by the same burst as the ones
	// already drained, so it is either buffered on the subscription already or
	// arrives right behind them — a short wait is enough to see it, and (unlike
	// a quiet-period drain) a mid-burst stall on a loaded runner cannot be
	// mistaken for the end of the replay.
	const extraWait = time.Second
	select {
	case ev, ok := <-sub.C:
		if ok {
			b.Fatalf("history replay delivered MORE than %d events for %d turns (extra: %s) — "+
				"the replay shape changed, so every measurement in this file is counting the wrong amount of work",
				want, turns, ev.Kind())
		}
	case <-time.After(extraWait):
	}
}

// BenchmarkComposedAttach sweeps TRANSCRIPT LENGTH — the axis an operator grows
// by using one session — with a single session in the store.
//
// It includes turns=1 deliberately. The small end is where the FIXED overhead
// of an attach lives (two roster RPCs, a resume, a settle wait, a websocket
// dial), and fixed overhead is invisible at turns=1000 where the per-turn work
// swamps it. gofer#313 was pure fixed overhead: it cost the same 2s on a
// one-turn session as on a thousand-turn one. A sweep that started at 100 would
// have described that session as "fast per turn" and been useless.
func BenchmarkComposedAttach(b *testing.B) {
	for _, turns := range []int{1, 10, 100, 1000} {
		b.Run(fmt.Sprintf("turns=%d", turns), func(b *testing.B) {
			root, ids, _ := composedSeedRoot(b, 1, turns)
			composedVerifyReplayShape(b, root, ids[0], turns)
			wantEvents := turns * composedEventsPerTurn

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				b.StopTimer()
				stack := newComposedStack(b, root)
				b.StartTimer()

				m := composedAttach(b, stack.url, ids[0], wantEvents)

				b.StopTimer()
				if got := m.View(testkit.Width, testkit.Height); got == "" {
					b.Fatal("attached transcript rendered empty")
				}
				stack.close()
				b.StartTimer()
			}
		})
	}
}

// BenchmarkComposedAttachStoreSize sweeps the OTHER axis of the same path: how
// many sessions the store holds, at a fixed short transcript.
//
// Swept separately, per CONTRIBUTING's rule, because the two axes are paid for
// by different code and a fix that flattens one leaves the other untouched. A
// cold attach reads the whole store before it reads the session: wirestream's
// sessionCwd calls gofer/roster AND gofer/overview to resolve session/load's
// required cwd, and gofer/overview is the roster read that opens and parses
// every non-archived journal (the gofer#298 path). So attaching to a one-turn
// session gets more expensive as the OPERATOR'S OTHER SESSIONS accumulate —
// a cost no benchmark of the session being attached to can express.
func BenchmarkComposedAttachStoreSize(b *testing.B) {
	// 8 turns: long enough to be a real transcript, short enough that the
	// per-transcript work does not mask the store-wide read this isolates.
	const turns = 8
	for _, sessions := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("sessions=%d", sessions), func(b *testing.B) {
			root, ids, _ := composedSeedRoot(b, sessions, turns)
			composedVerifyReplayShape(b, root, ids[0], turns)
			wantEvents := turns * composedEventsPerTurn

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				b.StopTimer()
				stack := newComposedStack(b, root)
				b.StartTimer()

				m := composedAttach(b, stack.url, ids[0], wantEvents)

				b.StopTimer()
				if got := m.View(testkit.Width, testkit.Height); got == "" {
					b.Fatal("attached transcript rendered empty")
				}
				stack.close()
				b.StartTimer()
			}
		})
	}
}
