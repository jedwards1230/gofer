package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/jedwards1230/agent-sdk-go/acp"
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

	// attachedMu guards attached. It is a small per-peer lock, distinct from
	// the daemon's registry lock — see [Daemon.attachPeer]'s lock-ordering note.
	attachedMu sync.Mutex
	// attached is the set of session ids this peer is registered for in the
	// daemon's fan-out registry (see [Daemon.sessionPeers]). Tracking it on the
	// peer makes deregister-on-close O(attached) rather than O(all sessions):
	// [Daemon.detachPeer] walks exactly this set.
	attached map[string]struct{}

	// goferNative reports whether this peer speaks the gofer-native control
	// surface (it has invoked a gofer/* method or permission.reply) rather than
	// being a pure ACP client. It gates the daemon's spec-ACP
	// session/request_permission fan-out: that request is a JSON-RPC REQUEST a
	// client must answer, which a gofer-native client (internal/daemonbridge)
	// does not — it answers via the permission.reply notification and consumes
	// gofer/permission_requested instead. Default false (assume ACP) is the safe
	// direction: an ACP client (which never calls a gofer/* method) is never
	// mismarked and so is never skipped, while a gofer-native client that has not
	// yet been marked merely receives a request it silently drops (harmless — it
	// still answers via permission.reply). See [Daemon.requestPermissionFromPeers].
	goferNative atomic.Bool

	// outIDC generates ids for daemon-initiated requests (session/request_permission)
	// this peer sends to its client. Separate id space from the client's own
	// request ids — the two never collide because they key different maps
	// (pendingOut here vs the client's own pending table).
	outIDC atomic.Int64
	// pendingMu guards pendingOut.
	pendingMu sync.Mutex
	// pendingOut maps a daemon-initiated request's marshaled id to the channel
	// its response is delivered on (see [peer.request]/[peer.deliverReply]).
	// Closed entries on connection teardown unblock any in-flight [peer.request].
	pendingOut map[string]chan peerReply
}

// peerReply is the outcome of a daemon-initiated request: exactly one of Result
// or Err is set, mirroring a JSON-RPC response.
type peerReply struct {
	Result json.RawMessage
	Err    *rpcError
}

func newPeer(conn *websocket.Conn, d *Daemon) *peer {
	return &peer{
		conn:       conn,
		daemon:     d,
		inFlight:   make(chan struct{}, maxInFlightPerPeer),
		attached:   make(map[string]struct{}),
		pendingOut: make(map[string]chan peerReply),
	}
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
	// Registered FIRST so it runs LAST (defers are LIFO): the peer is removed
	// from the daemon's fan-out registry only after cancel has unblocked every
	// in-flight handler and wg.Wait has joined them, so no handler of THIS peer
	// is still broadcasting when it leaves the registry. A concurrent broadcast
	// from ANOTHER peer's handler that snapshotted this peer before it detached
	// is harmless — its notify just errors on the closing connection and is
	// logged and skipped (see handleSessionPrompt).
	defer p.daemon.detachPeer(p)
	defer p.closePending()
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
	// A response to a daemon-initiated request (session/request_permission): route
	// it to the waiting [peer.request] and stop — it is NOT a method call.
	if env.isResponse() {
		p.deliverReply(env.ID, peerReply{Result: env.Result, Err: env.Error})
		return
	}
	if env.Method == "" {
		log.Warn("invalid request: missing method")
		if !env.isNotification() {
			p.reply(ctx, env.ID, nil, invalidRequest("missing method"))
		}
		return
	}

	// A peer that drives the gofer-native control surface (any gofer/* method or
	// permission.reply) is not a pure ACP client — mark it so the spec-ACP
	// session/request_permission fan-out skips it (see the goferNative field).
	if isGoferNativeMethod(env.Method) {
		p.goferNative.Store(true)
	}

	h, ok := methodTable[env.Method]
	if !ok {
		// WARN, not DEBUG: an unrecognized method name is the smoking gun for
		// client-compat debugging (a client speaking a method this daemon
		// version doesn't implement).
		log.Warn("unknown method", "method", env.Method, "id", string(env.ID))
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
		// A failing read is always worth seeing, even for a high-frequency
		// polled method — stays at INFO regardless of isHighFrequencyRead.
		log.Info("request handled", "method", env.Method, "id", string(env.ID), "outcome", "error", "code", rerr.Code, "message", rerr.Message, "dur_ms", durMS)
	} else {
		// A --log-level info log needs to stay readable with a TUI attached:
		// the TUI polls gofer/roster (and a CLI client may poll gofer/ps or
		// session/list) at ~1Hz, and logging every one of those at INFO would
		// drown out everything else on an otherwise quiet daemon. Demote only
		// the ok outcome of these specific high-frequency, read-only methods
		// to DEBUG; every other method's ok log — and any of these methods'
		// error outcome, above — stays at INFO.
		level := slog.LevelInfo
		if isHighFrequencyRead(env.Method) {
			level = slog.LevelDebug
		}
		log.Log(ctx, level, "request handled", "method", env.Method, "id", string(env.ID), "outcome", "ok", "dur_ms", durMS)
	}
	p.reply(ctx, env.ID, result, rerr)
}

