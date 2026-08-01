package daemon_test

// broadcast_bench_test.go measures allocations on the REAL event-forwarding
// path — [daemon.Daemon.BroadcastRawEvent] fanning a worker's gofer/event frame
// out to N attached peers — by calling production code, not a model of it.
//
// # Why it lives here, and why it carries no build tag
//
// It is in internal/daemon because that is where the code under test is: the
// router's sink (internal/router/router.go's eventSink) does nothing but hand
// the frame to this method, so the router adds no measurable work to forward.
//
// It carries NO build tag, unlike internal/router/bench_test.go's workerbench
// harness, because it needs none: it spawns no processes, shells out to nothing,
// and runs entirely in-process over an httptest server. A benchmark function is
// not executed by `go test ./...` at all (only by `-bench`), so leaving it
// untagged costs CI nothing while making CI COMPILE it on every push. That is
// the point — the benchmark this one replaces was able to keep reporting
// numbers for a path Slice 3b deleted precisely because a tagged file is not
// built by default and so never fails when the code it models moves on.
//
// # What is and is not attributed here
//
// The measured window contains the daemon's whole fan-out: for each attached
// peer, [peer.writeJSON] marshals a JSON-RPC notification envelope around the
// verbatim params and writes one WebSocket frame. It does NOT contain any
// decode or re-encode of the event itself — that is the work Slice 3b removed,
// and its absence is what this benchmark exists to show.
//
// Go's allocation accounting is process-wide, so the in-process peers' own
// frame reads land in the window too. The drain loops below are therefore kept
// as lean as possible (raw byte copy into a reused buffer, no JSON decode), and
// every figure here should be read as an UPPER BOUND on the daemon-side cost: a
// real client's read allocations are paid on another machine.
//
// That same process-wide accounting is why one iteration broadcasts
// [broadcastsPerOp] times rather than once. Divide any reported figure by that
// constant for the per-broadcast cost; the constant's doc has the sizing rule
// and the evidence that batching changed the scale and not the measurement.
//
// The ACP projection ([daemon.Daemon.BroadcastSessionUpdate], the lossy half the
// router calls alongside this one for pure-ACP peers) is deliberately out of
// scope: it is a different hop with a different cost, and folding it in would
// reintroduce exactly the conflation this file is replacing.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// benchSessionID is the fixed session id the payload fixtures carry. The
// broadcast is keyed by the id passed to BroadcastRawEvent, not by anything
// inside the frame, so the fixture's own id only has to be realistic.
const benchSessionID = "11111111-2222-3333-4444-555555555555"

