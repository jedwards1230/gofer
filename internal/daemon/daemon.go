package daemon

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// DefaultListenAddr is the loopback address [Daemon] binds when
// [Config.ListenAddr] is empty. A tailnet address is opted into explicitly
// via the flag (see cmd/gofer's `daemon` command) — the daemon never guesses
// a non-loopback bind.
const DefaultListenAddr = "127.0.0.1:7333"

// maxMessageBytes caps a single inbound WebSocket message. coder/websocket
// defaults to a 32 KiB read limit — a session/prompt carrying a large paste or
// an inlined file would exceed it and tear down the whole connection. 1 MiB
// bounds per-frame memory while comfortably fitting a realistic prompt.
const maxMessageBytes = 1 << 20

// shutdownTimeout bounds the graceful HTTP shutdown so a wedged non-hijacked
// handler cannot block daemon exit forever. WebSocket connections are hijacked
// (Shutdown neither waits for nor closes them); their handlers are unblocked by
// the context cancellation in Shutdown, not by this timeout.
const shutdownTimeout = 5 * time.Second

// Config configures a [Daemon].
type Config struct {
	// ListenAddr is the address Serve binds. Empty uses [DefaultListenAddr].
	ListenAddr string
	// BearerToken, when non-empty, is required of every WebSocket upgrade
	// (see [Daemon.Handler]). Empty disables auth — appropriate only for a
	// loopback-bound daemon.
	BearerToken string
	// DefaultModel is the model a session/new or session/load ACP request
	// resolves to, since ACP's session/new carries no model field. Callers
	// resolve this the same way `gofer run` does (the sole logged-in
	// provider's model) before constructing Config; the daemon does not
	// re-derive it.
	DefaultModel string
}

// Daemon hosts a [supervisor.Supervisor] behind an ACP-over-WebSocket
// listener. See the package doc for the transport and streaming contract.
type Daemon struct {
	sup *supervisor.Supervisor
	cfg Config

	ctx    context.Context
	cancel context.CancelFunc

	server *http.Server
}

// New builds a Daemon around sup. It does not start listening — call Serve
// (or mount Handler on a caller-owned server, e.g. httptest.NewServer for
// tests).
func New(sup *supervisor.Supervisor, cfg Config) *Daemon {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = DefaultListenAddr
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Daemon{sup: sup, cfg: cfg, ctx: ctx, cancel: cancel}
}

// Handler returns the daemon's WebSocket upgrade handler, exported so tests
// can mount it on an httptest.Server instead of a real listener.
func (d *Daemon) Handler() http.Handler {
	return http.HandlerFunc(d.serveWS)
}

// serveWS authenticates the request, upgrades it to a WebSocket connection,
// and runs its peer loop until the connection or the daemon closes.
func (d *Daemon) serveWS(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(maxMessageBytes)
	defer conn.CloseNow() //nolint:errcheck // best-effort; the connection is already gone or about to be

	connCtx, cancel := context.WithCancel(d.ctx)
	defer cancel()

	p := newPeer(conn, d)
	p.run(connCtx)

	_ = conn.Close(websocket.StatusNormalClosure, "")
}

// authorized reports whether r carries the configured bearer token. A
// [Config] with no BearerToken accepts every connection — the operator's
// choice for a loopback-only daemon. The token is read from the standard
// Authorization: Bearer header, falling back to a "token" query parameter for
// clients (e.g. some mobile WebSocket libraries) that cannot set a custom
// upgrade header. Comparison is constant-time; the token itself is never
// logged or included in any error.
func (d *Daemon) authorized(r *http.Request) bool {
	if d.cfg.BearerToken == "" {
		return true
	}
	token := bearerFromHeader(r.Header.Get("Authorization"))
	if token == "" {
		token = r.URL.Query().Get("token")
	}
	if token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(d.cfg.BearerToken)) == 1
}

// bearerFromHeader extracts the token from an "Authorization: Bearer <token>"
// header value, or "" if it is not in that form.
func bearerFromHeader(h string) string {
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return h[len(prefix):]
}

// Serve runs the daemon's HTTP/WebSocket listener in the foreground until ctx
// is cancelled, then shuts down gracefully and returns. It is the entry point
// `gofer daemon` uses; tests instead mount [Daemon.Handler] on their own
// server.
func (d *Daemon) Serve(ctx context.Context) error {
	d.server = &http.Server{
		Addr:    d.cfg.ListenAddr,
		Handler: d.Handler(),
	}

	errCh := make(chan error, 1)
	go func() { errCh <- d.server.ListenAndServe() }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		return d.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("daemon: listen %s: %w", d.cfg.ListenAddr, err)
	}
}

// Shutdown cancels every live connection's context (unblocking any in-flight
// session/prompt handler) and gracefully shuts down the HTTP server. It does
// NOT close the supervisor — the caller owns that (see cmd/gofer's `daemon`
// command). Idempotent.
func (d *Daemon) Shutdown(ctx context.Context) error {
	d.cancel()
	if d.server == nil {
		return nil
	}
	return d.server.Shutdown(ctx)
}
