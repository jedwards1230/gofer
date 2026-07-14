package daemon

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
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

// readHeaderTimeout bounds how long the server will wait to read a request's
// headers (the HTTP upgrade request, in this daemon's case). With no limit a
// client that opens a connection and trickles bytes (or none at all) ties up
// a goroutine and a file descriptor indefinitely — the slowloris DoS.
const readHeaderTimeout = 10 * time.Second

// maxConns bounds the number of concurrent WebSocket connections the daemon
// will hold open at once, so a connection flood cannot exhaust file
// descriptors or per-connection memory (read buffers, the in-flight-handler
// semaphore, the ping goroutine). serveWS try-acquires a slot before
// upgrading and refuses the upgrade with 503 (rather than blocking the
// accept path) once the cap is reached — a rejected connection is cheap and
// immediately visible to the client, unlike blocking mid-handshake.
const maxConns = 128

// pingInterval is how often a live connection is pinged to detect a dead TCP
// peer. This is deliberately NOT an idle-read deadline: session/prompt and
// the gofer-native control methods are the only traffic a client sends, and
// an attached/peeking client can legitimately sit silent for minutes between
// prompts while a session runs or simply waits for the next event — closing
// on read-idle would tear down a perfectly healthy connection. A ping/pong
// round trip only tears the connection down when the peer has actually
// stopped responding, not merely gone quiet.
const pingInterval = 30 * time.Second

// pingTimeout bounds a single ping round trip.
const pingTimeout = 10 * time.Second

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
	// Logger receives the daemon's structured logs (connection lifecycle,
	// per-request outcome, session lifecycle — see the package doc's Logging
	// section). Nil defaults to a discarding logger in [New], so embedders and
	// tests that pass no logger stay silent rather than hitting a nil
	// dereference.
	Logger *slog.Logger
}

// Daemon hosts a [supervisor.Supervisor] behind an ACP-over-WebSocket
// listener. See the package doc for the transport and streaming contract.
type Daemon struct {
	sup *supervisor.Supervisor
	cfg Config
	log *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc

	server *http.Server

	// connSem is a counting semaphore bounding concurrent connections to
	// maxConns (see serveWS).
	connSem chan struct{}

	// sessionPeersMu guards sessionPeers. It is a dedicated lock, held ONLY
	// for the O(1) map mutations below and the snapshot in peersForSession —
	// never across a peer.notify (a socket write), so a slow or wedged client
	// can never stall the registry for every other session's fan-out.
	sessionPeersMu sync.RWMutex
	// sessionPeers is the session->peers fan-out registry: for each session id,
	// the set of connected peers that have "attached" to it (via session/load
	// or by driving a session/prompt). handleSessionPrompt broadcasts each
	// projected session/update to this set so a turn one client drives is seen
	// by every other client attached to the same session. Empty session sets
	// are deleted, so a live entry always has at least one peer.
	sessionPeers map[string]map[*peer]struct{}

	// permMu guards permRoutes.
	permMu sync.Mutex
	// permRoutes maps a permission request's call id to the session it belongs
	// to. An event.PermissionReply op carries only the call id (no session id —
	// see event.PermissionReply), so the daemon records the route when it
	// broadcasts a permission.requested (where it knows the session) and looks
	// it up again in handlePermissionReply to route the reply to that session's
	// gate. Cleared on the matching permission.resolved. A route left dangling
	// by a turn cancelled before it resolves lingers until the next daemon
	// restart — bounded by the unique tool-call ids of a session, the same M3
	// bound the SDK Gate's own pending map carries.
	permRoutes map[string]string

	// permReqMu guards permReqCancels.
	permReqMu sync.Mutex
	// permReqCancels maps a permission request's call id to the cancel func for
	// the spec-ACP session/request_permission requests the daemon fanned out to
	// ACP peers for it (see [Daemon.requestPermissionFromPeers]). When the
	// permission resolves by ANY path — an ACP peer's answer, a gofer-native
	// permission.reply, or an interrupt — the daemon cancels the outstanding
	// requests at every OTHER peer so no daemon-side waiter dangles, mirroring
	// the gofer/permission_resolved fanout timing. Bounded by session call ids,
	// same as permRoutes.
	permReqCancels map[string]context.CancelFunc
}

