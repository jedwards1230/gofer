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
// The ACP projection ([daemon.Daemon.BroadcastSessionUpdate], the lossy half the
// router calls alongside this one for pure-ACP peers) is deliberately out of
// scope: it is a different hop with a different cost, and folding it in would
// reintroduce exactly the conflation this file is replacing.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// BenchmarkBroadcastRawEvent measures per-event allocations on the forwarding
// path a worker-hosted turn actually takes, at several attached-peer counts.
//
// Two payload shapes are used, the same two the pre-Slice-3b baseline recorded:
// the small, overwhelmingly
// most frequent event on a streaming turn (message.delta) and a fat one with
// spill fields (tool.call.finished). Comparing the two is the sharpest read
// available here — the removed decode+re-encode cost MORE for the fatter event
// (14 vs 17 allocs/op) because it interpreted every field. A path that forwards
// bytes verbatim cannot care how many fields the event has, so the two shapes
// should cost the same in allocs/op. That equality is the evidence, not a ratio
// against the old model's numbers.
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
				d, sid := newBroadcastBenchFixture(b, peers)
				b.ReportAllocs()
				for b.Loop() {
					d.BroadcastRawEvent(sid, p.raw)
				}
			})
		}
	}
}

// newBroadcastBenchFixture stands up a real daemon over a real supervisor, opens
// a live session, and attaches peers real WebSocket clients to it via
// session/load — the same path a TUI or phone takes. It returns the daemon and
// the session id to broadcast on, with every peer already draining.
func newBroadcastBenchFixture(b *testing.B, peers int) (*daemon.Daemon, string) {
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
	assertBenchDelivery(b, d, newResp.SessionID, attached)
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
func assertBenchDelivery(b *testing.B, d *daemon.Daemon, sessionID string, peers []*benchPeer) {
	b.Helper()
	probe, err := json.Marshal(event.NewMessageDelta(benchSessionID, "assistant", "probe"))
	if err != nil {
		b.Fatalf("marshal delivery probe: %v", err)
	}
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
