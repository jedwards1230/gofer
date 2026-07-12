package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
)

// ErrNoDaemon indicates [Dial] could not reach ANY daemon at addr — refused,
// timed out, or nothing listening — as distinct from a daemon that IS running
// but rejected the handshake (see [ErrUnauthorized]). A caller deciding
// whether to route a session through a daemon or fall back to an in-process
// path branches on this distinction: use errors.Is(err, ErrNoDaemon) or, more
// simply, [Probe].
var ErrNoDaemon = errors.New("no daemon reachable")

// ErrUnauthorized indicates a daemon IS running at addr but rejected the
// WebSocket upgrade for a missing or incorrect bearer token (see
// [Daemon.authorized]). This is distinct from [ErrNoDaemon]: the connection
// found a live daemon, it just was not let in.
var ErrUnauthorized = errors.New("daemon rejected the connection: unauthorized")

// Client is a JSON-RPC-over-WebSocket client for [Daemon]'s wire protocol: one
// JSON-RPC message per text frame, matching [Daemon.Handler] exactly (see the
// package doc). It serves both gofer's own CLI (gofer/roster, gofer/kill,
// gofer/archive, and driving a session as an ACP client) and, in principle,
// any other client speaking the same protocol.
//
// A Client is safe for concurrent use: [Client.Call] may be outstanding for
// more than one request at once (each keyed by its own id), and
// [Client.Notifications] delivers every inbound notification (e.g.
// session/update) on a single shared channel regardless of which goroutine is
// calling.
//
// Notification-drain contract: the single read loop that demuxes responses
// also delivers notifications, and it BLOCKS on the notifications channel
// (buffer 64) rather than dropping — so nothing that matters for correctness
// (e.g. a session/update stream) is silently lost. The cost is that a caller
// which invokes methods expected to emit notifications MUST drain
// [Client.Notifications] concurrently: if that channel fills and stays full,
// the read loop stops reading, and any [Client.Call] awaiting a response then
// stalls behind it. The rule of thumb: any goroutine issuing a session/prompt
// (or any call that streams) needs a peer goroutine ranging over
// Notifications until it closes (see cmd/gofer's driveDaemonSession). A caller
// making only unary control calls that emit no notifications (gofer/roster,
// gofer/kill, gofer/archive) need not drain — the daemon sends nothing on that
// channel for them.
type Client struct {
	conn *websocket.Conn

	ctx    context.Context
	cancel context.CancelFunc

	writeMu sync.Mutex
	idc     int64

	mu      sync.Mutex
	pending map[string]chan inboundFrame
	closed  bool

	notifications chan Notification
	done          chan struct{}
}

// Notification is one inbound JSON-RPC notification (e.g. a session/update
// push from an in-flight session/prompt): a method with no accompanying id,
// per JSON-RPC 2.0 — it never has a response of its own.
type Notification struct {
	Method string
	Params json.RawMessage
}

// inboundFrame is the shape of any daemon->client frame this Client reads: a
// response (Result/Error set, ID present) or a notification (Method set, ID
// absent). It mirrors [outboundResponse] and [outboundNotification] loosely
// enough to decode either into one struct without knowing in advance which
// arrived.
type inboundFrame struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

// CallError is returned by [Client.Call] when the daemon replies with a
// JSON-RPC error object. It carries the error code alongside the message so a
// caller can branch on it (e.g. a standard JSON-RPC code vs the gofer-owned
// application code, [codeAppError]) without string-matching the message.
type CallError struct {
	Code    int
	Message string
}

func (e *CallError) Error() string { return e.Message }

// Dial opens a WebSocket connection to addr (a bare host:port, matching
// [Config.ListenAddr] — or a full ws://.../wss://... URL) and starts the
// client's read loop. token, if non-empty, is sent as a standard
// "Authorization: Bearer <token>" header, mirroring the header [Daemon.authorized]
// prefers.
//
// Dial distinguishes two failure modes so a caller can decide whether to fall
// back to an in-process path ([ErrNoDaemon]) or surface a credential problem
// ([ErrUnauthorized]) instead of silently falling back: a 401 response means a
// daemon IS listening at addr, it just rejected the token, while any other
// dial failure (connection refused, timeout, DNS failure) means there is no
// daemon to fall back from at all. See [Probe] for the common case of only
// needing the yes/no answer.
func Dial(ctx context.Context, addr, token string) (*Client, error) {
	header := http.Header{}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}

	conn, resp, err := websocket.Dial(ctx, wsURL(addr), &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("%w: %s", ErrUnauthorized, addr)
		}
		return nil, fmt.Errorf("%w: %s: %w", ErrNoDaemon, addr, err)
	}
	conn.SetReadLimit(maxMessageBytes)

	cctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		conn:          conn,
		ctx:           cctx,
		cancel:        cancel,
		pending:       make(map[string]chan inboundFrame),
		notifications: make(chan Notification, 64),
		done:          make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// wsURL builds the WebSocket URL Dial connects to: addr passed through
