package lspdiag

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	sdklsp "github.com/jedwards1230/agent-sdk-go/lsp"
)

const (
	// DefaultTimeout bounds how long Diagnose waits for a fresh
	// textDocument/publishDiagnostics notification after (re)opening a file.
	// A cold gopls can take several seconds to load a workspace's packages on
	// its first request, so this is generous rather than tuned for the
	// steady state; a timeout degrades to "no diagnostics this call", never
	// an error.
	DefaultTimeout = 4 * time.Second
	// DefaultMaxDiagnostics caps how many diagnostic lines Diagnose returns —
	// the context-cost bound: a file with a cascading syntax error can
	// produce dozens of diagnostics, and dumping all of them into the
	// model's context on every edit is exactly the firehose gofer's
	// context-cost discipline rules out.
	DefaultMaxDiagnostics = 10
	// shutdownGrace bounds the polite shutdown+exit handshake Manager.Close
	// gives each live server before force-closing its transport (which kills
	// the process outright). A server that ignores it still gets killed —
	// this only decides how long Close waits to be nice about it.
	shutdownGrace = 2 * time.Second
)

// ErrClosed is returned by [Manager.Diagnose] (via ensureServer) once
// [Manager.Close] has run.
var ErrClosed = errors.New("lspdiag: manager closed")

// Options configures a [Wrap]'d registry's diagnostics behavior. The zero
// value is invalid for Enabled — Options are only used after a caller
// explicitly opts in — but Timeout/MaxDiagnostics fill in defaults via
// resolve.
type Options struct {
	// Enabled gates whether Wrap decorates the registry at all. False is a
	// zero-cost no-op: Wrap returns the base registry unchanged.
	Enabled bool
	// Timeout bounds the wait for a diagnostics publish per Diagnose call.
	// <= 0 resolves to DefaultTimeout.
	Timeout time.Duration
	// MaxDiagnostics caps the diagnostic lines returned per call. <= 0
	// resolves to DefaultMaxDiagnostics.
	MaxDiagnostics int
}

func (o Options) resolve() Options {
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.MaxDiagnostics <= 0 {
		o.MaxDiagnostics = DefaultMaxDiagnostics
	}
	return o
}

// Manager owns every language server this process has started, keyed by
// (workspace root, language) — one live [sdklsp.Client] per key, lazily
// started on first use and shared by every session whose sessionGuard wraps
// its tool registry through this Manager (see internal/supervisor). Manager
// itself implements [sdklsp.Publisher]: it fans a server's
// textDocument/publishDiagnostics notifications out to whichever call is
// currently waiting on that URI.
//
// The zero value is not usable; construct with [NewManager]. A Manager is
// safe for concurrent use.
type Manager struct {
	registry *sdklsp.Registry
	logger   *slog.Logger

	mu      sync.Mutex
	closed  bool
	servers map[string]*server // key: workspace root + "\x00" + language

	waitMu  sync.Mutex
	waiters map[string][]chan sdklsp.Batch // key: file URI
}

// server is one live language-server connection plus the minimal open-file
// bookkeeping Diagnose needs to force a fresh diagnostics round-trip.
type server struct {
	client *sdklsp.Client

	mu       sync.Mutex
	openDocs map[string]struct{} // key: file URI
}

// NewManager returns a Manager that starts nothing until the first
// [Manager.Diagnose] call needs a server — constructing one spawns no
// process and opens no file descriptor.
func NewManager() *Manager {
	return &Manager{
		registry: sdklsp.DefaultRegistry(),
		logger:   slog.New(slog.DiscardHandler),
		servers:  make(map[string]*server),
		waiters:  make(map[string][]chan sdklsp.Batch),
	}
}

// Publish implements [sdklsp.Publisher]. It is called from each server's own
// background read loop (see agent-sdk-go/lsp.Client), never from a Diagnose
// caller's goroutine.
func (m *Manager) Publish(_ context.Context, _ string, batch sdklsp.Batch) {
	m.waitMu.Lock()
	chans := m.waiters[batch.URI]
	delete(m.waiters, batch.URI)
	m.waitMu.Unlock()
	for _, ch := range chans {
		select {
		case ch <- batch:
		default: // an abandoned (timed-out) waiter; nothing left to deliver to
		}
	}
}

// arm registers a fresh, buffered channel to receive the next Publish for
// uri. Callers MUST arm before triggering the action that will provoke the
// publish (see Diagnose) — arming after risks a notification racing ahead of
// the registration and being missed entirely.
func (m *Manager) arm(uri string) chan sdklsp.Batch {
	ch := make(chan sdklsp.Batch, 1)
	m.waitMu.Lock()
	m.waiters[uri] = append(m.waiters[uri], ch)
	m.waitMu.Unlock()
	return ch
}

// disarm removes ch from uri's waiter list without delivering to it — used
// when a caller gives up (timeout, sync error) so an abandoned wait can never
// accumulate in the map. Safe to call after Publish has already claimed (and
// removed) the whole list; it is then a no-op.
func (m *Manager) disarm(uri string, ch chan sdklsp.Batch) {
	m.waitMu.Lock()
	defer m.waitMu.Unlock()
	list := m.waiters[uri]
	for i, c := range list {
		if c == ch {
			m.waiters[uri] = append(list[:i], list[i+1:]...)
			break
		}
	}
	if len(m.waiters[uri]) == 0 {
		delete(m.waiters, uri)
	}
}

