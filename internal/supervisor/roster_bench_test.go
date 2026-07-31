package supervisor_test

// roster_bench_test.go measures the roster read the TUI performs on every
// refresh tick.
//
// This is the hot path behind gofer#298: OverviewRoster -> List ->
// diskSessionInfo -> session.ReadEntries opens and parses EVERY non-live
// session's ENTIRE journal, every time. Cost therefore scales on two axes at
// once — how many sessions exist, and how long each conversation is — and the
// operator feels it as a roster that gets slower the longer they use gofer,
// which is exactly the report that led here ("archive/delete is a tad
// sluggish").
//
// The benchmarks vary those two axes independently so the profile is
// attributable rather than a single number: BenchmarkOverviewRoster fixes the
// journal length and grows the session count, BenchmarkOverviewRosterJournalDepth
// fixes the count and grows the length. A fix that caches or reads sidecars
// instead of full journals should flatten BOTH, and the pair is what shows
// which one it actually flattened.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// benchRoot writes sessions journals, each with turns exchanges, into a
// temporary root laid out the way [supervisor.Supervisor.List] walks it
// (<root>/sessions/<slug>/<id>.jsonl), and returns a Supervisor over it.
//
// The journals are written through the real [session.FileStore] rather than
// hand-rolled JSON so the benchmark reads exactly the on-disk format production
// produces — a synthetic format could parse faster (or slower) than the real
// one and quietly measure the wrong thing.
func benchRoot(b *testing.B, sessions, turns int) *supervisor.Supervisor {
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

	sup, err := supervisor.New(supervisor.Config{
		Root:  root,
		Clock: func() time.Time { return time.Unix(0, 0) },
	})
	if err != nil {
		b.Fatalf("new supervisor: %v", err)
	}
	b.Cleanup(func() { _ = sup.Close() })
	return sup
}

// BenchmarkOverviewRoster grows the SESSION COUNT at a fixed, modest journal
// length. 30/100/500 spans "a normal week", "a heavy user", and "someone who
// never archives" — the last being the case that actually hurts, since nothing
// prunes the roster automatically.
func BenchmarkOverviewRoster(b *testing.B) {
	for _, sessions := range []int{30, 100, 500} {
		b.Run(fmt.Sprintf("sessions=%d", sessions), func(b *testing.B) {
			sup := benchRoot(b, sessions, 8)
			ctx := context.Background()

			// Warm the page cache so the measurement is the parse cost, not the
			// first-read IO. The TUI refreshes on a tick against files it has
			// already read many times, so a cold-cache number would describe a
			// situation the operator basically never sees.
			if _, err := sup.OverviewRoster(ctx); err != nil {
				b.Fatalf("warm: %v", err)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				got, err := sup.OverviewRoster(ctx)
				if err != nil {
					b.Fatalf("OverviewRoster: %v", err)
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

// BenchmarkOverviewRosterJournalDepth grows the JOURNAL LENGTH at a fixed
// session count, isolating the second axis: a roster read costs more as
// conversations get longer, even with no new sessions. This is the axis a
// naive "cache the session list" fix would leave untouched, so measuring it
// separately is what keeps a partial fix from looking complete.
func BenchmarkOverviewRosterJournalDepth(b *testing.B) {
	for _, turns := range []int{8, 64, 256} {
		b.Run(fmt.Sprintf("turns=%d", turns), func(b *testing.B) {
			sup := benchRoot(b, 30, turns)
			ctx := context.Background()
			if _, err := sup.OverviewRoster(ctx); err != nil {
				b.Fatalf("warm: %v", err)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				got, err := sup.OverviewRoster(ctx)
				if err != nil {
					b.Fatalf("OverviewRoster: %v", err)
				}
				if len(got) != 30 {
					b.Fatalf("roster = %d sessions, want 30", len(got))
				}
			}
		})
	}
}
