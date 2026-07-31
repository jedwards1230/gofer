package supervisor_test

// roster_live_bench_test.go covers the HALF of the roster read that
// roster_bench_test.go does not reach: the live-session branch.
//
// The gap it closes (gofer#315, item 1). The benchmarks next door build their
// fixture with session.FileStore.CreateWithID + supervisor.New and nothing
// else. That writes journals to disk but registers NOTHING live, so
// Supervisor.liveByID() returns an empty map and the `if info, ok := live[id];
// ok` branch in List is never taken — all 500 rows go down diskSessionInfo.
// The benchmark that reads like "the roster at 500 sessions" was measuring
// exactly the half that cannot contain a live session's cost.
//
// What lives only on that branch is managed.info(), and specifically its two
// SDK calls — Session.Cost() and Session.LastUsage(). Each one takes the
// journal lock and does `make([]Entry, len(j.entries))` + copy before walking
// (agent-sdk-go session/journal.go), so ONE roster row costs TWO full copies of
// that session's journal. On the TUI's 1 Hz poll, and again from watch.go's
// notify() on every prompt enqueue, turn start, turn end, and idle transition.
//
// So the axis is live sessions x journal length, and the two benchmarks below
// sweep them separately for the reason the file next door states: a fix that
// flattens one and not the other looks complete against a combined number.
//
// WHY A REAL RUNNER. The whole cost under measurement is inside the SDK's
// Journal. A fake Session — which is what every other test in this package uses
// — would return a canned Cost/LastUsage and measure nothing, which is the same
// class of mistake this file exists to correct. So these resume REAL
// *runner.Runner sessions over the journals just written to disk, with a faux
// provider injected so no credential and no network are involved. Resume rather
// than Create because resume is what loads an existing journal's entries into
// memory, which is what makes the copies non-trivial.
//
// The live counts stay modest (1/8/32) on purpose: every live session is a real
// runner with its own pump goroutine, and an operator with 32 agents running at
// once is already an extreme. The journal-DEPTH sweep is where the per-row cost
// actually grows, and that one goes to 256 turns.

import (
	"context"
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/provider/faux"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// benchLiveRoot writes `sessions` journals of `turns` exchanges each, then
// RESUMES every one of them into the returned Supervisor, so the roster read
// takes the live branch for every row.
//
// Assistant entries carry a model and a usage tally (unlike the disk-only
// fixture next door), because that is what a real journal looks like and
// because LastUsage walks the folded chain looking for exactly that field — a
// journal with no usage anywhere would still pay for the copy but would walk
// the whole chain to find nothing, which is not the shape production has.
func benchLiveRoot(b *testing.B, sessions, turns int) *supervisor.Supervisor {
	b.Helper()
	root := b.TempDir()
	cwd := b.TempDir()

	store, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		b.Fatalf("new store: %v", err)
	}
	b.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()

	ids := make([]string, 0, sessions)
	for i := range sessions {
		id := fmt.Sprintf("0192a1b2-0000-7000-8000-%012d", i)
		j, err := store.CreateWithID(ctx, session.Slugify(cwd), id)
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
			if _, err := j.Append(session.NewMessageEntry(asst,
				session.WithEntryModel("faux"),
				session.WithEntryUsage(provider.Usage{InputTokens: 1200, OutputTokens: 300}),
			)); err != nil {
				b.Fatalf("append assistant %d/%d: %v", i, t, err)
			}
		}
		if err := j.Close(); err != nil {
			b.Fatalf("close journal %d: %v", i, err)
		}
		ids = append(ids, id)
	}

	// A script long enough that a runner never exhausts it. Nothing below sends
	// a prompt, so no turn is ever consumed — this is only here because faux
	// requires a script to construct.
	prov := faux.New(faux.Default())

	sup, err := supervisor.New(supervisor.Config{
		Root:  root,
		Store: store,
		Clock: func() time.Time { return time.Unix(0, 0) },
		ResumeSession: func(ctx context.Context, id string, opts runner.Options) (supervisor.Session, error) {
			opts.Store = store
			opts.Provider = prov
			return runner.Resume(ctx, id, opts)
		},
	})
	if err != nil {
		b.Fatalf("new supervisor: %v", err)
	}
	b.Cleanup(func() { _ = sup.Close() })

	for _, id := range ids {
		if _, err := sup.Resume(ctx, id, supervisor.ResumeOptions{Cwd: cwd, Model: "faux"}); err != nil {
			b.Fatalf("resume %s: %v", id, err)
		}
	}
	return sup
}

