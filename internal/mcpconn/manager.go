package mcpconn

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jedwards1230/agent-sdk-go/mcp"
	"github.com/jedwards1230/agent-sdk-go/tool"

	"github.com/jedwards1230/gofer/internal/config"
)

// defaultHealthCheckInterval bounds how often a connected server is
// re-verified with a cheap "tools/list" round trip. This is an internal
// implementation cadence, not an operator opinion config.MCP exposes: unlike
// ConnectTimeout/CallTimeout/RetryMaxInterval (which govern outcomes an
// operator can reasonably want to tune), how often THIS package polls a
// healthy connection is an implementation detail with no user-facing
// tradeoff worth a config knob — matching internal/lspdiag's own
// shutdownGrace precedent.
const defaultHealthCheckInterval = 30 * time.Second

// initialBackoff is the first retry delay after a connect (or health-check)
// failure; it doubles on each subsequent failure, capped at
// [config.MCP.RetryMaxInterval].
const initialBackoff = 1 * time.Second

// Config configures a [Manager].
type Config struct {
	// MCP is the server list and timeouts to connect. The zero value has no
	// servers, so [NewManager] over a zero Config is fully valid and Start
	// does nothing.
	MCP config.MCP
	// Logger is optional structured diagnostics for otherwise-silent
	// behavior (a connect failure before backoff, a health check tearing a
	// connection down). Nil defaults to a discard logger, mirroring
	// internal/lspdiag.Manager's own default-silent posture.
	Logger *slog.Logger
	// Dialer overrides how a server is connected. Nil defaults to [Dial].
	// Tests inject a Dialer that talks to an in-memory transport (see
	// mcp.NewStdio) so nothing here ever spawns a real subprocess or touches
	// the network.
	Dialer Dialer
}

// serverState is one server's live connection bookkeeping. client is nil
// whenever the server is not currently reachable — never connected yet, or
// connected once and now down awaiting reconnect — which is also exactly
// [Manager.Snapshot]'s signal for that server belonging in Snapshot.Down
// instead of contributing tools.
type serverState struct {
	client    *mcp.Client
	tools     []tool.Tool
	attempted bool // at least one connect attempt (success or failure) has completed
}

// Manager owns every MCP server connection ONE gofer process makes — see the
// package doc's Lifecycle section. The zero value is not usable; construct
// with [NewManager]. A Manager is safe for concurrent use.
type Manager struct {
	cfg    config.MCP
	dial   Dialer
	logger *slog.Logger

	healthInterval time.Duration // package default; same-package tests may lower it

	mu      sync.Mutex
	started bool
	closed  bool
	cancel  context.CancelFunc
	servers map[string]*serverState
	order   []string // server names, config order — Snapshot's determinism
	pending int      // enabled servers with no completed attempt yet

	settleOnce sync.Once
	settled    chan struct{}

	wg sync.WaitGroup
}

// NewManager builds a Manager that connects nothing until [Manager.Start].
func NewManager(cfg Config) *Manager {
	dial := cfg.Dialer
	if dial == nil {
		dial = Dial
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Manager{
		cfg:            cfg.MCP,
		dial:           dial,
		logger:         logger,
		healthInterval: defaultHealthCheckInterval,
		servers:        make(map[string]*serverState),
		settled:        make(chan struct{}),
	}
}

// Start begins connecting every [config.MCP.EnabledServers] server
// asynchronously — one goroutine per server, none of which Start waits for —
// so a slow or unreachable server never delays Start's return. ctx is the
// Manager's OWN long-lived context: cancelling it (or calling [Manager.Close],
// which does so internally) stops every reconnect loop and lets every live
// connection be closed. Safe to call at most once; a second call, or a call
// after Close, is a no-op.
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	if m.started || m.closed {
		m.mu.Unlock()
		return
	}
	m.started = true
	runCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel

	servers := m.cfg.EnabledServers()
	m.pending = len(servers)
	for _, srv := range servers {
		m.servers[srv.Name] = &serverState{}
		m.order = append(m.order, srv.Name)
	}
	noServers := len(servers) == 0
	m.mu.Unlock()

	if noServers {
		// Nothing to wait for: settle immediately so AwaitReady never blocks
		// a session create on a Manager with no configured servers.
		m.settleOnce.Do(func() { close(m.settled) })
		return
	}

	for _, srv := range servers {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.runServer(runCtx, srv)
		}()
	}
}

// runServer is one server's whole lifecycle: connect, project its tools,
// record the outcome, and — on success — watch the connection until it dies
// or ctx is cancelled, then loop back to reconnect. Every failure (connect,
// project, or a later health-check death) funnels through the SAME capped
// backoff before the next attempt, so a flapping server cannot hot-loop this
// goroutine.
func (m *Manager) runServer(ctx context.Context, srv config.MCPServer) {
	connectTimeout := srv.ConnectTimeout(m.cfg)
	callTimeout := srv.CallTimeout(m.cfg)
	var backoff time.Duration

	for {
		if ctx.Err() != nil {
			return
		}

		client, err := m.dial(ctx, srv, connectTimeout, callTimeout)
		if err != nil {
			m.logger.Warn("mcpconn: connect failed", "server", srv.Name, "error", err)
			m.recordAttempt(srv.Name, nil, nil)
		} else if tools, perr := projectTools(ctx, m, client, srv); perr != nil {
			m.logger.Warn("mcpconn: list tools failed", "server", srv.Name, "error", perr)
			_ = client.Close()
			m.recordAttempt(srv.Name, nil, nil)
		} else {
			m.logger.Info("mcpconn: connected", "server", srv.Name, "tools", len(tools))
			m.recordAttempt(srv.Name, client, tools)
			backoff = 0
			m.watch(ctx, srv, client) // blocks until the connection dies or ctx ends
			if ctx.Err() != nil {
				return
			}
			// The connection died: fall through to backoff+retry below,
			// exactly like a failed connect attempt.
		}

		backoff = nextBackoff(backoff, m.cfg.RetryMaxInterval())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}

// watch periodically re-verifies client with a cheap "tools/list" call until
// either ctx ends (Manager shutting down — the caller's cue to return without
// touching client, see runServer) or a health check fails, in which case it
// marks the server down (so [proxyTool.Run] starts reporting IsError
// immediately rather than waiting on a hung call) and closes client before
// returning.
func (m *Manager) watch(ctx context.Context, srv config.MCPServer, client *mcp.Client) {
	ticker := time.NewTicker(m.healthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := client.ListTools(ctx); err != nil {
				m.logger.Warn("mcpconn: health check failed, reconnecting", "server", srv.Name, "error", err)
				m.markDown(srv.Name, client)
				_ = client.Close()
				return
			}
		}
	}
}

