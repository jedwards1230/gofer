//go:build workerbench && unix

// This file is an N-workers STRESS/BENCHMARK harness, not a correctness test.
// It spawns ~50 real detached worker processes and reports numbers; it is slow
// (minutes), memory-hungry, and its output is measurements, not assertions.
//
// # Why a build tag and not testing.Short()
//
// The obvious "expensive test" idiom in Go is `if testing.Short() { t.Skip() }`,
// and it does NOT work here. CI (.github/workflows/ci.yml) runs bare
// `go test ./...` and `go test -race ./...` with NO `-short` flag, and
// testing.Short() is FALSE by default — so a Short-gated benchmark would run on
// every push, spawn 50 processes inside the runner, and very likely blow the
// job up on time or memory. A build tag is the only gate that actually excludes
// it from those two commands, and it is the repo's established idiom for
// conditionally-compiled files (see the `//go:build unix` files across
// internal/daemon/ and internal/sandbox/). `unix` is in the tag because the
// harness shells out to ps(1) for RSS and reasons about pids/SIGKILL.
//
// The testing.Short() skip below is belt-and-braces only — it costs nothing and
// documents intent for anyone who runs `go test -tags workerbench -short`. The
// build tag is what does the work.
//
// Run it (from the repo root):
//
//	go test -tags workerbench -run TestWorkerFleetBenchmark -v -timeout 30m ./internal/router/
//
// See docs/TESTING.md for the knobs.
package router

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/wirestream"
)

// benchConfig is the harness's shape. Every field is env-overridable so a
// smaller machine can produce a comparable (if lower-N) run rather than
// silently failing — a re-run MUST report the same values to be comparable.
type benchConfig struct {
	// workers is the target fleet size, and also the router's MaxWorkers cap:
	// admission control is exercised, configured for exactly this many.
	workers int
	// checkpoints are the fleet sizes at which the Roster/List latency curve is
	// sampled. The CURVE is the point — a single number at N=50 cannot show
	// whether call cost scales with worker count.
	checkpoints []int
	// callIters is how many Roster/List calls are timed per checkpoint.
	callIters int
	// fanoutSessions / fanoutSubscribers / fanoutTurns shape the event-throughput
	// phase. Subscribers exercise the daemon's per-peer delivery cost (one write
	// per peer); they do NOT amplify marshal cost — daemon.broadcastGoferEvent
	// already marshals each event once and reuses the bytes for every peer
	// (internal/daemon/handlers.go). What the M6 worker split adds, and what a
	// marshal-once bridge removes, is the ROUTER's second-hop decode+re-encode.
	fanoutSessions    int
	fanoutSubscribers int
	fanoutTurns       int
}

func benchConfigFromEnv(t *testing.T) benchConfig {
	t.Helper()
	cfg := benchConfig{
		workers:           50,
		callIters:         20,
		fanoutSessions:    4,
		fanoutSubscribers: 8,
		fanoutTurns:       5, // the faux script holds 8 turns per worker; stay under it
	}
	cfg.workers = benchEnvInt(t, "GOFER_BENCH_WORKERS", cfg.workers)
	cfg.callIters = benchEnvInt(t, "GOFER_BENCH_CALL_ITERS", cfg.callIters)
	cfg.fanoutSessions = benchEnvInt(t, "GOFER_BENCH_FANOUT_SESSIONS", cfg.fanoutSessions)
	cfg.fanoutSubscribers = benchEnvInt(t, "GOFER_BENCH_FANOUT_SUBSCRIBERS", cfg.fanoutSubscribers)
	cfg.fanoutTurns = benchEnvInt(t, "GOFER_BENCH_FANOUT_TURNS", cfg.fanoutTurns)

	// Checkpoints scale with the fleet so a reduced-N run still yields a curve.
	for _, frac := range []int{50, 20, 4, 2, 1} { // N/50, N/20, N/4, N/2, N
		if n := cfg.workers / frac; n >= 1 {
			cfg.checkpoints = appendUnique(cfg.checkpoints, n)
		}
	}
	cfg.checkpoints = appendUnique(cfg.checkpoints, 1)
	sort.Ints(cfg.checkpoints)
	return cfg
}

func benchEnvInt(t *testing.T, key string, def int) int {
	t.Helper()
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		t.Fatalf("%s=%q is not a positive integer", key, raw)
	}
	return n
}

func appendUnique(xs []int, v int) []int {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}

// benchReaper guarantees that every worker process this harness starts is dead
// before the test binary exits — on the happy path, on an early t.Fatal, and on
// a panic.
//
// Ordinary cleanup is NOT sufficient here: M6 workers are spawned with Setsid
// and are DELIBERATELY designed to outlive their parent (design §3,
// Supervisor.Close explicitly stops signalling them), so 50 orphans reparented
// to pid 1 is the default outcome of a failed run. The reaper therefore kills by
// pid — both the pids it recorded at Create time and any pid advertised in a
// worker endpoint file — polls daemon.ProcessAlive until each is genuinely gone,
// unlinks the endpoint/socket residue, and t.Errorf's LOUDLY if anything
// survives. That last part is the leak check: a cleanup regression fails the run
// instead of quietly leaving processes behind.
type benchReaper struct {
	t    *testing.T
	once sync.Once

	mu   sync.Mutex
	pids map[int]string // worker pid -> session id
	sup  *Supervisor
}

// newBenchReaper registers the reap as the FIRST cleanup, so LIFO ordering runs
// it LAST — after the router's own Close (which by design leaves workers alive).
func newBenchReaper(t *testing.T) *benchReaper {
	t.Helper()
	r := &benchReaper{t: t, pids: make(map[int]string)}
	t.Cleanup(r.reap)
	return r
}

