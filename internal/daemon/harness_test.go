package daemon_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/runner"
	"github.com/jedwards1230/agent-sdk-go/session"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// defaultWait bounds every blocking wait in this package's tests: generous
// enough for CI, short enough that a real regression fails fast rather than
// hanging the suite.
const defaultWait = 5 * time.Second

// newTestSupervisor builds a Supervisor whose sessions are real
// [runner.Runner]s (real journal, real event broker, real loop) driven by a
// test-scripted [provider.Provider] — no network, fully deterministic.
// newProvider is called once per Create/Resume so each session gets its own
// provider instance (important for [blockingProvider], whose state is
// per-turn).
func newTestSupervisor(t *testing.T, newProvider func() provider.Provider) *supervisor.Supervisor {
	t.Helper()
	return newTestSupervisorAtRoot(t, t.TempDir(), newProvider)
}

// newTestSupervisorAtRoot is [newTestSupervisor] with an explicit store root
// instead of a fresh t.TempDir() — the seam a daemon-restart test uses to
// build a second Supervisor over the exact same on-disk root once the first
// is closed.
func newTestSupervisorAtRoot(t *testing.T, root string, newProvider func() provider.Provider) *supervisor.Supervisor {
	t.Helper()
	return newTestSupervisorModelAtRoot(t, root, "faux", newProvider)
}

// newTestSupervisorModelAtRoot is [newTestSupervisorAtRoot] with the session
// model pinned to a caller-chosen id instead of the unregistered "faux" — the
// seam a test needs when it must exercise the SDK's registered-model paths
// (context-window sizing + usage pricing), which the faux model can't reach
// (faux is not in the provider registry, so [provider.Lookup]/[provider.CostOf]
// miss and TurnFinished carries ContextWindow 0). See TestSessionPromptUsageUpdate.
func newTestSupervisorModelAtRoot(t *testing.T, root, model string, newProvider func() provider.Provider) *supervisor.Supervisor {
	t.Helper()
	store, err := session.NewFileStore(session.WithRoot(root))
	if err != nil {
		t.Fatalf("session.NewFileStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := supervisor.Config{
		Root:  root,
		Store: store,
		NewSession: func(ctx context.Context, opts runner.Options) (supervisor.Session, error) {
			opts.Store = store
			opts.Model = model
			opts.Provider = newProvider()
			return runner.New(ctx, opts)
		},
		ResumeSession: func(ctx context.Context, id string, opts runner.Options) (supervisor.Session, error) {
			opts.Store = store
			opts.Model = model
			opts.Provider = newProvider()
			return runner.Resume(ctx, id, opts)
		},
	}
	sup, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })
	return sup
}

// newTestDaemon wires sup behind an in-process httptest.Server, returning the
// Daemon and its ws:// base URL. The server is closed on test cleanup. It is a
// thin wrapper over [newTestDaemonWithConfig] with the default token/model
// config every non-auth test uses.
func newTestDaemon(t *testing.T, sup *supervisor.Supervisor, token string) (*daemon.Daemon, string) {
	t.Helper()
	return newTestDaemonWithConfig(t, sup, daemon.Config{BearerToken: token, DefaultModel: "faux"})
}

// newTestDaemonWithConfig wires sup behind an in-process httptest.Server using
// the caller-supplied cfg, so a test can exercise Config fields (e.g.
// AuthedProviders) the common newTestDaemon path doesn't set. The server is
// closed on test cleanup.
func newTestDaemonWithConfig(t *testing.T, sup *supervisor.Supervisor, cfg daemon.Config) (*daemon.Daemon, string) {
	t.Helper()
	d := daemon.New(sup, cfg)
	srv := httptest.NewServer(d.Handler())
	t.Cleanup(srv.Close)
	return d, "ws" + srv.URL[len("http"):]
}

