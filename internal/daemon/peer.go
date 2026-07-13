package daemon

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// maxInFlightPerPeer bounds the number of request-handler goroutines one
// connection may have running at once. Unlike serveWS's connection-level
// maxConns (which rejects a new connection outright with 503, since refusing
// a handshake is cheap and immediately visible to the caller), acquiring this
// semaphore BLOCKS: a client that floods a single connection with more than
// maxInFlightPerPeer concurrent requests stalls its own read loop (see run)
// until a handler frees a slot, applying backpressure instead of spawning an
// unbounded number of goroutines and per-request buffers for that one peer. A
// well-behaved client — at most a session/prompt or two in flight — never
// notices the cap.
const maxInFlightPerPeer = 64

// peer is one WebSocket connection's JSON-RPC session: it reads frames in a
// loop, dispatches each request onto its own goroutine (so a long-running
// session/prompt can never block the read loop — a client must be able to
// send session/cancel while one is in flight), and serializes writes behind
// writeMu so concurrent handlers' responses and notifications never
// interleave mid-frame.
type peer struct {
	conn   *websocket.Conn
	daemon *Daemon

	writeMu sync.Mutex

	// wg tracks in-flight request-handler goroutines so run can join them
	// before returning — a handler observes ctx cancellation (the connection
	// context, cancelled on read-loop exit or daemon shutdown) and unwinds
	// rather than leaking.
	wg sync.WaitGroup

	// inFlight is the maxInFlightPerPeer semaphore (see its doc).
	inFlight chan struct{}
}

func newPeer(conn *websocket.Conn, d *Daemon) *peer {
	return &peer{conn: conn, daemon: d, inFlight: make(chan struct{}, maxInFlightPerPeer)}
}

// run reads frames until the connection closes or ctx is cancelled, dispatching
// each to its own goroutine, then joins every in-flight handler before
// returning.
func (p *peer) run(ctx context.Context) {
	// Derive a cancellable context so read-loop exit (a client disconnect)
	// cancels in-flight request handlers BEFORE joining them. Defers run LIFO:
	// wg.Wait is registered first so it runs LAST — after cancel. Without the
	// cancel, a handler blocked on a silent turn (its select on the subscription
	// or ctx.Done) would never observe the disconnect, and wg.Wait would hang
	// until the next event flowed or a write failed.
	ctx, cancel := context.WithCancel(ctx)
	defer p.wg.Wait()
	defer cancel()

	for {
		typ, data, err := p.conn.Read(ctx)
		if err != nil {
			return
		}
		// ACP-over-WebSocket carries one JSON-RPC message per TEXT frame; a
		// binary frame has no defined meaning here and is ignored rather than
		// treated as a protocol error, so a client that also uses binary frames
		// for something else (e.g. a keepalive convention) does not wedge the
		// connection.
		if typ != websocket.MessageText {
			continue
		}

		frame := data

		// Acquire an in-flight slot BEFORE spawning the handler goroutine —
		// see maxInFlightPerPeer's doc. The select on ctx.Done alongside the
		// blocking acquire ensures a peer wedged at the cap during shutdown
		// unblocks via cancellation rather than the read loop (and therefore
		// this whole exit path) hanging forever waiting for a slot that will
		// never free because nothing is reading frames to eventually finish
		// the in-flight handlers.
		select {
		case p.inFlight <- struct{}{}:
		case <-ctx.Done():
			return
		}

		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			defer func() { <-p.inFlight }()
			p.handleFrame(ctx, frame)
		}()
	}
}

// handleFrame parses one JSON-RPC frame and dispatches it. A notification
// (no id) never receives a response, even on error, per JSON-RPC 2.0.
//
// This is the daemon's single per-request logging chokepoint — every inbound
// frame's method, id, and outcome are known here. REDACTION RULE: log ONLY
// method names, JSON-RPC ids, rpc error codes/messages, durations, and
// (elsewhere, at connection accept/close) remote addrs and session ids.
// NEVER log env.Params, a handler's result, or the raw frame bytes — they may
// carry prompt text, message content, or tool inputs/outputs. If in doubt,
// leave it out.
func (p *peer) handleFrame(ctx context.Context, data []byte) {
	log := p.daemon.log
	var env inboundEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		log.Warn("parse error", "err", err)
		p.reply(ctx, nil, nil, parseError(err))
		return
	}
	if env.Method == "" {
		log.Warn("invalid request: missing method")
		if !env.isNotification() {
			p.reply(ctx, env.ID, nil, invalidRequest("missing method"))
		}
		return
	}

	h, ok := methodTable[env.Method]
	if !ok {
		// WARN, not DEBUG: an unrecognized method name is the smoking gun for
		// client-compat debugging (a client speaking a method this daemon
		// version doesn't implement).
		log.Warn("unknown method", "method", env.Method)
		if !env.isNotification() {
			p.reply(ctx, env.ID, nil, methodNotFound(env.Method))
		}
		return
	}

	start := time.Now()
	result, rerr := h(p.daemon, ctx, p, env.Params)
	durMS := time.Since(start).Milliseconds()

	if env.isNotification() {
		log.Debug("notification handled", "method", env.Method, "dur_ms", durMS)
		return
	}

	if rerr != nil {
		log.Info("request handled", "method", env.Method, "id", string(env.ID), "outcome", "error", "code", rerr.Code, "message", rerr.Message, "dur_ms", durMS)
	} else {
		log.Info("request handled", "method", env.Method, "id", string(env.ID), "outcome", "ok", "dur_ms", durMS)
	}
	p.reply(ctx, env.ID, result, rerr)
}

// reply sends a JSON-RPC response for a request. A nil id (parse failure, id
// unknown) is sent as the literal JSON null, per spec.
func (p *peer) reply(ctx context.Context, id json.RawMessage, result any, rerr *rpcError) {
	resp := outboundResponse{JSONRPC: jsonrpcVersion, ID: id}
	if id == nil {
		resp.ID = json.RawMessage("null")
	}
	if rerr != nil {
		resp.Error = rerr
	} else {
		resp.Result = result
	}
	_ = p.writeJSON(ctx, resp)
}

// notify sends a JSON-RPC notification (e.g. session/update) to this peer.
func (p *peer) notify(ctx context.Context, method string, params any) error {
	return p.writeJSON(ctx, outboundNotification{JSONRPC: jsonrpcVersion, Method: method, Params: params})
}

// writeJSON marshals v and writes it as a single WebSocket text frame, holding
// writeMu for the duration so two goroutines (e.g. a session/prompt handler
// streaming notifications and another request's response) can never
// interleave two frames' bytes.
func (p *peer) writeJSON(ctx context.Context, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.conn.Write(ctx, websocket.MessageText, data)
}
