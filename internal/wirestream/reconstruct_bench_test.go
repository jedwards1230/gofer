package wirestream

// reconstruct_bench_test.go measures the per-frame decode cost of the ONLY path
// a worker's events take to a client under --workers.
//
// It exists because that path had no benchmark at all while carrying every event
// of every remote session: its cost is paid per message delta and per tool-call
// delta, so it scales with transcript length and turn count at once — exactly
// the growth axis CONTRIBUTING.md says to measure rather than assume. The three
// kinds swept are the ones that actually dominate a live stream (a delta-heavy
// assistant message, a tool call's terminal frame with its Diagnostics/Spill
// payload, and a lifecycle frame), not one representative number.

import (
	"encoding/json"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"
)

// BenchmarkHandleGoferEvent sweeps handleGoferEvent over the kinds that make up
// the bulk of a real stream. The reconstructor has no subscribers and no sink, so
// what is measured is the decode-and-publish itself rather than fan-out.
func BenchmarkHandleGoferEvent(b *testing.B) {
	const sid = "sess-bench"

	frames := []struct {
		name string
		ev   event.Event
	}{
		{"message_delta", event.NewMessageDelta(sid, event.MessageText, "the quick brown fox jumps over the lazy dog")},
		{"tool_call_finished", func() event.Event {
			e := event.NewToolCallFinishedSpill(sid, "call-1", json.RawMessage(`{"path":"main.go"}`),
				"ok", false, []string{"lint: unused variable x"}, "/tmp/spill/call-1.txt", 41231,
				"9f2c1e5b7a3d4f6089c2e1b5a7d3f46089c2e1b5a7d3f46089c2e1b5a7d3f460")
			e.Agent = "reviewer"
			return e
		}()},
		{"turn_finished", event.NewTurnFinishedCost(sid, "end_turn",
			provider.Usage{InputTokens: 1200, OutputTokens: 340}, nil)},
	}

	for _, f := range frames {
		raw, err := json.Marshal(f.ev)
		if err != nil {
			b.Fatalf("marshal %s: %v", f.name, err)
		}
		b.Run(f.name, func(b *testing.B) {
			r := newSinkTestReconstructor(nil)
			r.RegisterFresh(sid)
			// One warm-up OUTSIDE the measurement. scripts/bench.sh runs the
			// suite at -benchtime 1x, so without this the first sub-benchmark
			// measures one decode plus every lazily-initialized thing it touches
			// on first use (encoding/json's per-type cache, the broker's
			// machinery) — which read as ~600 allocs/op against a real cost of
			// ~11, and would bake that into the baseline as this path's price.
			r.handleGoferEvent(raw)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				r.handleGoferEvent(raw)
			}
		})
	}
}