func (r *benchReaper) attach(s *Supervisor) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sup = s
}

func (r *benchReaper) record(pid int, sessionID string) {
	if pid <= 0 || pid == os.Getpid() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pids[pid] = sessionID
}

// reap is idempotent (sync.Once): the panic path below and the t.Cleanup both
// call it, and whichever arrives first does the work.
func (r *benchReaper) reap() {
	r.once.Do(r.reapOnce)
}

func (r *benchReaper) reapOnce() {
	r.mu.Lock()
	sup := r.sup
	targets := make(map[int]string, len(r.pids))
	for pid, id := range r.pids {
		targets[pid] = id
	}
	r.mu.Unlock()

	// (1) Kill through the router's own handles first — the ordinary path.
	if sup != nil {
		killWorkers(sup)
	}

	// (2) Sweep the endpoint files for anything the handles missed (a worker
	// whose Create failed after the fork, say), and fold those pids into the
	// target set so they are verified dead too.
	entries, err := daemon.ListWorkerEndpoints()
	if err != nil {
		r.t.Errorf("bench reaper: list worker endpoints: %v", err)
	}
	for _, e := range entries {
		if e.Endpoint.PID > 0 && e.Endpoint.PID != os.Getpid() {
			if _, ok := targets[e.Endpoint.PID]; !ok {
				targets[e.Endpoint.PID] = e.UUID
			}
		}
	}

	// (3) SIGKILL every target. os.FindProcess never fails on unix and Kill
	// tolerates an already-dead process, so this is unconditional.
	for pid := range targets {
		if proc, ferr := os.FindProcess(pid); ferr == nil {
			_ = proc.Kill()
		}
	}

	// (4) Poll — never sleep-and-hope — until every target is genuinely gone.
	deadline := time.Now().Add(30 * time.Second)
	var alive []int
	for {
		alive = alive[:0]
		for pid := range targets {
			if daemon.ProcessAlive(pid) {
				alive = append(alive, pid)
			}
		}
		if len(alive) == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// (5) Unlink the on-disk residue (endpoint file + socket) for every session
	// this harness touched.
	for _, id := range targets {
		if id != "" {
			removeWorkerArtifacts(id)
		}
	}
	for _, e := range entries {
		removeWorkerArtifacts(e.UUID)
	}

	// (6) The leak check: loud failure if anything survived.
	if len(alive) > 0 {
		sort.Ints(alive)
		r.t.Errorf("PROCESS LEAK: %d spawned worker pid(s) still alive after reap: %v", len(alive), alive)
	}
	if left, lerr := daemon.ListWorkerEndpoints(); lerr == nil && len(left) > 0 {
		r.t.Errorf("ARTIFACT LEAK: %d worker endpoint file(s) survived cleanup", len(left))
	}
}

// TestWorkerFleetBenchmark spawns a fleet of real worker processes and measures
// the four costs M6 §10 ("Costs & risks") asserts without numbers:
//
//  1. RSS — §10's "~10–20 MB RSS baseline" per worker.
//  2. Roster/List latency AS A CURVE over fleet size — today every call fans
//     per-worker RPCs out, so cost should scale with N. This is the metric a
//     push-based roster cache is meant to flatten.
//  3. Event throughput with several subscribers attached — the metric a
//     marshal-once fan-out is meant to raise.
//  4. Spawn latency distribution — §10's "tens of ms" startup cost.
//
// It asserts almost nothing: a failure here means the harness could not take a
// measurement (or leaked a process), not that a number moved.
func TestWorkerFleetBenchmark(t *testing.T) {
	if testing.Short() {
		// Belt-and-braces only; the build tag is the real gate (see the file doc).
		t.Skip("worker fleet benchmark: skipped under -short")
	}

	cfg := benchConfigFromEnv(t)
	rep := &benchReport{cfg: cfg}

	shortRuntimeDir(t)
	root := t.TempDir()
	workspace := t.TempDir()

	reaper := newBenchReaper(t)
	// t.Cleanup already runs on a panic (tRunner reaps cleanups before
	// re-panicking), but a detached-process leak is bad enough to justify the
	// explicit belt: reap, then let the panic continue.
	defer func() {
		if p := recover(); p != nil {
			reaper.reap()
			panic(p)
		}
	}()

	sup, err := New(Config{
		Root: root,
		// Admission control configured for exactly the target fleet: the harness
		// runs WITH the cap engaged, not around it.
		MaxWorkers:   cfg.workers,
		NewWorkerCmd: fauxWorkerSeam(root),
	})
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	reaper.attach(sup)
	t.Cleanup(func() { _ = sup.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	rep.selfRSSBeforeKiB = benchSelfRSS(t)

	// ---- Phase 1: spawn the fleet, sampling the Roster/List curve en route ----
	//
	// Creates are SEQUENTIAL on purpose: a concurrent fan-out would measure lock
	// contention and machine parallelism rather than the per-session fork/exec →
	// discovery → dial → handshake → adopted cost the distribution is about.
	checkpoints := make(map[int]bool, len(cfg.checkpoints))
	for _, n := range cfg.checkpoints {
		checkpoints[n] = true
	}

	ids := make([]string, 0, cfg.workers)
	spawns := make([]time.Duration, 0, cfg.workers)
	for i := 1; i <= cfg.workers; i++ {
		start := time.Now()
		info, cerr := sup.Create(ctx, "", supervisor.CreateOptions{Cwd: workspace})
		elapsed := time.Since(start)
		if cerr != nil {
			// An honest short fleet beats a faked full one: record what was
			// achieved and why, and carry on measuring at the achieved size.
			rep.spawnStoppedAt = i
			rep.spawnStopErr = cerr.Error()
			t.Logf("spawn %d/%d failed, continuing with a fleet of %d: %v", i, cfg.workers, len(ids), cerr)
			break
		}
		spawns = append(spawns, elapsed)
		ids = append(ids, info.ID)
		if h, ok := sup.get(info.ID); ok {
			reaper.record(h.pid, info.ID)
		}
		if checkpoints[len(ids)] {
			rep.curve = append(rep.curve, measureRosterCurvePoint(t, ctx, sup, len(ids), cfg.callIters))
		}
	}
	rep.fleet = len(ids)
	rep.spawn = newDurStats(spawns)
	if rep.fleet == 0 {
		t.Fatal("no workers spawned; nothing to measure")
	}
	// A checkpoint at the achieved fleet size, in case the fleet fell short of
	// any configured checkpoint.
	if len(rep.curve) == 0 || rep.curve[len(rep.curve)-1].workers != rep.fleet {
		rep.curve = append(rep.curve, measureRosterCurvePoint(t, ctx, sup, rep.fleet, cfg.callIters))
	}

	// ---- Phase 2: RSS of the settled fleet ----
	rep.rss = measureFleetRSS(t, sup)
	rep.selfRSSAfterKiB = benchSelfRSS(t)

	// ---- Phase 3: event throughput through the fan-out ----
	rep.fanout = measureFanout(t, ctx, sup, ids, workspace, cfg)

	// ---- Report ----
	out := rep.render()
	// Written with fmt, not t.Log: t.Log prefixes and indents every line, which
	// makes the table a mess to paste into a PR body.
	fmt.Print(out)
	if path := os.Getenv("GOFER_BENCH_OUT"); path != "" {
		if werr := os.WriteFile(path, []byte(out), 0o600); werr != nil {
			t.Errorf("write GOFER_BENCH_OUT=%s: %v", path, werr)
		}
	}
}

// ---------------------------------------------------------------------------
// Measurement
// ---------------------------------------------------------------------------

// curvePoint is one sample of the Roster/List latency curve.
type curvePoint struct {
	workers int
	roster  durStats
	list    durStats
}

// measureRosterCurvePoint times Roster and List against the CURRENT fleet. Both
// are measured because they cost differently: Roster is purely the per-worker
// RPC fan-out, while List adds the on-disk journal union — so if a roster cache
// flattens Roster but not List, the pair says so.
func measureRosterCurvePoint(t *testing.T, ctx context.Context, sup *Supervisor, workers, iters int) curvePoint {
	t.Helper()
	roster := make([]time.Duration, 0, iters)
	list := make([]time.Duration, 0, iters)
	for range iters {
		start := time.Now()
		rows, err := sup.Roster(ctx)
		roster = append(roster, time.Since(start))
		if err != nil {
			t.Fatalf("Roster at %d workers: %v", workers, err)
		}
		if len(rows) != workers {
			// Not fatal: a worker can be mid-anything. Worth knowing, because a
			// short roster is a cheaper roster and would bias the number.
			t.Logf("note: Roster returned %d rows at a fleet of %d", len(rows), workers)
		}

		start = time.Now()
		if _, err := sup.List(ctx); err != nil {
			t.Fatalf("List at %d workers: %v", workers, err)
		}
		list = append(list, time.Since(start))
	}
	return curvePoint{workers: workers, roster: newDurStats(roster), list: newDurStats(list)}
}

// fleetRSS is the settled fleet's memory footprint.
type fleetRSS struct {
	sampled  int
	totalKiB int64
	stats    []int64 // sorted per-worker RSS in KiB
}

// measureFleetRSS reads per-worker RSS via ps(1). ps is used rather than a
// dependency because RSS is an OS-level number, /proc is Linux-only, and the
// harness already assumes unix.
func measureFleetRSS(t *testing.T, sup *Supervisor) fleetRSS {
	t.Helper()
	var pids []int
	for _, h := range sup.snapshotHandles() {
		if h.pid > 0 {
			pids = append(pids, h.pid)
		}
	}
	byPID, err := processRSSKiB(pids)
	if err != nil {
		t.Fatalf("read worker RSS: %v", err)
	}
	out := fleetRSS{}
	for _, kib := range byPID {
		out.stats = append(out.stats, kib)
		out.totalKiB += kib
	}
	out.sampled = len(out.stats)
	sort.Slice(out.stats, func(i, j int) bool { return out.stats[i] < out.stats[j] })
	return out
}

// processRSSKiB returns resident set size in KiB for each pid, via
// `ps -o pid=,rss= -p <list>`. Pids that have exited are simply absent.
func processRSSKiB(pids []int) (map[int]int64, error) {
	out := make(map[int]int64, len(pids))
	if len(pids) == 0 {
		return out, nil
	}
	args := make([]string, 0, len(pids))
	for _, pid := range pids {
		args = append(args, strconv.Itoa(pid))
	}
	cmd := exec.Command("ps", "-o", "pid=,rss=", "-p", strings.Join(args, ","))
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ps -o pid=,rss=: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pid, perr := strconv.Atoi(fields[0])
		kib, kerr := strconv.ParseInt(fields[1], 10, 64)
		if perr != nil || kerr != nil {
			continue
		}
		out[pid] = kib
	}
	return out, nil
}

func benchSelfRSS(t *testing.T) int64 {
	t.Helper()
	byPID, err := processRSSKiB([]int{os.Getpid()})
	if err != nil {
		t.Fatalf("read own RSS: %v", err)
	}
	return byPID[os.Getpid()]
}

// fanoutResult is the event-throughput measurement.
type fanoutResult struct {
	sessions    int
	subscribers int // per session
	turns       int // per session
	deliveries  int64
	elapsed     time.Duration
}

func (f fanoutResult) perSec() float64 {
	if f.elapsed <= 0 {
		return 0
	}
	return float64(f.deliveries) / f.elapsed.Seconds()
}

// measureFanout drives real turns on a subset of the live fleet with several
// real WebSocket peers attached to each session, and counts the notifications
// those peers receive per second of prompt time.
//
// The denominator is deliberately the PROMPT wall time, not "time until the last
// notification was read": the per-event work on the forwarding path happens
// inline before session/prompt returns, so prompt time is what a change to that
// path moves. Delivery counts are settled separately, after the clock stops.
//
// What subscribers do and do not measure: they scale the daemon's per-peer
// WRITE cost, not marshal cost. daemon.broadcastGoferEvent already marshals an
// event once and reuses the bytes across peers, so peer count does not multiply
// encoding work at that hop. The cost the M6 worker split introduced — and the
// one a marshal-once bridge removes — is the ROUTER's decode+re-encode on the
// second hop, which is per-event and independent of peer count.
func measureFanout(t *testing.T, ctx context.Context, sup *Supervisor, ids []string, cwd string, cfg benchConfig) fanoutResult {
	t.Helper()

	sessions := min(cfg.fanoutSessions, len(ids))
	if sessions == 0 {
		return fanoutResult{}
	}
	target := ids[:sessions]

	d := daemon.New(sup, daemon.Config{DefaultModel: "faux"})
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")

	var delivered atomic.Int64

	// Subscribers: real peers attached via session/load, each draining its
	// notification channel into the counter (the Client contract REQUIRES the
	// channel be drained, and an undrained peer would also stall the fan-out).
	for _, id := range target {
		for range cfg.fanoutSubscribers {
			c := mustDial(t, ctx, addr)
			go func() {
				for range c.Notifications() {
					delivered.Add(1)
				}
			}()
			lctx, lcancel := context.WithTimeout(ctx, 30*time.Second)
			_, lerr := c.Call(lctx, acp.MethodSessionLoad, acp.LoadSessionRequest{SessionID: id, Cwd: cwd})
			lcancel()
			if lerr != nil {
				t.Fatalf("session/load %s (subscriber attach): %v", id, lerr)
			}
		}
	}

	// The driver runs the turns. It must drain too.
	driver := mustDial(t, ctx, addr)
	go func() {
		for range driver.Notifications() {
		}
	}()

	// Discard everything the attach replays so the measured window contains only
	// live turn traffic. Poll for quiescence rather than sleeping a magic number:
	// the counter is stable when it has not moved across a full poll interval.
	waitCounterSettled(t, &delivered, 30*time.Second)
	delivered.Store(0)

	// Turns run concurrently across sessions — that is the realistic shape (N
	// independent sessions streaming at once) and it keeps the fan-out path
	// genuinely contended.
	start := time.Now()
	var wg sync.WaitGroup
	errs := make([]error, len(target))
	for i, id := range target {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range cfg.fanoutTurns {
				pctx, pcancel := context.WithTimeout(ctx, 60*time.Second)
				_, perr := driver.Call(pctx, acp.MethodSessionPrompt, acp.PromptRequest{
					SessionID: id,
					Prompt:    []acp.ContentBlock{acp.TextBlock("bench")},
				})
				pcancel()
				if perr != nil {
					errs[i] = perr
					return
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	for i, perr := range errs {
		if perr != nil {
			t.Fatalf("session/prompt on %s: %v", target[i], perr)
		}
	}

	// Let the in-flight reads land before counting; this is outside the clock.
	waitCounterSettled(t, &delivered, 30*time.Second)

	return fanoutResult{
		sessions:    sessions,
		subscribers: cfg.fanoutSubscribers,
		turns:       cfg.fanoutTurns,
		deliveries:  delivered.Load(),
		elapsed:     elapsed,
	}
}

// waitCounterSettled blocks until n has stopped moving or the deadline passes.
// It is a quiescence poll, not a sleep: the observable condition is "no further
// deliveries".
//
// "Stopped" requires settleReads CONSECUTIVE equal samples, not one. A single
// quiet poll interval is not evidence the stream ended — a natural lull between
// a turn's events (provider latency, a tool call) can produce one, and treating
// that as settled would silently bias the throughput number in both directions:
// it truncates the pre-measurement drain (leaving replay traffic to be counted
// as live) and cuts the post-measurement settle short (undercounting
// deliveries). Requiring several consecutive quiet intervals makes a mid-stream
// false settle much less likely for the same worst-case cost.
const (
	settleReads    = 3
	settleInterval = 50 * time.Millisecond
)

func waitCounterSettled(t *testing.T, n *atomic.Int64, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	last := int64(-1)
	stable := 0
	for time.Now().Before(deadline) {
		switch cur := n.Load(); cur {
		case last:
			if stable++; stable >= settleReads {
				return
			}
		default:
			last, stable = cur, 0
		}
		time.Sleep(settleInterval)
	}
	t.Logf("note: notification counter never settled within %v (last %d)", budget, n.Load())
}

// ---------------------------------------------------------------------------
// Stats + report
// ---------------------------------------------------------------------------

type durStats struct {
	n              int
	mean           time.Duration
	p50, p90, max_ time.Duration
}

func newDurStats(ds []time.Duration) durStats {
	if len(ds) == 0 {
		return durStats{}
	}
	sorted := make([]time.Duration, len(ds))
	copy(sorted, ds)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var total time.Duration
	for _, d := range sorted {
		total += d
	}
	return durStats{
		n:    len(sorted),
		mean: total / time.Duration(len(sorted)),
		p50:  nearestRank(sorted, 0.50),
		p90:  nearestRank(sorted, 0.90),
		max_: sorted[len(sorted)-1],
	}
}

// nearestRank is the nearest-rank percentile: no interpolation, so every
// reported value is a value that was actually observed.
func nearestRank(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p*float64(len(sorted))+0.999999) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

type benchReport struct {
	cfg              benchConfig
	fleet            int
	spawnStoppedAt   int
	spawnStopErr     string
	spawn            durStats
	curve            []curvePoint
	rss              fleetRSS
	selfRSSBeforeKiB int64
	selfRSSAfterKiB  int64
	fanout           fanoutResult
}

func (r *benchReport) render() string {
	var b strings.Builder
	ms := func(d time.Duration) string { return fmt.Sprintf("%.2f", float64(d.Microseconds())/1000) }
	mib := func(kib int64) string { return fmt.Sprintf("%.1f", float64(kib)/1024) }

	fmt.Fprintf(&b, "\n=== gofer M6 worker-fleet benchmark ===\n\n")

	// Machine + run context first: a comparison run that does not match these is
	// not a comparison.
	fmt.Fprintf(&b, "Run context\n")
	fmt.Fprintf(&b, "  go / os / arch     : %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "  NumCPU / GOMAXPROCS: %d / %d\n", runtime.NumCPU(), runtime.GOMAXPROCS(0))
	fmt.Fprintf(&b, "  workers requested  : %d (router MaxWorkers = %d)\n", r.cfg.workers, r.cfg.workers)
	fmt.Fprintf(&b, "  workers achieved   : %d\n", r.fleet)
	if r.spawnStopErr != "" {
		fmt.Fprintf(&b, "  spawn stopped at   : #%d — %s\n", r.spawnStoppedAt, r.spawnStopErr)
	}
	fmt.Fprintf(&b, "  roster call iters  : %d per checkpoint\n", r.cfg.callIters)
	fmt.Fprintf(&b, "  timestamp (UTC)    : %s\n", time.Now().UTC().Format(time.RFC3339))
	if load := os.Getenv("GOFER_BENCH_LOAD_NOTE"); load != "" {
		fmt.Fprintf(&b, "  machine load       : %s\n", load)
	}
	fmt.Fprintf(&b, "\n")

	fmt.Fprintf(&b, "1. Process RSS  [AUTHORITATIVE]  (M6 §10 claims ~10-20 MB baseline per worker)\n")
	fmt.Fprintf(&b, "   Memory is steady under CPU contention; this is the number that answers\n")
	fmt.Fprintf(&b, "   \"how many agents fit on one box\".\n")
	if r.rss.sampled == 0 {
		fmt.Fprintf(&b, "  (no workers sampled)\n\n")
	} else {
		fmt.Fprintf(&b, "  workers sampled    : %d\n", r.rss.sampled)
		fmt.Fprintf(&b, "  per-worker RSS MiB : min %s  p50 %s  p90 %s  max %s\n",
			mib(r.rss.stats[0]), mib(kibRank(r.rss.stats, 0.50)), mib(kibRank(r.rss.stats, 0.90)), mib(r.rss.stats[len(r.rss.stats)-1]))
		fmt.Fprintf(&b, "  fleet total RSS    : %s MiB\n", mib(r.rss.totalKiB))
		fmt.Fprintf(&b, "  router (test proc) : %s MiB before -> %s MiB after (delta %s MiB)\n\n",
			mib(r.selfRSSBeforeKiB), mib(r.selfRSSAfterKiB), mib(r.selfRSSAfterKiB-r.selfRSSBeforeKiB))
	}

	fmt.Fprintf(&b, "2. Roster / List latency vs fleet size  [INDICATIVE ONLY]\n")
	fmt.Fprintf(&b, "   Wall clock, contention-sensitive. The SHAPE (does cost scale with N?) is\n")
	fmt.Fprintf(&b, "   the signal; the absolute values are not. The authoritative version of this\n")
	fmt.Fprintf(&b, "   claim is the gofer/roster frame count (TestRosterWireFrameCount).\n")
	fmt.Fprintf(&b, "  %-8s | %-32s | %-32s\n", "workers", "Roster mean/p50/p90/max (ms)", "List mean/p50/p90/max (ms)")
	fmt.Fprintf(&b, "  %-8s-+-%-32s-+-%-32s\n", strings.Repeat("-", 8), strings.Repeat("-", 32), strings.Repeat("-", 32))
	for _, p := range r.curve {
		fmt.Fprintf(&b, "  %-8d | %-32s | %-32s\n", p.workers,
			fmt.Sprintf("%s / %s / %s / %s", ms(p.roster.mean), ms(p.roster.p50), ms(p.roster.p90), ms(p.roster.max_)),
			fmt.Sprintf("%s / %s / %s / %s", ms(p.list.mean), ms(p.list.p50), ms(p.list.p90), ms(p.list.max_)))
	}
	// Per-SEGMENT slopes, not first-to-last. A single end-to-end ratio anchored
	// at N=1 is dominated by fixed per-call overhead amortizing away, which
	// reads as "super-linear growth" when the truth is the opposite. The slope
	// between adjacent checkpoints is what actually says whether cost scales
	// with N: ~1.0 is linear, >1 super-linear, <1 sub-linear.
	if len(r.curve) >= 2 {
		fmt.Fprintf(&b, "  per-segment slope (dlog(mean)/dlog(workers); ~1.0 = linear):\n")
		for i := 1; i < len(r.curve); i++ {
			prev, cur := r.curve[i-1], r.curve[i]
			if prev.roster.mean <= 0 || prev.workers <= 0 {
				continue
			}
			workerRatio := float64(cur.workers) / float64(prev.workers)
			if workerRatio <= 1 {
				continue
			}
			slope := math.Log(float64(cur.roster.mean)/float64(prev.roster.mean)) / math.Log(workerRatio)
			fmt.Fprintf(&b, "    %3d -> %3d workers : %.2f\n", prev.workers, cur.workers, slope)
		}
	}
	fmt.Fprintf(&b, "\n")

	fmt.Fprintf(&b, "3. Event throughput through the fan-out  [INDICATIVE ONLY]\n")
	fmt.Fprintf(&b, "   Dominated by socket time and machine load. The authoritative version of\n")
	fmt.Fprintf(&b, "   this claim is allocs/op on the fan-out path (BenchmarkEventForward).\n")
	fmt.Fprintf(&b, "  sessions driven    : %d (concurrently), %d turns each\n", r.fanout.sessions, r.fanout.turns)
	fmt.Fprintf(&b, "  subscribers        : %d per session (%d peers total)\n", r.fanout.subscribers, r.fanout.sessions*r.fanout.subscribers)
	fmt.Fprintf(&b, "  notifications      : %d delivered in %s\n", r.fanout.deliveries, r.fanout.elapsed.Round(time.Millisecond))
	fmt.Fprintf(&b, "  throughput         : %.0f notifications/sec\n\n", r.fanout.perSec())

	fmt.Fprintf(&b, "4. Spawn latency  [INDICATIVE ONLY]  (fork/exec -> discovered -> dialed -> handshaked -> adopted)\n")
	fmt.Fprintf(&b, "   fork/exec under load skews the tail hardest; read p50, distrust max.\n")
	fmt.Fprintf(&b, "  samples            : %d (sequential Creates)\n", r.spawn.n)
	fmt.Fprintf(&b, "  ms                 : mean %s  p50 %s  p90 %s  max %s\n\n",
		ms(r.spawn.mean), ms(r.spawn.p50), ms(r.spawn.p90), ms(r.spawn.max_))

	return b.String()
}

// kibRank is nearestRank for the RSS samples.
func kibRank(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p*float64(len(sorted))+0.999999) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// ---------------------------------------------------------------------------
// gofer/roster wire-frame count per Roster/List call
// ---------------------------------------------------------------------------

// TestRosterWireFrameCount counts the `gofer/roster` REQUEST FRAMES the router
// puts on the wire per Roster and per List call, at several fleet sizes.
//
// This is the mechanism metric, and the reason it is worth more than the wall
// clock: today the router asks EVERY live worker for its roster on every call,
// so the count is expected to be exactly N (and List, which overlays the live
// roster onto the on-disk union, another N) — linear in fleet size. A
// push-based roster cache replaces those per-call RPCs with a local read, which
// should take the count to 0. That is a discrete integer no machine noise can
// muddy, and it proves the call pattern changed rather than merely that a timer
// moved.
//
// It runs against IN-PROCESS counting fake workers adopted through the router's
// real adoption path, NOT against spawned worker processes. That is deliberate:
// the frames-per-call number is a property of the router's call pattern and is
// identical either way, and counting frames requires a worker end this test
// controls. Every other metric in this harness uses real processes.
func TestRosterWireFrameCount(t *testing.T) {
	if testing.Short() {
		t.Skip("roster wire-frame count: skipped under -short")
	}
	cfg := benchConfigFromEnv(t)

	var b strings.Builder
	fmt.Fprintf(&b, "\n=== gofer M6 roster wire-frame count ===\n\n")
	fmt.Fprintf(&b, "  go / os / arch     : %s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "  NumCPU / GOMAXPROCS: %d / %d\n\n", runtime.NumCPU(), runtime.GOMAXPROCS(0))
	fmt.Fprintf(&b, "  %-8s | %-18s | %-18s | %-18s\n", "workers", "STEADY-STATE per", "STEADY-STATE per", "ONE-TIME warm-up")
	fmt.Fprintf(&b, "  %-8s | %-18s | %-18s | %-18s\n", "", "Roster() call", "List() call", "(total frames)")
	fmt.Fprintf(&b, "  %-8s-+-%-18s-+-%-18s-+-%-18s\n", strings.Repeat("-", 8), strings.Repeat("-", 18), strings.Repeat("-", 18), strings.Repeat("-", 18))

	for _, n := range cfg.checkpoints {
		c := measureRosterFrames(t, n)
		fmt.Fprintf(&b, "  %-8d | %-18s | %-18s | %-18s\n", n,
			fmt.Sprintf("%.2f", c.perRoster), fmt.Sprintf("%.2f", c.perList),
			fmt.Sprintf("%d", c.warmup))
	}
	fmt.Fprintf(&b, "\n  [AUTHORITATIVE] a count, not a duration: contention cannot change an integer.\n")
	fmt.Fprintf(&b, "  Pre-cache baseline: exactly N per call — one RPC per live worker, every call.\n")
	fmt.Fprintf(&b, "  Post-cache: steady state should be 0 — the per-call RPC is gone, not cheaper.\n")
	fmt.Fprintf(&b, "  Warm-up is the cache's ONE-TIME seed (~1 RPC per worker at adoption), paid\n")
	fmt.Fprintf(&b, "  once rather than per call. It is reported separately on purpose: folding it\n")
	fmt.Fprintf(&b, "  into a per-call average understates the win and invents a phantom recurring\n")
	fmt.Fprintf(&b, "  cost. A steady-state row above 0 would mean handles are MISSING the cache\n")
	fmt.Fprintf(&b, "  and falling back to live RPCs (by design, for a degraded worker).\n\n")

	out := b.String()
	fmt.Print(out)
	if path := os.Getenv("GOFER_BENCH_FRAMES_OUT"); path != "" {
		if werr := os.WriteFile(path, []byte(out), 0o600); werr != nil {
			t.Errorf("write GOFER_BENCH_FRAMES_OUT=%s: %v", path, werr)
		}
	}
}

// rosterFrameCost separates the two distinct costs a roster cache has, which a
// single "frames per call" number conflates.
type rosterFrameCost struct {
	// warmup is total gofer/roster frames observed from adoption through the
	// first settling calls: the cache's ONE-TIME seed. The push-based cache seeds
	// each handle with a single live RPC and thereafter serves from an atomic
	// snapshot, so this is bounded by fleet size and paid once per worker.
	warmup int64
	// perRoster / perList are STEADY-STATE frames per call, measured only after
	// the cache has settled. This is the number that answers "does a roster call
	// still cost an RPC per worker?" — and the one comparable to the pre-cache
	// baseline of exactly N.
	perRoster, perList float64
}

// measureRosterFrames adopts n counting workers into a fresh router and measures
// the gofer/roster wire frames its roster calls produce.
//
// It deliberately measures warm-up and steady state SEPARATELY. Measuring from a
// cold router conflates them: seeding is asynchronous, so seed RPCs land during
// the first calls and show up as a small fractional "per-call" cost that is
// really a one-time startup charge racing the measurement window. Reporting that
// fraction as the steady-state answer would misstate what the cache does — and
// reporting 0 without settling first would be unverified. So: settle, then
// measure.
func measureRosterFrames(t *testing.T, n int) rosterFrameCost {
	t.Helper()
	// A per-N runtime dir keeps each round's endpoint files to itself, so the
	// router adopts exactly the n workers this round planted.
	shortRuntimeDirRound(t, n)
	root := t.TempDir()

	var frames atomic.Int64
	for range n {
		startCountingWorker(t, uuid.Must(uuid.NewV7()).String(), &frames)
	}

	sup, err := New(Config{Root: root, NewWorkerCmd: fauxWorkerSeam(root)})
	if err != nil {
		t.Fatalf("router.New at %d workers: %v", n, err)
	}
	defer func() { _ = sup.Close() }()

	if got := liveWorkerCount(sup); got != n {
		t.Fatalf("adopted %d counting workers, want %d", got, n)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Warm-up: call until the frame counter stops moving, so every handle has
	// either seeded or fallen back. Polling for quiescence rather than sleeping
	// keeps this non-flaky on a loaded machine.
	var (
		last   int64 = -1
		stable int
	)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, rerr := sup.Roster(ctx); rerr != nil {
			t.Fatalf("Roster warm-up at %d workers: %v", n, rerr)
		}
		switch cur := frames.Load(); cur {
		case last:
			if stable++; stable >= settleReads {
				deadline = time.Time{} // settled
			}
		default:
			last, stable = cur, 0
		}
		if deadline.IsZero() {
			break
		}
		time.Sleep(settleInterval)
	}
	out := rosterFrameCost{warmup: frames.Load()}

	const iters = 20
	frames.Store(0)
	for range iters {
		if _, rerr := sup.Roster(ctx); rerr != nil {
			t.Fatalf("Roster at %d workers: %v", n, rerr)
		}
	}
	out.perRoster = float64(frames.Load()) / iters

	frames.Store(0)
	for range iters {
		if _, lerr := sup.List(ctx); lerr != nil {
			t.Fatalf("List at %d workers: %v", n, lerr)
		}
	}
	out.perList = float64(frames.Load()) / iters
	return out
}

// shortRuntimeDirRound is shortRuntimeDir for one round of the frame-count
// sweep: a fresh, short-rooted XDG_RUNTIME_DIR whose worker endpoint files
// belong to this round alone. It is separate from shortRuntimeDir (which this
// file may not modify) only because each round needs its own directory.
func shortRuntimeDirRound(t *testing.T, round int) {
	t.Helper()
	base := "/tmp"
	if _, err := os.Stat(base); err != nil {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, fmt.Sprintf("gfrb%d", round))
	if err != nil {
		t.Fatalf("mkdir short runtime dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("XDG_RUNTIME_DIR", dir)
}

// startCountingWorker plants a live in-process fake worker for id that answers
// gofer/hello (so the router ADOPTS it) and gofer/roster (INCREMENTING frames
// on every request frame), and errors everything else.
//
// It duplicates the shape of adopt_test.go's startFakeWorker rather than
// extending it: this harness may not modify existing files in this package, and
// the counting + roster-answering behavior is not what that helper does.
//
// It advertises the TEST PROCESS's own pid, which the router's self-pid guard
// records as pid 0 — so no Kill path can ever signal the test binary, and the
// fleet reaper (which skips its own pid) has nothing to kill here.
func startCountingWorker(t *testing.T, id string, frames *atomic.Int64) {
	t.Helper()
	sockPath, err := daemon.WorkerSocketPath(id)
	if err != nil {
		t.Fatalf("WorkerSocketPath: %v", err)
	}
	if mkErr := os.MkdirAll(filepath.Dir(sockPath), 0o700); mkErr != nil {
		t.Fatalf("mkdir workers dir: %v", mkErr)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	rosterRows := []wirestream.SessionInfo{{ID: id, Live: true, Status: "idle", Model: "faux"}}

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, aerr := websocket.Accept(w, r, nil)
		if aerr != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		for {
			var env struct {
				Method string          `json:"method"`
				ID     json.RawMessage `json:"id"`
			}
			if rerr := wsjson.Read(r.Context(), c, &env); rerr != nil {
				return
			}
			switch {
			case env.Method == "gofer/hello":
				_ = wsjson.Write(r.Context(), c, jsonRPCResult(env.ID, daemon.HelloResult{
					BinaryVersion: "bench",
					WireVersion:   daemon.WireVersion,
				}))
			case env.Method == "gofer/roster":
				frames.Add(1)
				_ = wsjson.Write(r.Context(), c, jsonRPCResult(env.ID, rosterRows))
			case len(env.ID) > 0:
				// Everything else errors promptly (an ignored request would wedge
				// the router on its bounded call for the full wire timeout).
				_ = wsjson.Write(r.Context(), c, jsonRPCError(env.ID, -32000, "counting worker: "+env.Method+" unimplemented"))
			}
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close(); _ = ln.Close() })

	writeEndpoint(t, id, os.Getpid(), daemon.WireVersion, "unix://"+sockPath)
}

// ---------------------------------------------------------------------------
// Allocations per forwarded event
// ---------------------------------------------------------------------------

// BenchmarkEventForward measures the per-event allocation cost of the ROUTER's
// second hop — decoding a worker's gofer/event envelope into a typed
// event.Event and re-encoding it — using the testing framework's own per-op
// allocation accounting (b.ReportAllocs), a far more stable instrument than a
// wall clock, which on this path is dominated by socket time.
//
// This is the cost the M6 worker split introduced and that a marshal-once /
// forward-verbatim bridge removes. Note it is NOT the daemon→client hop:
// daemon.broadcastGoferEvent already marshals once per event and reuses the
// bytes for every peer (internal/daemon/handlers.go), so nothing here scales
// with subscriber count.
//
// There is deliberately no "forward verbatim" comparison arm. Rebinding a
// json.RawMessage compiles to nothing measurable — a body with no function call
// is not protected from elimination by b.Loop, and the measured result was
// dominated by loop overhead (the LARGER payload reported a SMALLER ns/op,
// which is only possible if the number is noise). Its zero would also invite
// reading the pair as a ~1000x speedup. The meaningful figure is the absolute
// allocs/op below; a post-3b run should land below it and above zero.
//
// Honesty note: this MODELS the production decode rather than calling it. The
// production decoder (wirestream's goferEventWire + its New* dispatch) is
// unexported in another package, and this harness may not modify existing
// files. benchGoferEventWire replicates only the fields the benchmarked kinds
// carry, and must be re-checked if the wire type changes. The re-encode half IS
// the production call (json.Marshal of the same concrete event).
func BenchmarkEventForward(b *testing.B) {
	// Two representative payloads: the smallest and by far most frequent event
	// on a streaming turn (message.delta), and a fat one (tool.call.finished).
	delta, err := json.Marshal(event.NewMessageDelta("11111111-2222-3333-4444-555555555555", "assistant", "Hello, world"))
	if err != nil {
		b.Fatalf("marshal delta fixture: %v", err)
	}
	finished, err := json.Marshal(event.NewToolCallFinishedSpill(
		"11111111-2222-3333-4444-555555555555", "call-1",
		json.RawMessage(`{"path":"/tmp/x","limit":200}`),
		strings.Repeat("result line\n", 40), false, nil, "", 0, "",
	))
	if err != nil {
		b.Fatalf("marshal finished fixture: %v", err)
	}

	fixtures := []struct {
		name string
		raw  json.RawMessage
	}{
		{"message_delta", delta},
		{"tool_call_finished", finished},
	}

	for _, f := range fixtures {
		b.Run(f.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(f.raw)))
			for b.Loop() {
				ev, derr := benchDecodeGoferEvent(f.raw)
				if derr != nil {
					b.Fatal(derr)
				}
				out, merr := json.Marshal(ev)
				if merr != nil {
					b.Fatal(merr)
				}
				if len(out) == 0 {
					b.Fatal("empty re-encode")
				}
			}
		})
	}
}

// benchGoferEventWire mirrors the SHAPE of internal/wirestream's (unexported)
// goferEventWire — the envelope every gofer/event notification carries — for
// the kinds this benchmark decodes. It is a replica, not the production type;
// see BenchmarkEventForward's honesty note.
//
// It is NOT field-for-field identical to production and does not need to be:
// only the fields the benchmarked kinds actually carry affect the measurement.
// Fields outside that set are present to keep the decode shape representative.
// Re-check this type if the wire envelope changes.
type benchGoferEventWire struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`

	Err   string `json:"error"`
	Fatal bool   `json:"fatal"`
	Title string `json:"title"`

	StopReason string         `json:"stop_reason"`
	Usage      provider.Usage `json:"usage"`
	// Pointer, matching production — a value here would decode a null `cost`
	// into a zero struct rather than nil. Neither benchmarked kind carries a
	// cost field, so this does not affect the recorded numbers, but the replica
	// should not diverge from the type it stands in for.
	Cost *provider.Cost `json:"cost"`

	Kind    string            `json:"kind"`
	Text    string            `json:"text"`
	Content string            `json:"content"`
	Meta    map[string]string `json:"meta"`

	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Input       json.RawMessage `json:"input"`
	Delta       string          `json:"delta"`
	Result      string          `json:"result"`
	IsError     bool            `json:"is_error"`
	Diagnostics []string        `json:"diagnostics"`
	SpillPath   string          `json:"spill_path"`
	SpillBytes  int64           `json:"spill_bytes"`
	SpillSHA256 string          `json:"spill_sha256"`
}

// benchDecodeGoferEvent mirrors wirestream's handleGoferEvent decode-and-rebuild
// for the kinds this benchmark exercises.
func benchDecodeGoferEvent(raw json.RawMessage) (event.Event, error) {
	var w benchGoferEventWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, err
	}
	switch w.Type {
	case event.KindMessageDelta:
		return event.NewMessageDelta(w.SessionID, event.MessageKind(w.Kind), w.Text), nil
	case event.KindToolCallFinished:
		return event.NewToolCallFinishedSpill(w.SessionID, w.ID, w.Input, w.Result, w.IsError, w.Diagnostics, w.SpillPath, w.SpillBytes, w.SpillSHA256), nil
	default:
		return nil, fmt.Errorf("bench: unhandled event kind %q", w.Type)
	}
}