// recordAttempt stores srv's outcome (client/tools nil on failure) and, the
// FIRST time any given server completes an attempt, counts it toward
// settling — see [Manager.AwaitReady].
func (m *Manager) recordAttempt(name string, client *mcp.Client, tools []tool.Tool) {
	m.mu.Lock()
	s, ok := m.servers[name]
	if !ok { // Close ran between dial and here; nothing left to record into.
		m.mu.Unlock()
		if client != nil {
			_ = client.Close()
		}
		return
	}
	s.client = client
	s.tools = tools
	firstAttempt := !s.attempted
	s.attempted = true
	if firstAttempt && m.pending > 0 {
		m.pending--
	}
	settle := firstAttempt && m.pending == 0
	m.mu.Unlock()

	if settle {
		m.settleOnce.Do(func() { close(m.settled) })
	}
}

// markDown clears name's live client IF it is still the one the caller
// observed dying (guards a reconnect that already replaced it from racing
// against a stale health-check's teardown).
func (m *Manager) markDown(name string, dead *mcp.Client) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.servers[name]; ok && s.client == dead {
		s.client = nil
	}
}

// live returns name's current *mcp.Client, or nil if it has never connected
// or is currently down. Called by [proxyTool.Run] on every invocation — see
// the package doc's "Reconnect without re-registration".
func (m *Manager) live(name string) *mcp.Client {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.servers[name]
	if !ok {
		return nil
	}
	return s.client
}

// AwaitReady blocks until either every enabled server has completed at
// least one connection attempt (Start's "initial discovery" has settled) or
// ctx is done, whichever comes first — the bounded best-effort wait
// [config.MCP.ReadyTimeout] governs, same shape and rationale as
// [config.DefaultLoadSettleTimeout]. It returns immediately once already
// settled (the common case for every session after the first), and
// immediately if Start was never called or configured zero servers.
func (m *Manager) AwaitReady(ctx context.Context) {
	select {
	case <-m.settled:
	case <-ctx.Done():
	}
}

// Snapshot returns the Manager's CURRENT best-effort tool set: the union of
// every currently-connected server's projected tools (config order), plus
// the names of every enabled server with no live connection right now
// (Down) — the caller's cue to emit a visible notice (see
// internal/supervisor's sessionGuard). This is a point-in-time read with NO
// promise of staying current: see the package doc's "A session's tool set is
// fixed at create".
func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out Snapshot
	for _, name := range m.order {
		s := m.servers[name]
		if s == nil || s.client == nil {
			out.Down = append(out.Down, name)
			continue
		}
		out.Tools = append(out.Tools, s.tools...)
	}
	return out
}

// Snapshot is [Manager.Snapshot]'s result.
type Snapshot struct {
	// Tools is the union of every currently-connected server's projected
	// tools, config order.
	Tools []tool.Tool
	// Down lists enabled servers with no live connection at snapshot time —
	// including a server that has never once connected successfully.
	Down []string
}

// Close stops every server goroutine (cancelling the context [Manager.Start]
// derived) and joins them, THEN closes every still-live client — ordering
// that matters: a goroutine mid-dial or mid-watch must observe ctx done and
// return before Close sweeps m.servers, so no client is closed out from
// under a goroutine still using it, and no goroutine can record a new
// connection into a Manager Close has already swept. Idempotent; safe even
// if Start was never called.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	cancel := m.cancel
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	m.wg.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()
	var errs []error
	for _, s := range m.servers {
		if s.client != nil {
			if err := s.client.Close(); err != nil {
				errs = append(errs, err)
			}
			s.client = nil
		}
	}
	return errors.Join(errs...)
}

// nextBackoff returns the NEXT retry delay given cur (the delay just used,
// or zero for "no attempt yet"): [initialBackoff] for the first failure,
// doubling on every failure after that, capped at max. A non-positive max —
// [config.MCP.RetryMaxInterval] never resolves to one, but a zero-value
// Manager built without Start still has a zero cfg — falls back to
// initialBackoff so a degenerate cap can never produce a zero/negative
// sleep.
func nextBackoff(cur, max time.Duration) time.Duration {
	if max <= 0 {
		max = initialBackoff
	}
	if cur <= 0 {
		if initialBackoff > max {
			return max
		}
		return initialBackoff
	}
	next := cur * 2
	if next > max || next <= 0 { // next <= 0 guards overflow on a very large cur
		next = max
	}
	return next
}