// rpcFrame is the generic shape of any daemon->client frame: a response
// (Result/Error set, ID present) or a notification (Method set, ID absent).
type rpcFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *frameError     `json:"error,omitempty"`
}

type frameError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// wsNotificationBuffer sizes every [wsClient]'s notification buffer.
//
// [wsClient.readLoop]'s send into that channel is BLOCKING and unselected, so a
// test that emits a BURST of frames before it starts draining needs the whole
// burst to fit. Anything over capacity rests on TCP socket buffers alone, and
// when those fill the daemon-side fan-out write stalls against
// [relayWriteTimeout] — whose cancellation then closes the very connection the
// test is watching, failing it for a reason unrelated to what it asserts.
//
// It is sized well clear of the largest such burst (the fan-out write-context
// probe's fanOutProbeWrites + 1 sentinel) rather than exactly at it: the
// coupling between a test's repetition count and this constant should have
// slack, and a test that depends on the burst fitting guards it explicitly
// against cap() instead of assuming a number here.
const wsNotificationBuffer = 256

// wsClient is a minimal JSON-RPC-over-WebSocket test client: it demuxes
// inbound frames into a notifications stream and a responses stream so a test
// can send a request and drain session/update notifications concurrently
// while waiting for the terminal response.
type wsClient struct {
	t    *testing.T
	conn *websocket.Conn
	ctx  context.Context
	idc  int64

	notifications chan rpcFrame

	// inboundRequests carries daemon-initiated REQUESTS (a method AND an id —
	// session/request_permission), which a pure ACP client answers. A test
	// reads one via waitRequest and answers with respond/respondError. A gofer-
	// native test client simply never drains it (the daemon still works — the
	// request just goes unanswered), so it is buffered generously.
	inboundRequests chan rpcFrame

	// pending maps a request's marshaled id to the channel readLoop delivers
	// its matching response to. It exists because multiple requests can be
	// outstanding on one connection at once (e.g. a blocked session/prompt
	// alongside a gofer/archive sent while it's in flight): a single shared
	// response channel would let whichever goroutine's select happens to win
	// steal a response meant for a different in-flight request. readLoop is
	// the connection's only reader, so it alone decides delivery.
	mu        sync.Mutex
	pending   map[string]chan rpcFrame
	unmatched chan rpcFrame // response-shaped frames with no registered id (e.g. a parse-error's null id)
}

// dial opens a WebSocket connection to url. header, if non-nil, is sent with
// the upgrade request (e.g. an Authorization header for bearer auth).
func dial(t *testing.T, ctx context.Context, url string, header map[string][]string) *wsClient {
	t.Helper()
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	c := &wsClient{
		t:               t,
		conn:            conn,
		ctx:             ctx,
		notifications:   make(chan rpcFrame, wsNotificationBuffer),
		inboundRequests: make(chan rpcFrame, 16),
		pending:         make(map[string]chan rpcFrame),
		unmatched:       make(chan rpcFrame, 16),
	}
	go c.readLoop()
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	return c
}

