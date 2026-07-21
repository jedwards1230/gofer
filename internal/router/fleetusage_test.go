package router

// fleetusage_test.go pins [Supervisor.FleetUsage]: the fleet-wide Cost/Usage
// total is the sum of the live sessions' cached rows, it grows as a session's
// cost grows, and it stays a plain sum (no double-count) — all off the roster
// cache with zero worker RPCs.
//
// It reuses rostercache_test.go's countingWorker harness: a fake worker answers
// the adoption handshake and one seed, then a test drives its cached row by
// pushing turn.finished events carrying cost/usage deltas, exactly as a real
// worker does.

import (
	"context"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// adoptCountingWorkers stands up ONE router that adopts every worker in ws (their
// endpoint files were written by startRosterCacheWorker before New scans), and
// returns the router plus each worker's handle, seeded.
func adoptCountingWorkers(t *testing.T, root string, ws ...*countingWorker) (*Supervisor, []*workerHandle) {
	t.Helper()
	sup, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	handles := make([]*workerHandle, 0, len(ws))
	for _, w := range ws {
		h, ok := sup.get(w.id)
		if !ok {
			t.Fatalf("router did not adopt counting worker %s", w.id)
		}
		awaitSeed(t, h)
		handles = append(handles, h)
	}
	return sup, handles
}

// pushTurnCost drives one turn.finished carrying a usd cost and an input-token
// usage onto w's cached row.
func pushTurnCost(t *testing.T, w *countingWorker, usd float64, inputTokens int) {
	t.Helper()
	w.pushEvent(t, map[string]any{
		"type": string(event.KindTurnFinished), "session_id": w.id, "stop_reason": "end_turn",
		"usage": map[string]any{"input_tokens": inputTokens},
		"cost":  map[string]any{"usd": usd},
	})
}

// TestFleetUsageSumsLiveSessions is the headline claim: the fleet total equals
// the sum of the per-session costs across two live sessions, and its usage is
// the sum of theirs. The router computes it off the roster cache, so it is
// exactly what a client would see summing the roster — but authoritative and
// zero-RPC.
func TestFleetUsageSumsLiveSessions(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()
	w1 := startRosterCacheWorker(t, "0.3.0")
	w2 := startRosterCacheWorker(t, "0.3.0")
	sup, handles := adoptCountingWorkers(t, root, w1, w2)

	pushTurnCost(t, w1, 0.10, 100)
	awaitSnapshot(t, handles[0], "w1 cost folded", func(i supervisor.SessionInfo) bool {
		return i.Cost.USD == 0.10
	})
	pushTurnCost(t, w2, 0.25, 200)
	awaitSnapshot(t, handles[1], "w2 cost folded", func(i supervisor.SessionInfo) bool {
		return i.Cost.USD == 0.25
	})

	cost, usage := sup.FleetUsage()

	// The fleet total is the sum of the sessions' costs...
	if want := 0.35; !floatNear(cost.USD, want) {
		t.Errorf("FleetUsage cost = $%.4f, want $%.4f (sum of $0.10 + $0.25)", cost.USD, want)
	}
	// ...and equals the sum computed directly from the sessions' own rows.
	rows, err := sup.Roster(context.Background())
	if err != nil {
		t.Fatalf("Roster: %v", err)
	}
	var wantUSD float64
	var wantTokens int
	for _, r := range rows {
		wantUSD += r.Cost.USD
		wantTokens += r.Usage.InputTokens
	}
	if !floatNear(cost.USD, wantUSD) {
		t.Errorf("FleetUsage cost = $%.4f, want $%.4f (sum of the roster rows)", cost.USD, wantUSD)
	}
	if usage.InputTokens != wantTokens || usage.InputTokens != 300 {
		t.Errorf("FleetUsage usage input tokens = %d, want %d (100 + 200)", usage.InputTokens, wantTokens)
	}
}

// TestFleetUsageGrowsWithSessionCost pins that the total tracks a session's cost
// as it accumulates: a second turn on one session raises the fleet total by that
// turn's delta.
func TestFleetUsageGrowsWithSessionCost(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()
	w := startRosterCacheWorker(t, "0.3.0")
	sup, handles := adoptCountingWorkers(t, root, w)

	pushTurnCost(t, w, 0.10, 10)
	awaitSnapshot(t, handles[0], "first turn cost", func(i supervisor.SessionInfo) bool {
		return i.Cost.USD == 0.10
	})
	if cost, _ := sup.FleetUsage(); !floatNear(cost.USD, 0.10) {
		t.Fatalf("FleetUsage after one turn = $%.4f, want $0.10", cost.USD)
	}

	// A second turn accumulates onto the same session's running total (the cache
	// folds deltas), so the fleet total rises to the sum.
	pushTurnCost(t, w, 0.15, 20)
	awaitSnapshot(t, handles[0], "accumulated cost", func(i supervisor.SessionInfo) bool {
		return floatNear(i.Cost.USD, 0.25)
	})
	cost, usage := sup.FleetUsage()
	if !floatNear(cost.USD, 0.25) {
		t.Errorf("FleetUsage after two turns = $%.4f, want $0.25 (0.10 + 0.15)", cost.USD)
	}
	if usage.InputTokens != 30 {
		t.Errorf("FleetUsage usage input tokens = %d, want 30 (10 + 20)", usage.InputTokens)
	}
}

// TestFleetUsageEmptyIsZero pins that a router with no live sessions reports a
// zero total rather than panicking on the empty sum.
func TestFleetUsageEmptyIsZero(t *testing.T) {
	shortRuntimeDir(t)
	root := t.TempDir()
	sup, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })

	cost, usage := sup.FleetUsage()
	if cost.USD != 0 || usage.InputTokens != 0 {
		t.Errorf("FleetUsage on an empty router = %+v / %+v, want zero", cost, usage)
	}
}

// floatNear compares two USD sums with a tolerance, so the assertions do not
// hinge on exact IEEE-754 equality of summed floats.
func floatNear(a, b float64) bool {
	const eps = 1e-9
	d := a - b
	return d < eps && d > -eps
}