// broadcastsPerOp is how many fan-outs ONE benchmark iteration performs. It is
// a signal-to-noise measure, not a workload choice, and it exists for the same
// reason internal/supervisor's liveCallsPerOp does.
//
// testing.B derives allocs/op and B/op from runtime.ReadMemStats, whose
// Mallocs counter is PROCESS-WIDE: every allocation by every goroutine inside
// the timed region is charged to the benchmark. This fixture holds one drain
// goroutine per attached peer, each blocked in p.conn.Reader, and a read
// allocates. At one broadcast per iteration, whether that single frame's
// peer-side reads landed inside the window was decided by the scheduler alone.
// Three independent measurements of message_delta/peers=1, every one of them on
// IDENTICAL code:
//
//	unmodified main, 40 runs           15-18   baseline of 15 came back 18 times
//	this file, batching removed        15-22   the N=1 column below
//	ubuntu-latest CI                      19   the failure
//
// — against a baseline of 15 whose 25% gate tolerance is 3 allocations. The two
// local ranges differ because the second was measured on a busier machine; the
// gate does not care which it gets, only that its 18.75 threshold sits inside
// all three. CI duly failed gofer#332 at 19, a pull request that touched no file
// in this package, and gofer#334 is where that was run down.
//
// WHY 128. The stray is a small ABSOLUTE number that does not scale with the
// measured work, so raising the work raises the margin and leaves the noise
// where it is. Measured here (darwin/arm64, M2 Pro, 30 runs at each factor,
// nothing else on the machine), as max-minus-min allocations per row. The N=1
// column is this same file with the batching and the quiesce taken back out —
// what the gate saw when #334 fired:
//
//	row                              N=1   N=64   N=128   margin at 128
//	message_delta/peers=1             +7     +4      +5             483
//	message_delta/peers=8             +9    +29     +24           2,055
//	message_delta/peers=32           +33    +37     +23           7,435
//	tool_call_finished/peers=1        +7     +6      +4             484
//	tool_call_finished/peers=8        +8    +12     +24           2,053
//	tool_call_finished/peers=32      +11    +53     +55           7,437
//
// The stray barely moves across a 128x change in the work while the margin
// tracks the work exactly, which is the whole mechanism stated as data rather
// than assumed. Note what the N=1 column means against the old baselines of 15
// and 19: a tolerance of 3 allocations against a stray of +7 is not a gate, it
// is a coin toss, and #334 is what losing it looks like.
//
// At 128 the weakest row clears its own worst stray by 85x. Even pairing the
// SMALLEST margin (483) with the LARGEST stray seen on ANY row (+55) — a pairing
// no scheduler can produce, since the big strays belong to the rows that read
// 4,096 frames per iteration — leaves 8.8x. On its own data 64 cleared that
// pessimistic pairing by 4.6x: over the bar with nothing to spare, and under it
// the moment a broadcast gets any cheaper. That is what decided 128 over it.
//
// THE RULE, so the next person does not have to re-derive it: the weakest row's
// batched count must stay at or above 16x the largest stray (a 25% tolerance is
// 4x a stray only when the count is 16x it). Against the +55 measured here that
// floor is ~880 allocations, and the weakest row sits at 1,934. What invalidates
// it is anything that makes a broadcast CHEAPER — the margin is sized against
// the signal while the noise stays put, which is exactly how the supervisor's
// factor went stale when the SDK cut its per-read cost. At 128 the per-broadcast
// cost can fall from 15.1 to 6.9 allocations before the floor is reached; below
// that, re-measure and raise this constant rather than discovering it on CI.
//
// DIVIDING BACK. Batching changes the scale, not the measurement — checkable,
// and checked. Every row divides by this constant back to the count the same
// code reports at one broadcast per iteration:
//
//	row                          N=128    /128     unbatched
//	message_delta/peers=1        1,934   15.11            15
//	tool_call_finished/peers=1   1,937   15.13            15
//	message_delta/peers=8        8,220   64.22            64
//	tool_call_finished/peers=8   8,213   64.16            64
//	message_delta/peers=32      29,740  232.34           232
//	tool_call_finished/peers=32 29,751  232.43           232
//
// The residual is +0.11 to +0.43 and one-sided, which is [quiesce]'s constant
// spread over 128 broadcasts — not a distortion of the measurement.
//
// Read the right-hand column again: at every peer count the two payload shapes
// cost the SAME. That is the equality this benchmark exists to show — a path
// that forwards bytes verbatim cannot care how many fields the event has — and
// it is now exact rather than approximate, because [assertBenchDelivery] warms
// with the payload the sub-benchmark measures instead of a fixed slim one.
//
// Each iteration is therefore N independent repetitions of the measured
// operation, not one bigger operation: BroadcastRawEvent takes a fresh
// relayWriteTimeout budget per call, so nothing is shared or amortised across
// the batch except that one-time cost. And one iteration still does a
// DETERMINISTIC amount of work, which is what keeps allocation counts exact at
// scripts/bench.sh's -benchtime 1x. This is not raising -benchtime.
const broadcastsPerOp = 128