// New builds a Daemon around sup. It does not start listening — call Serve
// (or mount Handler on a caller-owned server, e.g. httptest.NewServer for
// tests).
func New(sup *supervisor.Supervisor, cfg Config) *Daemon {
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = DefaultListenAddr
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Daemon{
		sup:            sup,
		cfg:            cfg,
		log:            logger,
		ctx:            ctx,
		cancel:         cancel,
		connSem:        make(chan struct{}, maxConns),
		sessionPeers:   make(map[string]map[*peer]struct{}),
		permRoutes:     make(map[string]string),
		permReqCancels: make(map[string]context.CancelFunc),
	}
}

// attachPeer records that p is interested in sessionID's session/update
// stream, so handleSessionPrompt's broadcast (see peersForSession) reaches it.
// Called when a peer attaches to a session — on session/load and at the top of
// session/prompt (a prompting client is attached for subsequent turns other
// peers drive too). Idempotent: attaching an already-attached peer is a no-op.
//
// Lock discipline: sessionPeersMu and the peer's own attachedMu are taken in
// sequence, never nested — sessionPeersMu is released before attachedMu is
// acquired. detachPeer takes them in the opposite order (attachedMu first),
// which is safe precisely because neither is ever held while acquiring the
// other, so no lock-ordering cycle exists.
func (d *Daemon) attachPeer(sessionID string, p *peer) {
	d.sessionPeersMu.Lock()
	peers := d.sessionPeers[sessionID]
	if peers == nil {
		peers = make(map[*peer]struct{})
		d.sessionPeers[sessionID] = peers
	}
	peers[p] = struct{}{}
	d.sessionPeersMu.Unlock()

	p.attachedMu.Lock()
	p.attached[sessionID] = struct{}{}
	p.attachedMu.Unlock()
}

// detachPeer removes p from every session it is attached to. Called once from
// a deferred cleanup in peer.run, after all of p's in-flight handlers have been
// joined, so the peer leaves the registry cleanly on disconnect with no
// dangling reference and no goroutine/subscription leak. O(sessions p attached
// to), not O(all sessions), because p tracks its own attached set.
func (d *Daemon) detachPeer(p *peer) {
	p.attachedMu.Lock()
	ids := make([]string, 0, len(p.attached))
	for id := range p.attached {
		ids = append(ids, id)
	}
	p.attached = make(map[string]struct{})
	p.attachedMu.Unlock()

	d.sessionPeersMu.Lock()
	for _, id := range ids {
		peers := d.sessionPeers[id]
		if peers == nil {
			continue
		}
		delete(peers, p)
		if len(peers) == 0 {
			delete(d.sessionPeers, id)
		}
	}
	d.sessionPeersMu.Unlock()
}

// peersForSession returns a snapshot of the peers attached to sessionID. The
// registry lock is held ONLY to copy the set into a fresh slice and released
// before the caller iterates — a peer.notify (a socket write, potentially slow
// on a wedged client) must never run under sessionPeersMu, or one stuck client
// would stall fan-out for every session. A peer that disconnects after the
// snapshot is taken is harmless: its notify simply errors, which the broadcast
// caller logs and skips (see handleSessionPrompt).
func (d *Daemon) peersForSession(sessionID string) []*peer {
	d.sessionPeersMu.RLock()
	defer d.sessionPeersMu.RUnlock()
	peers := d.sessionPeers[sessionID]
	out := make([]*peer, 0, len(peers))
	for p := range peers {
		out = append(out, p)
	}
	return out
}

// recordPermRoute remembers that permission call id belongs to sessionID, so a
// later permission.reply carrying only that id can be routed to the right
// session's gate (see handlePermissionReply). Called as a permission.requested
// is broadcast.
func (d *Daemon) recordPermRoute(id, sessionID string) {
	d.permMu.Lock()
	d.permRoutes[id] = sessionID
	d.permMu.Unlock()
}

// clearPermRoute drops id's route once its request has resolved.
func (d *Daemon) clearPermRoute(id string) {
	d.permMu.Lock()
	delete(d.permRoutes, id)
	d.permMu.Unlock()
}

// lookupPermRoute returns the session a permission call id belongs to, or
// ("", false) if no outstanding request has that id.
func (d *Daemon) lookupPermRoute(id string) (string, bool) {
	d.permMu.Lock()
	defer d.permMu.Unlock()
	s, ok := d.permRoutes[id]
	return s, ok
}

// registerPermCancel records the cancel func for the session/request_permission
// requests fanned out for call id. A pre-existing entry (a call-id collision,
// not expected) is cancelled before being replaced so no cancel func is lost.
func (d *Daemon) registerPermCancel(id string, cancel context.CancelFunc) {
	d.permReqMu.Lock()
	old, ok := d.permReqCancels[id]
	d.permReqCancels[id] = cancel
	d.permReqMu.Unlock()
	if ok {
		old()
	}
}

