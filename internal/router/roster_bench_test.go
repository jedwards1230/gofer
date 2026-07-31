package router

// roster_bench_test.go measures the roster read on the PRIMARY deployment path.
//
// internal/supervisor has had BenchmarkOverviewRoster since gofer#309, but the
// router — which under M6 is the daemon a TUI or `gofer ps` actually talks to —
// had no perf coverage at all. That gap is not incidental: it is exactly how the
// first cut of the gofer#298 fix came to land only in the in-process supervisor.
// Every supervisor benchmark improved by two orders of magnitude while the
// router still parsed every journal on every fetch, and nothing measured the
// difference. A benchmark on the fallback path but not the primary one reads as
// coverage while proving nothing about what users run.
//
// The two axes are swept independently for the same reason they are in
// supervisor's copy: a fix that flattens session count but not journal depth
// looks complete against a combined number.

import (
	"context"
	"fmt"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/session"
)

// benchRouterRoot writes sessions journals, each with turns exchanges, into a
// temporary root laid out the way the router walks it
// (<root>/sessions/<slug>/<id>.jsonl), and returns a router over it.
//
// Journals go through the real [session.FileStore] rather than hand-rolled JSON
// so the benchmark reads exactly the on-disk format production writes — a
// synthetic format could parse faster or slower and quietly measure the wrong
// thing. No workers are started: every row is an offline row, which is the case
// the roster read is dominated by and the one gofer#298 is about.
func benchRouterRoot(b *testing.B, sessions, turns int) *Supervisor {
	b.Helper()
	root := b.TempDir()

	store, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		b.Fatalf("new store: %v", err)
	}
	ctx := context.Background()

	for i := range sessions {
		j, err := store.CreateWithID(ctx, "bench", fmt.Sprintf("0192a1b2-0000-7000-8000-%012d", i))
		if err != nil {
			b.Fatalf("create journal %d: %v", i, err)
		}
		if _, err := j.Append(session.NewMetaEntry("/home/bench/project")); err != nil {
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
	}
	if err := store.Close(); err != nil {
		b.Fatalf("close store: %v", err)
	}

	sup, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		b.Fatalf("router.New: %v", err)
	}
	b.Cleanup(func() { _ = sup.Close() })
	return sup
}

// BenchmarkRouterList grows the SESSION COUNT at a fixed, modest journal length,
// mirroring BenchmarkOverviewRoster's 30/100/500 so the two paths are directly
// comparable — the point being that they should no longer differ.
func BenchmarkRouterList(b *testing.B) {
	for _, sessions := range []int{30, 100, 500} {
		b.Run(fmt.Sprintf("sessions=%d", sessions), func(b *testing.B) {
			sup := benchRouterRoot(b, sessions, 8)
			ctx := context.Background()

			// Warm the page cache AND the sidecar cache: the TUI refreshes on a
			// tick against journals it has already read many times, so a cold
			// number would describe a situation the operator basically never
			// sees — the first fetch after a daemon start, once.
			if _, err := sup.List(ctx); err != nil {
				b.Fatalf("warm: %v", err)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				got, err := sup.List(ctx)
				if err != nil {
					b.Fatalf("List: %v", err)
				}
				// Guard against a future short-circuit making this benchmark
				// measure an early return instead of the roster read. A
				// benchmark that stops exercising its subject reads as a
				// spectacular optimization.
				if len(got) != sessions {
					b.Fatalf("roster = %d sessions, want %d", len(got), sessions)
				}
			}
		})
	}
}

// BenchmarkRouterListJournalDepth grows the JOURNAL LENGTH at a fixed session
// count. This is the axis that must go FLAT: with the derivation cached in the
// sidecar, a roster fetch stops caring how long the conversations are.
func BenchmarkRouterListJournalDepth(b *testing.B) {
	for _, turns := range []int{8, 64, 256} {
		b.Run(fmt.Sprintf("turns=%d", turns), func(b *testing.B) {
			sup := benchRouterRoot(b, 30, turns)
			ctx := context.Background()
			if _, err := sup.List(ctx); err != nil {
				b.Fatalf("warm: %v", err)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				got, err := sup.List(ctx)
				if err != nil {
					b.Fatalf("List: %v", err)
				}
				if len(got) != 30 {
					b.Fatalf("roster = %d sessions, want 30", len(got))
				}
			}
		})
	}
}