// BenchmarkBroadcastRawEvent measures allocations on the forwarding path a
// worker-hosted turn actually takes, at several attached-peer counts. One
// iteration is [broadcastsPerOp] events, so every reported figure divides by
// that constant for the per-event cost.
//
// Two payload shapes are used, the same two the pre-Slice-3b baseline recorded:
// the small, overwhelmingly
// most frequent event on a streaming turn (message.delta) and a fat one with
// spill fields (tool.call.finished). Comparing the two is the sharpest read
// available here — the removed decode+re-encode cost MORE for the fatter event
// (14 vs 17 allocs/op) because it interpreted every field. A path that forwards
// bytes verbatim cannot care how many fields the event has, so the two shapes
// should cost the same in allocs/op. That equality is the evidence, not a ratio
// against the old model's numbers — and it holds: 15.11 vs 15.13 per event at
// peers=1, 64.22 vs 64.16 at peers=8, 232.34 vs 232.43 at peers=32. It did not
// hold before this benchmark stopped warming with a slim probe and started
// batching; see [broadcastsPerOp] and [assertBenchDelivery] for both halves.
func BenchmarkBroadcastRawEvent(b *testing.B) {
	delta, err := json.Marshal(event.NewMessageDelta(benchSessionID, "assistant", "Hello, world"))
	if err != nil {
		b.Fatalf("marshal message.delta fixture: %v", err)
	}
	finished, err := json.Marshal(event.NewToolCallFinishedSpill(
		benchSessionID, "call-1",
		json.RawMessage(`{"path":"/tmp/x","limit":200}`),
		strings.Repeat("result line\n", 40), false, nil, "", 0, "",
	))
	if err != nil {
		b.Fatalf("marshal tool.call.finished fixture: %v", err)
	}

	payloads := []struct {
		name string
		raw  json.RawMessage
	}{
		{"message_delta", delta},
		{"tool_call_finished", finished},
	}
	// 1 / 8 / 32: enough spread that a per-peer term is visible as a slope
	// rather than inferred from a single point.
	peerCounts := []int{1, 8, 32}

	for _, p := range payloads {
		for _, peers := range peerCounts {
			b.Run(fmt.Sprintf("%s/peers=%d", p.name, peers), func(b *testing.B) {
				d, sid := newBroadcastBenchFixture(b, peers, p.raw)

				quiesce()

				b.ReportAllocs()
				for b.Loop() {
					for range broadcastsPerOp {
						d.BroadcastRawEvent(sid, p.raw)
					}
				}
			})
		}
	}
}

// newBroadcastBenchFixture stands up a real daemon over a real supervisor, opens
// a live session, and attaches peers real WebSocket clients to it via
// session/load — the same path a TUI or phone takes. It returns the daemon and
// the session id to broadcast on, with every peer already draining.
//
// probe is the frame [assertBenchDelivery] sends, and it must be the SAME
// payload the caller is about to measure — see that function for why.
func newBroadcastBenchFixture(b *testing.B, peers int, probe json.RawMessage) (*daemon.Daemon, string) {
	b.Helper()

	sup := newTestSupervisor(b, fauxProvider)
	d, url := newTestDaemon(b, sup, "")
	cwd := b.TempDir()

	// session/new does NOT attach the creating peer to the fan-out set (only
	// session/load does), so this connection creates the session and then plays
	// no further part.
	creator := dialBenchPeer(b, url)
	var newResp struct {
		SessionID string `json:"sessionId"`
	}
	creator.call(b, acp.MethodSessionNew, acp.NewSessionRequest{Cwd: cwd}, &newResp)
	if newResp.SessionID == "" {
		b.Fatal("session/new returned an empty sessionId")
	}

	attached := make([]*benchPeer, 0, peers)
	for range peers {
		p := dialBenchPeer(b, url)
		p.call(b, acp.MethodSessionLoad, acp.LoadSessionRequest{SessionID: newResp.SessionID, Cwd: cwd}, nil)
		p.drain()
		attached = append(attached, p)
	}
	waitBenchPeerCount(b, d, newResp.SessionID, peers)
	// Registration is not delivery: prove a frame actually reaches every peer
	// before measuring the cost of sending them.
	assertBenchDelivery(b, d, newResp.SessionID, attached, probe)
	return d, newResp.SessionID
}