// isHighFrequencyRead reports whether method is a read-only, high-frequency
// (polled roughly at UI refresh rate) request whose successful outcome should
// log at DEBUG rather than INFO — see handleFrame.
func isHighFrequencyRead(method string) bool {
	switch method {
	case methodGoferRoster, methodGoferPS, acp.MethodSessionList:
		return true
	default:
		return false
	}
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

// isGoferNativeMethod reports whether method belongs to the gofer-native control
// surface — any gofer/* method or the permission.reply op. A peer that invokes
// one is a gofer client (internal/daemonbridge), not a pure ACP one, and is
// excluded from the spec-ACP session/request_permission fan-out (see
// [peer.goferNative]). ACP method names never carry the gofer/ prefix, so an
// ACP client can never trip this.
func isGoferNativeMethod(method string) bool {
	return strings.HasPrefix(method, "gofer/") || method == methodPermissionReply
}

// request sends a daemon-initiated JSON-RPC request (session/request_permission)
// to this peer's client and blocks for its matching response. It is the mirror
// of [Client.Call], running in the agent->client direction: the daemon here is
// the requester and the connected client answers. The response is routed back
// by [peer.handleFrame] via [peer.deliverReply].
//
// ctx cancellation (the permission resolved by another path, the driving turn
// ended, or the daemon shut down) unregisters the waiter and returns ctx.Err —
// the client may still answer later, which [peer.deliverReply] then finds no
// waiter for and drops. A connection teardown closes the waiter channel (see
// [peer.closePending]) and returns a clear error.
//
// The request FRAME is written under the daemon's own context, NOT ctx:
// coder/websocket's Write closes the connection if its context is cancelled
// mid-write, and ctx here is cancelled on the ordinary resolve path (another
// peer answered) — writing under it would tear an innocent peer's connection
// down in the race between this write and that resolution. The daemon context
// is cancelled only on shutdown, when closing is appropriate; a write to an
// already-closed peer connection still fails fast on its own. ctx governs only
// the RESPONSE wait below.
func (p *peer) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := p.outIDC.Add(1)
	idJSON, err := json.Marshal(id)
	if err != nil {
		return nil, fmt.Errorf("peer request %s: marshal id: %w", method, err)
	}
	key := string(idJSON)

	ch := make(chan peerReply, 1)
	p.pendingMu.Lock()
	p.pendingOut[key] = ch
	p.pendingMu.Unlock()
	defer func() {
		p.pendingMu.Lock()
		delete(p.pendingOut, key)
		p.pendingMu.Unlock()
	}()

	req := outboundRequest{JSONRPC: jsonrpcVersion, ID: idJSON, Method: method, Params: params}
	if err := p.writeJSON(p.daemon.ctx, req); err != nil {
		return nil, fmt.Errorf("peer request %s: write: %w", method, err)
	}

	select {
	case r, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("peer request %s: connection closed before a response arrived", method)
		}
		if r.Err != nil {
			return nil, fmt.Errorf("peer request %s: %s", method, r.Err.Message)
		}
		return r.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// deliverReply routes a client's response to the [peer.request] that is waiting
// on id, if any. A response with no registered waiter — the request already
// unregistered on ctx cancellation (resolved elsewhere), or a duplicate — is
// dropped, which is exactly the first-answer-wins no-op the approval relay
// relies on.
func (p *peer) deliverReply(id json.RawMessage, r peerReply) {
	p.pendingMu.Lock()
	ch, ok := p.pendingOut[string(id)]
	if ok {
		delete(p.pendingOut, string(id))
	}
	p.pendingMu.Unlock()
	if ok {
		ch <- r // buffered (cap 1); never blocks
	}
}

// closePending closes every outstanding daemon-initiated request's waiter on
// connection teardown, so an in-flight [peer.request] blocked on this now-gone
// client unblocks with a clear "connection closed" error rather than lingering
// until its ctx happens to cancel. Run from [peer.run]'s deferred cleanup.
func (p *peer) closePending() {
	p.pendingMu.Lock()
	for key, ch := range p.pendingOut {
		close(ch)
		delete(p.pendingOut, key)
	}
	p.pendingMu.Unlock()
}
