package tui_test

// transcript_bench_test.go measures the two costs an operator feels in a LONG
// session, as distinct from a long roster (internal/supervisor's
// roster_bench_test.go covers that):
//
//   - Ingest: replaying a session's event stream, which is what attaching to an
//     existing session does before the first frame appears. This is open-latency
//     — the pause between pressing enter on a session and seeing it.
//   - View: rendering the transcript, which happens on EVERY frame, so its cost
//     sets the ceiling on how smooth scrolling can be.
//
// Both sweep transcript size rather than reporting one number, because the
// question is never "how fast is it" but "what happens as my session grows" —
// a per-event cost that is constant is fine at any size, and one that is
// super-linear is a wall the operator will eventually hit.
//
// [BenchmarkAppRenderMassiveTranscript] (app_internal_test.go) is the
// complementary whole-App render at one deliberately absurd size; these are the
// scaling curves underneath it.
//
// Both ingest [tui.GoldenBenchTurns] — prose, a fenced code block, a tool call
// and a multi-line tool result per turn. They used to ingest one line of plain
// text per message, which made every absolute number here about 8x optimistic
// and left a markdown/code-block/tool-result regression barely visible in the
// gated figure (gofer#315, item 3). See GoldenBenchTurn's doc for the full
// rationale.

import (
	"fmt"
	"testing"

	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// BenchmarkTranscriptIngest measures replaying a whole session's stream — the
// work done between opening a session and seeing its first frame.
//
// Reported per REPLAY, not per event, because that is the latency an operator
// experiences: they wait for the whole journal, not for one entry.
func BenchmarkTranscriptIngest(b *testing.B) {
	for _, turns := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("turns=%d", turns), func(b *testing.B) {
			evs := tui.GoldenBenchTurns(turns)

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				m := tui.New(theme.Test())
				for _, ev := range evs {
					m.Ingest(ev)
				}
				// Keep the fully-replayed model observable so the loop cannot be
				// optimized away, and assert it actually absorbed the stream — a
				// benchmark whose subject silently stopped ingesting would look
				// like a huge win.
				if got := m.View(testkit.Width, testkit.Height); got == "" {
					b.Fatal("replayed transcript rendered empty")
				}
			}
		})
	}
}

// BenchmarkTranscriptView measures ONE frame of an already-replayed transcript.
// This is the per-frame cost every scroll notch and every re-render pays, so it
// is the number that decides whether a long session scrolls smoothly.
//
// The model is built ONCE outside the timer: replay cost belongs to
// [BenchmarkTranscriptIngest], and folding it in here would hide the render
// behind it at large sizes.
func BenchmarkTranscriptView(b *testing.B) {
	for _, turns := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("turns=%d", turns), func(b *testing.B) {
			m := tui.New(theme.Test())
			for _, ev := range tui.GoldenBenchTurns(turns) {
				m.Ingest(ev)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if got := m.View(testkit.Width, testkit.Height); got == "" {
					b.Fatal("transcript rendered empty")
				}
			}
		})
	}
}