// waitBenchPeerCount polls the daemon's fan-out registry until the expected
// number of peers is attached. Attachment completes inside session/load, so this
// is normally satisfied on the first read; polling covers the registry lock
// rather than a race.
func waitBenchPeerCount(b *testing.B, d *daemon.Daemon, sessionID string, want int) {
	b.Helper()
	deadline := time.Now().Add(defaultWait)
	for {
		if got := d.PeersForSessionCount(sessionID); got == want {
			return
		} else if time.Now().After(deadline) {
			b.Fatalf("peer count for %s = %d, want %d (timed out)", sessionID, got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// benchPeer is a deliberately minimal JSON-RPC-over-WebSocket client, separate
// from the harness's [wsClient] because that one decodes every inbound frame
// into an rpcFrame. Under a fan-out benchmark those decodes are pure
// measurement noise that scales with peer count — exactly the axis being
// measured. This client reads setup responses with a decode and everything
// afterwards as raw bytes into a reused buffer.
type benchPeer struct {
	conn *websocket.Conn
	ctx  context.Context
	idc  atomic.Int64
	// frames counts inbound frames seen by [benchPeer.drain]; read by
	// [assertBenchDelivery] to prove the fan-out actually delivers.
	frames atomic.Int64
}

func dialBenchPeer(b *testing.B, url string) *benchPeer {
	b.Helper()
	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		b.Fatalf("dial %s: %v", url, err)
	}
	b.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	return &benchPeer{conn: conn, ctx: ctx}
}

// call issues a JSON-RPC request and blocks for its matching response,
// unmarshaling the result into out when out is non-nil. Frames that are not
// that response (a replayed notification) are skipped. It is setup-only — it
// must not be called once [benchPeer.drain] owns the connection's reads.
func (p *benchPeer) call(b *testing.B, method string, params any, out any) {
	b.Helper()
	id := p.idc.Add(1)
	req := struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int64  `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{"2.0", id, method, params}
	data, err := json.Marshal(req)
	if err != nil {
		b.Fatalf("marshal %s request: %v", method, err)
	}
	if werr := p.conn.Write(p.ctx, websocket.MessageText, data); werr != nil {
		b.Fatalf("write %s: %v", method, werr)
	}

	ctx, cancel := context.WithTimeout(p.ctx, defaultWait)
	defer cancel()
	for {
		_, raw, rerr := p.conn.Read(ctx)
		if rerr != nil {
			b.Fatalf("read %s response: %v", method, rerr)
		}
		var resp struct {
			ID     *int64          `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if uerr := json.Unmarshal(raw, &resp); uerr != nil || resp.ID == nil || *resp.ID != id {
			continue // a notification, or some other request's response
		}
		if resp.Error != nil {
			b.Fatalf("%s error: %d %s", method, resp.Error.Code, resp.Error.Message)
		}
		if out != nil {
			if uerr := json.Unmarshal(resp.Result, out); uerr != nil {
				b.Fatalf("unmarshal %s result: %v", method, uerr)
			}
		}
		return
	}
}

// drain consumes inbound frames forever, discarding them without decoding but
// COUNTING them. A peer that does not drain would fill its socket buffer and
// turn the fan-out into a measurement of [relayWriteTimeout] instead of of the
// write path. The goroutine exits when the connection closes at benchmark
// cleanup.
//
// The count exists so the fixture can prove frames actually arrive — see
// [assertBenchDelivery]. Incrementing an atomic per frame is far cheaper than
// the decode this client deliberately avoids.
//
// [broadcastsPerOp] multiplies what these loops must absorb — 4,096 frames per
// iteration at peers=32 — so this is the place backpressure would show up if it
// were going to. It does not: per-frame wall clock is unchanged from the
// unbatched benchmark (9.2 microseconds at peers=32 before, 8.6-13 after), which
// it could not be if writes were queueing behind a full socket buffer, let alone
// hitting relayWriteTimeout.
func (p *benchPeer) drain() {
	go func() {
		buf := make([]byte, 32*1024)
		for {
			_, r, err := p.conn.Reader(p.ctx)
			if err != nil {
				return
			}
			if _, err := io.CopyBuffer(io.Discard, r, buf); err != nil {
				return
			}
			p.frames.Add(1)
		}
	}()
}

// assertBenchDelivery broadcasts one probe frame and blocks until every peer has
// received something, failing the benchmark if any peer does not.
//
// This closes the hole that this whole task exists to fix. [Daemon.BroadcastRawEvent]
// logs peer-notify failures at Debug and SWALLOWS them, so without this check a
// change that made every write fail would leave the benchmark completing
// happily, reporting a plausible per-peer slope for work that never happened —
// the same class of defect as the modelled benchmark this one replaces, just
// one level subtler. Measuring a fan-out without proving the fan-out occurred
// is not a measurement.
//
// It runs during fixture setup, before the measured loop, so it costs the
// reported numbers nothing.
//
// THE PROBE MUST BE THE PAYLOAD THE CALLER MEASURES, which is why it is passed
// in rather than built here. It used to be a fixed, small message.delta for
// every sub-benchmark, and that made this a warm-up for a cousin of the measured
// call rather than for the call itself — the defect 624057b fixed next door and
// docs/TESTING.md writes down as doctrine, in payload form. The consequence was
// measurable: with a slim probe, the fat-payload rows read ~2 allocations higher
// than the slim ones at peers=1 and peers=8 (17 vs 15, 66 vs 64) because the
// first write of a FAT frame in the process happened inside the timed window and
// its one-time cost was charged there. Batching hides that (it amortises to
// 0.02/op at 128) rather than fixing it, and a one-time cost baked into a
// committed baseline is what the baseline exists to prevent.
func assertBenchDelivery(b *testing.B, d *daemon.Daemon, sessionID string, peers []*benchPeer, probe json.RawMessage) {
	b.Helper()
	d.BroadcastRawEvent(sessionID, probe)

	deadline := time.Now().Add(defaultWait)
	for {
		delivered := 0
		for _, p := range peers {
			if p.frames.Load() > 0 {
				delivered++
			}
		}
		if delivered == len(peers) {
			return
		}
		if time.Now().After(deadline) {
			b.Fatalf("delivery probe reached %d of %d peers; the fan-out is not delivering, so any number this benchmark reports is meaningless", delivered, len(peers))
		}
		time.Sleep(time.Millisecond)
	}
}

// quiesce retires the PREVIOUS sub-benchmark's teardown before the next one
// starts measuring. Six sub-benchmarks run back to back here, and b.Cleanup
// closes a daemon, a supervisor session and up to 33 WebSocket connections as
// each one ends — so the next would otherwise open its window on a heap full of
// that garbage and its pending finalizers and, because the counters are
// process-wide, be charged for collecting it. Two cycles: the first queues
// finalizers, the second runs them.
//
// It is called BEFORE b.Loop(), which performs its own b.ResetTimer() on the
// first call (testing.B.loopSlowPath), so the GCs are outside the measurement.
//
// It does move the numbers, by a constant ~12 allocations per window (measured:
// message_delta/peers=1 reads 962-964 without it and 970-979 with it at
// broadcastsPerOp=64). That is not the GC being charged — runtime.GC allocates
// nothing — it is sync.Pool: a GC empties every pool, so the pooled buffers
// coder/websocket reuses across writes must be re-allocated on the first frames
// after the quiesce, inside the window. Trading a scheduling-dependent stray for
// a constant one is the point, and at 128 broadcasts it is 0.6% of the figure.
func quiesce() {
	runtime.GC()
	runtime.GC()
}