// verbatim if it already names a scheme, else prefixed ws:// — the daemon
// speaks plain ws:// only (see [Daemon]'s package doc); a caller fronting it
// with TLS passes a full wss://... addr instead.
func wsURL(addr string) string {
	if strings.Contains(addr, "://") {
		return addr
	}
	return "ws://" + addr
}

// Probe reports whether a daemon is reachable at addr: it dials, and if that
// succeeds (or fails only on auth — see [Dial]) closes the connection and
// returns true. An auth failure still counts as "reachable" because a caller
// uses Probe to decide whether to route a session through the daemon at all
// — a wrong token is the caller's problem to fix (or fall back on
// deliberately), not evidence that no daemon exists.
func Probe(ctx context.Context, addr, token string) bool {
	c, err := Dial(ctx, addr, token)
	if err != nil {
		return errors.Is(err, ErrUnauthorized)
	}
	_ = c.Close()
	return true
}

// Notifications returns the channel every inbound notification (session/update
// pushes, chiefly) is delivered on. It is closed when the connection closes —
// ranging over it until it closes is the idiomatic way to drain it.
func (c *Client) Notifications() <-chan Notification { return c.notifications }

// Call sends a JSON-RPC request for method with params and blocks for its
// matching response, returning the raw result on success or a *[CallError] for
// a JSON-RPC error reply. ctx cancellation unregisters the pending call and
// returns ctx.Err() — the daemon may still be processing the request server
// side (Call has no way to abort it short of closing the connection or, for
// session/prompt specifically, sending session/cancel).
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := atomic.AddInt64(&c.idc, 1)
	idJSON, err := json.Marshal(id)
	if err != nil {
		return nil, fmt.Errorf("daemon client: marshal request id: %w", err)
	}
	key := string(idJSON)

	ch := make(chan inboundFrame, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("daemon client: %s: connection closed", method)
	}
	c.pending[key] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
	}()

	req := struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int64  `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{jsonrpcVersion, id, method, params}
	if err := c.writeJSON(ctx, req); err != nil {
		return nil, fmt.Errorf("daemon client: write %s: %w", method, err)
	}

	select {
	case f, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("daemon client: %s: connection closed before a response arrived", method)
		}
		if f.Error != nil {
			return nil, &CallError{Code: f.Error.Code, Message: f.Error.Message}
		}
		return f.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, fmt.Errorf("daemon client: %s: connection closed before a response arrived", method)
	}
}

// Notify sends a JSON-RPC notification (no id, no response expected) — used
// for session/cancel, the one notification gofer's CLI sends.
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	n := struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{jsonrpcVersion, method, params}
	if err := c.writeJSON(ctx, n); err != nil {
		return fmt.Errorf("daemon client: notify %s: %w", method, err)
	}
	return nil
}

// writeJSON marshals v and writes it as a single WebSocket text frame, holding
// writeMu for the duration so two goroutines calling Call/Notify concurrently
// can never interleave two frames' bytes — mirroring [peer.writeJSON].
func (c *Client) writeJSON(ctx context.Context, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.Write(ctx, websocket.MessageText, data)
}

// readLoop reads frames until the connection closes, demuxing each into
// either its matching pending response channel or the shared notifications
// channel — the same shape as [peer]'s read loop and the daemon test suite's
// wsClient, just running in the opposite direction. It is this Client's only
// reader.
func (c *Client) readLoop() {
	defer close(c.done)
	defer close(c.notifications)
	for {
		typ, data, err := c.conn.Read(c.ctx)
		if err != nil {
			c.mu.Lock()
			c.closed = true
			for _, ch := range c.pending {
				close(ch)
			}
			c.pending = nil
			c.mu.Unlock()
			return
		}
		if typ != websocket.MessageText {
			continue
		}

		var f inboundFrame
		if err := json.Unmarshal(data, &f); err != nil {
			// The daemon only ever sends well-formed JSON-RPC; a decode
			// failure here would be a protocol drift, not a client bug worth
			// tearing the connection down over. Drop the frame.
			continue
		}

		if f.Method != "" && len(f.ID) == 0 {
			select {
			case c.notifications <- Notification{Method: f.Method, Params: f.Params}:
			case <-c.ctx.Done():
				return
			}
			continue
		}

		c.mu.Lock()
		ch, ok := c.pending[string(f.ID)]
		if ok {
			delete(c.pending, string(f.ID))
		}
		c.mu.Unlock()
		if ok {
			ch <- f
		}
	}
}

// Close shuts the connection down and waits for the read loop to exit, so
// Notifications is guaranteed closed by the time Close returns. Safe to call
// more than once: cancel and conn.Close are both idempotent, and the second
// call's <-c.done returns immediately since the first already closed it.
func (c *Client) Close() error {
	c.cancel()
	err := c.conn.Close(websocket.StatusNormalClosure, "")
	<-c.done
	return err
}