// liveCallsPerOp is how many roster reads ONE benchmark iteration makes, in
// every benchmark in this file. It is a signal-to-noise measure, needed
// because of how testing counts allocations.
//
// testing.B derives allocs/op and B/op from runtime.ReadMemStats, whose
// Mallocs counter is PROCESS-WIDE. Every allocation made by every goroutine
// inside the timed region is therefore charged to the benchmark. That is not a
// theory: parking one goroutine that allocates in a loop alongside this
// benchmark moves it from a rock-steady 77 allocs/op to 821-1,563, and B/op
// from 69,352 to 134,736.
//
// These fixtures hold LIVE sessions, and a live session is three goroutines
// (pump, permission watch, decision watch) plus whatever the SDK runner
// starts — two dozen or more at eight sessions, none of which the benchmark
// controls. Unbatched, one roster read allocated 77-134 times in tens of
// microseconds, so the 25% gate tolerance left a margin of only ~19-33 stray
// allocations. That is too thin for a fixture like this, and CI proved it: a
// +39 stray failed LiveSnapshotJournalDepth/turns=8 (77 -> 116), and the same
// +39 would have failed the 134-alloc rows too (134 -> 173 is +29%).
//
// WHY 40 AND NOT 20. This constant has to be re-derived whenever the measured
// cost changes, because it is sized against the SIGNAL while the noise stays
// put — the stray allocations come from neighbouring goroutines and have
// nothing to do with the journal walk. agent-sdk-go v0.22.1 cut the per-read
// cost (77 -> 45 allocations here, 134 -> 102 next door), so the signal shrank
// and the margin shrank with it. Measured against the +39 stray CI actually
// produced, at a 25% tolerance:
//
//	row                          per read   x20 margin   x40 margin
//	OverviewRosterLive/live=1          33         4.2x         8.5x
//	LiveSnapshotJournalDepth/*         45         5.8x        11.5x
//	OverviewRosterLive/live=8         102        13.1x        26.2x
//
// A batched count of 624 is where the weakest row would drop to a 4x margin.
// At 20x it sits at 660 — above the line, but only just. 40x puts every row at
// 2x that, for a benchmark loop that is a few milliseconds longer.
//
// THE RULE, so the next person does not have to re-derive it: if the per-read
// allocation count of the weakest row falls below ~31, 20x stops giving a 4x
// margin. Re-measure and raise this constant rather than discovering it on CI.
//
// DIVIDING BACK. Every reported figure divides by this constant to the
// per-read cost. Eight of the nine committed rows do so exactly — for example
// LiveSnapshotJournalDepth at every depth is 1,800/40 = 45 allocations and
// 677,440/40 = 16,936 B. The ninth, OverviewRosterLive/live=32, comes back to
// 302.05 allocations and 129,485.2 B against a measured 302 / 129,480: a residual
// of +2 allocations and +208 B spread across 40 reads.
//
// That residual used to appear on four rows rather than one, and it shrank for
// the same reason everything else here got cheaper: v0.22.1 made each read
// faster, so the timed window is shorter and catches less of what the
// neighbouring goroutines do. The remaining case is the row with the most live
// sessions, hence the longest window — which is exactly the direction a
// process-wide counter predicts, and is worth more as corroboration than it
// costs as imprecision.
const liveCallsPerOp = 40

// assertAllLive fails the benchmark unless every row in the roster is a LIVE
// row. It is the guard the benchmarks next door needed and did not have: their
// fixture silently produced zero live rows, so the branch under test never ran
// and the numbers looked fine. A row count alone cannot catch that — a
// disk-only row and a live row are both one entry in the slice.
func assertAllLive(b *testing.B, sup *supervisor.Supervisor, want int) {
	b.Helper()
	got, err := sup.OverviewRoster(context.Background())
	if err != nil {
		b.Fatalf("OverviewRoster: %v", err)
	}
	if len(got) != want {
		b.Fatalf("roster = %d sessions, want %d", len(got), want)
	}
	for _, s := range got {
		if !s.Live {
			b.Fatalf("session %s is not Live — this benchmark is measuring the disk branch, which roster_bench_test.go already covers", s.ID)
		}
	}
}

// BenchmarkOverviewRosterLive grows the LIVE session count. Every row costs a
// managed.info(), so this is the axis that multiplies the per-row journal
// copies by the number of agents an operator is running at once.
func BenchmarkOverviewRosterLive(b *testing.B) {
	for _, sessions := range []int{1, 8, 32} {
		b.Run(fmt.Sprintf("live=%d", sessions), func(b *testing.B) {
			sup := benchLiveRoot(b, sessions, 8)
			ctx := context.Background()
			assertAllLive(b, sup, sessions) // also warms the measured call

			quiesce()

			b.ReportAllocs()
			for b.Loop() {
				for range liveCallsPerOp {
					got, err := sup.OverviewRoster(ctx)
					if err != nil {
						b.Fatalf("OverviewRoster: %v", err)
					}
					if len(got) != sessions {
						b.Fatalf("roster = %d sessions, want %d", len(got), sessions)
					}
				}
			}
		})
	}
}