func (c *wsClient) readLoop() {
	for {
		_, data, err := c.conn.Read(c.ctx)
		if err != nil {
			c.mu.Lock()
			for _, ch := range c.pending {
				close(ch)
			}
			c.pending = nil
			c.mu.Unlock()
			close(c.notifications)
			close(c.inboundRequests)
			close(c.unmatched)
			return
		}
		var f rpcFrame
		if err := json.Unmarshal(data, &f); err != nil {
			continue // the daemon only ever sends well-formed JSON
		}
		if f.Method != "" && len(f.ID) == 0 {
			c.notifications <- f
			continue
		}
		// A daemon-initiated REQUEST: a method AND an id (session/request_permission).
		if f.Method != "" && len(f.ID) > 0 {
			select {
			case c.inboundRequests <- f:
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
			continue
		}
		select {
		case c.unmatched <- f:
		default:
		}
	}
}

// register allocates a single-slot response channel for id and records it in
// pending before the request is written, so readLoop can never observe the
// response before a waiter is registered to receive it.
func (c *wsClient) register(id string) chan rpcFrame {
	ch := make(chan rpcFrame, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	return ch
}

// close tears down the underlying WebSocket connection mid-test (a client
// disconnect), so the daemon's peer.run observes the read error and runs its
// deregister-on-close path. The harness's own t.Cleanup also closes the
// connection; a second Close is a harmless no-op.
func (c *wsClient) close() {
	c.t.Helper()
	_ = c.conn.Close(websocket.StatusNormalClosure, "")
}

// writeRaw sends raw text verbatim, bypassing JSON-RPC envelope construction
// — used to exercise the parse-error path with deliberately malformed input.
func (c *wsClient) writeRaw(raw string) {
	c.t.Helper()
	if err := c.conn.Write(c.ctx, websocket.MessageText, []byte(raw)); err != nil {
		c.t.Fatalf("write raw: %v", err)
	}
}

// request sends a JSON-RPC request and blocks for its matching response.
func (c *wsClient) request(method string, params any) rpcFrame {
	c.t.Helper()
	id := atomic.AddInt64(&c.idc, 1)
	idJSON, _ := json.Marshal(id)
	ch := c.register(string(idJSON))

	req := struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int64  `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{jsonrpcVersion, id, method, params}
	c.write(req)

	select {
	case f, ok := <-ch:
		if !ok {
			c.t.Fatalf("connection closed waiting for response id=%d", id)
		}
		return f
	case <-time.After(defaultWait):
		c.t.Fatalf("timed out waiting for response id=%d", id)
	}
	return rpcFrame{}
}

// respond answers a daemon-initiated request with a success result, echoing id
// verbatim (the daemon owns the id space; the client just reflects it back).
func (c *wsClient) respond(id json.RawMessage, result any) {
	c.t.Helper()
	c.write(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{jsonrpcVersion, id, result})
}

// respondError answers a daemon-initiated request with a JSON-RPC error (e.g. a
// client that does not implement the method), echoing id verbatim.
func (c *wsClient) respondError(id json.RawMessage, code int, message string) {
	c.t.Helper()
	c.write(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   frameError      `json:"error"`
	}{jsonrpcVersion, id, frameError{Code: code, Message: message}})
}

// notify sends a JSON-RPC notification (no id, no response expected).
func (c *wsClient) notify(method string, params any) {
	c.t.Helper()
	n := struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{jsonrpcVersion, method, params}
	c.write(n)
}

func (c *wsClient) write(v any) {
	c.t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		c.t.Fatalf("marshal request: %v", err)
	}
	if err := c.conn.Write(c.ctx, websocket.MessageText, data); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

// waitRawResponse blocks for the next response-shaped frame with no
// registered pending id — used for the parse-error case, whose id is unknown
// (JSON-RPC null) rather than one this client chose and registered.
func (c *wsClient) waitRawResponse() rpcFrame {
	c.t.Helper()
	select {
	case f, ok := <-c.unmatched:
		if !ok {
			c.t.Fatalf("connection closed waiting for a response")
		}
		return f
	case <-time.After(defaultWait):
		c.t.Fatalf("timed out waiting for a response")
	}
	return rpcFrame{}
}

// waitNotification blocks for the next content-bearing session/update
// notification, silently skipping two orthogonal frames: any interleaved
// "gofer/event" frame, and the one-shot session_info_update the daemon fans
// when a session's first prompt derives its title. The M3 lossless-attach
// fanout (see internal/daemon/handlers.go's broadcastGoferEvent) sends the
// daemon's full-fidelity event stream on the SAME connection, for every event a
// session/update would carry and many it wouldn't (turn.started,
// session.error, tool.call.delta, ...); the title update is a metadata
// projection orthogonal to the content stream these tests assert on — so every
// existing session/update-focused test in this package goes through this one
// shared primitive to see only the content projection it's actually testing.
// TestPromptFanOutGoferEventFullFidelity (fanout_test.go) is this package's
// dedicated proof that gofer/event itself carries the full stream, and
// TestSessionFirstPromptSurfacesTitle its proof of the title projection; both
// read c.notifications directly instead of this helper.
func (c *wsClient) waitNotification() rpcFrame {
	c.t.Helper()
	deadline := time.After(defaultWait)
	for {
		select {
		case f, ok := <-c.notifications:
			if !ok {
				c.t.Fatalf("connection closed waiting for a notification")
			}
			if f.Method == "gofer/event" {
				continue
			}
			if isSessionInfoUpdate(f) {
				continue
			}
			return f
		case <-deadline:
			c.t.Fatalf("timed out waiting for a notification")
		}
	}
}

// isSessionInfoUpdate reports whether f is a session/update notification whose
// discriminator is "session_info_update" — the title metadata projection
// waitNotification and assertNoMoreUpdates skip so content-focused tests aren't
// disturbed by it.
func isSessionInfoUpdate(f rpcFrame) bool {
	if f.Method != "session/update" {
		return false
	}
	var up struct {
		Update struct {
			SessionUpdate string `json:"sessionUpdate"`
		} `json:"update"`
	}
	if err := json.Unmarshal(f.Params, &up); err != nil {
		return false
	}
	return up.Update.SessionUpdate == "session_info_update"
}

const jsonrpcVersion = "2.0"

// blockingProvider is a hand-scripted [provider.Provider] whose first model
// call blocks until either released explicitly or its ctx is cancelled — the
// seam the session/cancel test uses to deterministically observe an
// in-flight turn being interrupted with no arbitrary sleeps. See
// [blockingStream.Next]: the SDK's loop package only produces a
// StopReasonCancelled turn.finished when a turn's own ctx is found cancelled
// at the top of its per-chunk read loop, so the fake unblocks (rather than
// erroring) on cancellation and lets that check do the work.
type blockingProvider struct {
	// started is closed once the first (blocking) Next call is reached, so a
	// test can wait for the turn to genuinely be in flight before acting.
	started chan struct{}
	// release, if sent to, unblocks the first Next call with no cancellation
	// involved (unused by the cancel test, available for symmetry/future use).
	release chan struct{}
}

func newBlockingProvider() *blockingProvider {
	return &blockingProvider{started: make(chan struct{}), release: make(chan struct{})}
}

func (p *blockingProvider) Info() provider.ModelInfo { return provider.ModelInfo{ID: "block-test"} }

func (p *blockingProvider) Stream(ctx context.Context, _ provider.Request) (provider.StreamHandle, error) {
	return &blockingStream{p: p, ctx: ctx}, nil
}

type blockingStream struct {
	p   *blockingProvider
	ctx context.Context
	n   int
}

func (s *blockingStream) Next() (provider.StreamEvent, error) {
	s.n++
	switch s.n {
	case 1:
		close(s.p.started)
		select {
		case <-s.p.release:
		case <-s.ctx.Done():
			// Cancelled while "generating": return a normal event rather than
			// ctx.Err() so the loop's own pre-Next ctx check (not this
			// return value) is what turns this into a cancelled turn — see
			// the type doc.
		}
		return provider.StreamEvent{Type: provider.StreamTextDelta, Text: "hello"}, nil
	case 2:
		// Only reached on the RELEASE path: a clean StreamFinished so a
		// released (as opposed to cancelled) turn terminates with end_turn.
		// The cancel path never reaches here — the loop's pre-Next ctx check
		// turns the cancellation into a cancelled turn.finished after the
		// first delta, before Next is called again (see the type doc).
		return provider.StreamEvent{Type: provider.StreamFinished, StopReason: provider.StopEndTurn}, nil
	default:
		return provider.StreamEvent{}, io.EOF
	}
}

func (s *blockingStream) Close() error { return nil }
