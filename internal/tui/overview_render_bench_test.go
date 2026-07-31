package tui

// overview_render_bench_test.go measures rendering the roster SCREEN, which
// nothing measured before (gofer#315, item 4).
//
// The gap this closes is a naming coincidence that reads as coverage.
// internal/supervisor's BenchmarkOverviewRoster and internal/router's
// BenchmarkRouterList both look like "the roster at 500 sessions" and both
// measure the SUPERVISOR side — walking the store, reading sidecars, building
// SessionInfo. Neither renders a single row. The one benchmark that renders a
// whole App frame, BenchmarkAppRenderMassiveTranscript, renders the ATTACH
// screen against a two-row golden roster. So the roster's own render cost
// appeared in no benchmark at all.
//
// It is worth measuring because it is O(TOTAL sessions), not O(visible ones):
// Overview.body calls o.rows(width), which renders EVERY roster row into a
// styled string, and only then windows the result down to the ~35 rows that
// fit. Nothing prunes a roster automatically, so the total is whatever the
// operator has accumulated — and this runs on every arrow key and on the 1 Hz
// poll.
//
// This benchmark deliberately does NOT assert the render is windowed. Windowing
// it before the styling would be a real change to overview_render.go with its
// own risk of shifting what is on screen; this file exists so that change has a
// number to move.

import (
	"fmt"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// benchRoster builds n roster rows spread over several cwds, since the flat
// view groups by cwd and a single-group roster would skip the grouping and the
// per-group headers entirely — measuring a shape no real roster has.
func benchRoster(n int) []SessionInfo {
	statuses := []SessionStatus{StatusWorking, StatusNeedsInput, StatusIdle, StatusFinished}
	cwds := []string{"~/orchestration", "~/src/gofer", "~/src/agent-sdk-go", "~/notes"}
	out := make([]SessionInfo, n)
	for i := range out {
		out[i] = SessionInfo{
			ID:      fmt.Sprintf("0192a1b2-bench-7000-8000-%012d", i),
			Title:   fmt.Sprintf("wire the %02d listener fan-out and cover the late subscriber", i),
			Summary: fmt.Sprintf("turn %d — the upgrade lands but no events forward", i),
			Status:  statuses[i%len(statuses)],
			Cwd:     cwds[i%len(cwds)],
			Updated: GoldenNow.Add(-time.Duration(i) * time.Minute),
			Created: GoldenNow.Add(-time.Duration(i+30) * time.Minute),
		}
	}
	return out
}

// BenchmarkOverviewRender renders ONE overview frame, sweeping the roster size
// over the same 30/100/500 the supervisor-side roster benchmarks use, so the
// two halves of "the roster at N sessions" are finally comparable.
func BenchmarkOverviewRender(b *testing.B) {
	for _, sessions := range []int{30, 100, 500} {
		b.Run(fmt.Sprintf("sessions=%d", sessions), func(b *testing.B) {
			a := NewApp(theme.Test(), newInternalFakeSup(nil), GoldenMeta(), GoldenCommandEnv())
			mdl, _ := a.Update(tea.WindowSizeMsg{Width: testkit.Width, Height: testkit.Height})
			a = mdl.(App)
			mdl, _ = a.Update(rosterMsg{sessions: benchRoster(sessions)})
			a = mdl.(App)
			if a.scr != screenOverview {
				b.Fatalf("screen = %v, want the overview — this benchmark would be measuring a different frame", a.scr)
			}
			if len(a.over.sessions) != sessions {
				b.Fatalf("overview holds %d sessions, want %d", len(a.over.sessions), sessions)
			}

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if a.render() == "" {
					b.Fatal("rendered an empty overview frame")
				}
			}
		})
	}
}