// BenchmarkOverviewRosterLiveJournalDepth grows the JOURNAL LENGTH at a fixed
// live count. This is the sweep that exposes managed.info()'s two full-slice
// journal copies per row: the disk-side twin next door is FLAT on this axis
// (793 allocs at 8, 64 and 256 turns alike, since gofer#298's sidecar cache),
// so anything that grows here is coming from the live branch.
//
// The cost this measures is in the SDK, not in gofer: Journal.Cost and
// Journal.LastUsage each copy the entry slice before walking it. gofer cannot
// fix that without changing agent-sdk-go (CLAUDE.md invariant 1), so this
// benchmark's job is to make the shape of the growth visible and to fail if it
// ever gets worse.
func BenchmarkOverviewRosterLiveJournalDepth(b *testing.B) {
	for _, turns := range []int{8, 64, 256} {
		b.Run(fmt.Sprintf("turns=%d", turns), func(b *testing.B) {
			const sessions = 8
			sup := benchLiveRoot(b, sessions, turns)
			ctx := context.Background()
			assertAllLive(b, sup, sessions) // also warms the measured call

			quiesce()

			b.ReportAllocs()
			for b.Loop() {
				for range liveCallsPerOp {
					got, err := sup.OverviewRoster(ctx)
					if err != nil {
						b.Fatalf("OverviewRoster: %v", err)
					}
					if len(got) != sessions {
						b.Fatalf("roster = %d sessions, want %d", len(got), sessions)
					}
				}
			}
		})
	}
}

// BenchmarkLiveSnapshotJournalDepth is [BenchmarkOverviewRosterLiveJournalDepth]
// with the disk walk removed: Supervisor.Roster is snapshotLive and nothing
// else, so every byte it reports comes from managed.info().
//
// It is here for two reasons. It is a real path in its own right — Roster backs
// the WatchRoster fan-out the daemon pushes to every attached client, so it runs
// on each roster change, not on a timer. And it is what makes the attribution in
// the sweep above checkable rather than asserted: subtract this from the
// OverviewRoster number at the same depth and what remains is the disk side,
// which the benchmarks next door already show is flat on this axis.
// Its reported figures are per [liveCallsPerOp] calls, not per call — see
// that constant.
func BenchmarkLiveSnapshotJournalDepth(b *testing.B) {
	for _, turns := range []int{8, 64, 256} {
		b.Run(fmt.Sprintf("turns=%d", turns), func(b *testing.B) {
			const sessions = 8
			sup := benchLiveRoot(b, sessions, turns)
			ctx := context.Background()
			assertAllLive(b, sup, sessions)

			// Warm the function THIS benchmark measures, which assertAllLive
			// does not: it calls OverviewRoster, and Roster is a different
			// entry point (snapshotLive, no disk walk). Nothing else in this
			// package benchmarks Roster, so without this line the very first
			// Roster call in the whole process happens INSIDE the timed
			// region, and every first-call cost on that path — lazily built
			// runtime metadata, a cold branch, a first map growth — is charged
			// to turns=8 alone. That is what CI saw: 116 allocs on turns=8
			// while turns=64 and turns=256 reproduced byte-for-byte.
			if got, err := sup.Roster(ctx); err != nil {
				b.Fatalf("warm Roster: %v", err)
			} else if len(got) != sessions {
				b.Fatalf("warm Roster = %d sessions, want %d", len(got), sessions)
			}

			quiesce()

			// No b.ResetTimer(): b.Loop() performs one itself on its first
			// call (testing.B.loopSlowPath), so the window opens here and
			// everything above — including the two GCs — is already excluded.
			b.ReportAllocs()
			for b.Loop() {
				for range liveCallsPerOp {
					got, err := sup.Roster(ctx)
					if err != nil {
						b.Fatalf("Roster: %v", err)
					}
					if len(got) != sessions {
						b.Fatalf("roster = %d sessions, want %d", len(got), sessions)
					}
				}
			}
		})
	}
}

// quiesce retires the PREVIOUS sub-benchmark's teardown before the next one
// starts measuring. b.Cleanup closes eight live sessions and their journals
// when a sub-benchmark ends, so the one that follows would otherwise open its
// window on a heap full of that garbage and its pending finalizers — and,
// because the allocation counters are process-wide, be charged for collecting
// it. Two cycles: the first queues finalizers, the second runs them.
//
// It is called BEFORE b.Loop(), which performs its own b.ResetTimer() on the
// first call (testing.B.loopSlowPath), so none of this lands in the measurement.
func quiesce() {
	runtime.GC()
	runtime.GC()
}