// ensureServer returns the live server for (root, language), starting and
// initializing one if this is the first request for that key. A second
// caller racing the first start may spawn a duplicate process that loses the
// race; the loser is closed immediately rather than left running.
func (m *Manager) ensureServer(ctx context.Context, root, language string) (*server, error) {
	key := root + "\x00" + language

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	if s, ok := m.servers[key]; ok {
		m.mu.Unlock()
		return s, nil
	}
	m.mu.Unlock()

	resolved, err := m.registry.Resolve(language)
	if err != nil {
		// ErrNotRegistered (no server known for this language) or
		// ErrNotOnPath (known but not installed) — both are the caller's cue
		// to degrade silently, not a Manager-level failure.
		return nil, err
	}
	// Start does not accept lsp.Option (unlike NewClient) — its own read loop
	// diagnostics stay silent, matching the discard-by-default Manager.logger
	// this package otherwise uses for its own (Diagnose-level) logging.
	client, err := sdklsp.Start(ctx, resolved, m, key)
	if err != nil {
		return nil, fmt.Errorf("lspdiag: start %s: %w", resolved.Command, err)
	}
	if err := client.Initialize(ctx, fileURI(root)); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("lspdiag: initialize %s: %w", resolved.Command, err)
	}
	s := &server{client: client, openDocs: make(map[string]struct{})}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = client.Close()
		return nil, ErrClosed
	}
	if existing, ok := m.servers[key]; ok {
		// Lost the race: another goroutine's server for this exact key won
		// first. Close this one so it never leaks, and hand back the winner.
		m.mu.Unlock()
		_ = client.Close()
		return existing, nil
	}
	m.servers[key] = s
	m.mu.Unlock()
	return s, nil
}

// sync forces uri to reflect text on the server: DidClose (if it was already
// open) followed by DidOpen. The SDK's Client exposes no
// textDocument/didChange, so a close+reopen is the only way this package can
// ask a running server to re-diagnose a file it already has open — see the
// package doc's Lifecycle section.
func (s *server) sync(ctx context.Context, uri, language, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, open := s.openDocs[uri]; open {
		if err := s.client.DidClose(ctx, uri); err != nil {
			return err
		}
		delete(s.openDocs, uri)
	}
	if err := s.client.DidOpen(ctx, uri, language, text); err != nil {
		return err
	}
	s.openDocs[uri] = struct{}{}
	return nil
}

// Diagnose triggers a diagnostics round-trip for path (whose on-disk content
// is now text — the caller's job to have already written it) and returns up
// to opts.MaxDiagnostics normalized, one-line diagnostics for that file. It
// returns nil whenever nothing usable is available: an unsupported
// extension, no server installed for the language, a server that failed to
// start, or a wait that hit opts.Timeout with nothing published — every one
// of these is logged (at Warn for an actual failure, Debug for "nothing
// registered/installed", never louder) and never returned as an error, so a
// caller can treat a nil result as simply "no diagnostics this time".
func (m *Manager) Diagnose(ctx context.Context, root, path, text string, opts Options) []string {
	opts = opts.resolve()
	language, ok := languageForPath(path)
	if !ok {
		return nil
	}
	uri := fileURI(path)

	s, err := m.ensureServer(ctx, root, language)
	if err != nil {
		switch {
		case errors.Is(err, sdklsp.ErrNotRegistered), errors.Is(err, sdklsp.ErrNotOnPath):
			m.logger.Debug("lspdiag: no server available", "language", language, "error", err)
		case errors.Is(err, ErrClosed):
			// Manager shutting down mid-call; nothing to log.
		default:
			m.logger.Warn("lspdiag: server unavailable", "language", language, "error", err)
		}
		return nil
	}

	waitCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	ch := m.arm(uri)
	if err := s.sync(waitCtx, uri, language, text); err != nil {
		m.disarm(uri, ch)
		m.logger.Warn("lspdiag: sync failed", "uri", uri, "error", err)
		return nil
	}

	select {
	case batch := <-ch:
		return capDiagnostics(batch.Strings(), opts.MaxDiagnostics)
	case <-waitCtx.Done():
		m.disarm(uri, ch)
		m.logger.Debug("lspdiag: diagnostics wait timed out", "uri", uri, "timeout", opts.Timeout)
		return nil
	}
}

// capDiagnostics bounds items to max entries, appending a collapsed-count
// marker in the style the TUI's own approval-body truncation uses ("… +N
// more lines" — see docs/TUI.md) so the two truncation affordances in this
// codebase read the same way.
func capDiagnostics(items []string, max int) []string {
	if len(items) <= max {
		return items
	}
	out := make([]string, 0, max+1)
	out = append(out, items[:max]...)
	out = append(out, fmt.Sprintf("… +%d more", len(items)-max))
	return out
}

// Close shuts down every server this Manager ever started: a bounded
// shutdown+exit handshake per server, then Close unconditionally (which
// force-kills the process if it did not exit on its own and releases its
// stdio pipes). Idempotent; safe to call even if no server was ever started.
// After Close, every future Diagnose call returns nil immediately
// (ensureServer sees ErrClosed) rather than starting a new server.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	servers := make([]*server, 0, len(m.servers))
	for _, s := range m.servers {
		servers = append(servers, s)
	}
	m.servers = nil
	m.mu.Unlock()

	var errs []error
	for _, s := range servers {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		_ = s.client.Shutdown(shutdownCtx) // best-effort: Close below always runs regardless
		cancel()
		if err := s.client.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