// cancelPermRequest cancels and forgets the outstanding session/request_permission
// requests for call id. Idempotent: a second call (e.g. the drain loop's
// permission.resolved and the handler's deferred sweep both firing) is a no-op.
func (d *Daemon) cancelPermRequest(id string) {
	d.permReqMu.Lock()
	cancel, ok := d.permReqCancels[id]
	if ok {
		delete(d.permReqCancels, id)
	}
	d.permReqMu.Unlock()
	if ok {
		cancel()
	}
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

	// Try-acquire a connection slot rather than blocking: a client at the
	// cap gets an immediate, explicit 503 instead of an upgrade request that
	// hangs until some other connection closes.
	select {
	case d.connSem <- struct{}{}:
	default:
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}
	defer func() { <-d.connSem }()

	// The nil AcceptOptions here is LOAD-BEARING, not an oversight: with
	// OriginPatterns unset, coder/websocket's default rejects any upgrade
	// whose Origin header doesn't match the request host — same-origin only.
	// That is our cross-site WebSocket hijacking (CSWSH) defense-in-depth: a
	// malicious page in a browser cannot open a WebSocket to this daemon on
	// a victim's behalf even before the bearer-token check above runs on the
	// request. Bearer auth is the primary gate; don't add OriginPatterns or
	// InsecureSkipVerify here without replacing this protection some other
	// way.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		// r.RemoteAddr only — never r.URL, which may carry the ?token=
		// fallback (see authorized's doc).
		d.log.Warn("ws accept failed", "remote", r.RemoteAddr, "err", err)
		return
	}
	conn.SetReadLimit(maxMessageBytes)
	defer conn.CloseNow() //nolint:errcheck // best-effort; the connection is already gone or about to be

	// r.RemoteAddr only — never r.URL/r.Header (bearer token, ?token=
	// fallback). See handleFrame's redaction comment for the same rule
	// applied to request bodies.
	d.log.Info("ws connected", "remote", r.RemoteAddr)
	defer d.log.Info("ws disconnected", "remote", r.RemoteAddr)

	connCtx, cancel := context.WithCancel(d.ctx)
	defer cancel()

	// pingLoop detects a dead TCP peer without an idle-read deadline (see
	// pingInterval's doc). It is joined via pingWG before serveWS returns,
	// same discipline as peer.run joining its own in-flight handlers, so the
	// goroutine never outlives its connection.
	var pingWG sync.WaitGroup
	pingWG.Add(1)
	go func() {
		defer pingWG.Done()
		pingLoop(connCtx, conn, cancel)
	}()

	p := newPeer(conn, d)
	p.run(connCtx)

	// p.run returning means the connection's read loop exited (client
	// disconnect, protocol error, etc.) — that does NOT itself cancel
	// connCtx (only d.ctx's cancellation, i.e. daemon Shutdown, does).
	// Cancel explicitly so pingLoop stops now rather than lingering until
	// shutdown; cancel is idempotent, so the deferred cancel() above is a
	// harmless no-op on the shutdown path where connCtx is already done.
	cancel()
	pingWG.Wait()

	_ = conn.Close(websocket.StatusNormalClosure, "")
}

// pingLoop pings conn every pingInterval until ctx is cancelled or a ping
// fails/times out, in which case it cancels ctx itself — tearing the
// connection down through the same path a client disconnect or daemon
// shutdown already uses (peer.run observes the cancellation and unwinds).
func pingLoop(ctx context.Context, conn *websocket.Conn, cancel context.CancelFunc) {
	t := time.NewTicker(pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pingCtx, pingCancel := context.WithTimeout(ctx, pingTimeout)
			err := conn.Ping(pingCtx)
			pingCancel()
			if err != nil {
				cancel()
				return
			}
		}
	}
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
//
// Serve is the authoritative enforcement point for [ValidateListen]: no code
// path in this package binds a listener without going through it first (see
// also cmd/gofer's runDaemon, which calls ValidateListen separately, before
// this method, purely to fail fast with a clean CLI error before doing any
// other startup work — the check here is what actually protects a caller
// that skips that CLI convenience and calls Serve directly).
func (d *Daemon) Serve(ctx context.Context) error {
	if err := ValidateListen(d.cfg.ListenAddr, d.cfg.BearerToken); err != nil {
		return err
	}

	d.server = &http.Server{
		Addr:              d.cfg.ListenAddr,
		Handler:           d.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
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
