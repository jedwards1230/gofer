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

// relayWriteTimeout bounds ONE fan-out — every peer write performed for a
// single observed event, whether by [EventRelay]/[PermissionRelay] or by
// [handleSessionPrompt]'s own per-turn broadcasts — so a stalled client cannot
// wedge the caller that drives it.
//
// It is ALSO what gives every NON-ORIGIN peer write a context the write path
// owns instead of one borrowed from another peer. coder/websocket's Write
// registers a context.AfterFunc that CLOSES THE WHOLE CONNECTION when the
// write's context is cancelled, so fanning out to peer B under peer A's request
// context means A disconnecting mid-turn tears down B's healthy connection. In
// M6's geometry B is frequently a router's link to a live worker, so a client
// hanging up would mark a running session offline. Deriving from d.ctx breaks
// that coupling: it is cancelled only on daemon shutdown, when closing IS
// correct. The one deliberate exception is the ORIGIN peer's write in
// [Daemon.broadcastUpdate] — see its doc.
//
// It exists because the relays are called SYNCHRONOUSLY on an M6 router's
// per-worker wirestream demuxer goroutine, and that goroutine is the sole
// drainer of its [Client.notifications] channel. Without a deadline the chain
// is: one client whose TCP connection is stalled-but-open blocks the relay
// write -> the demuxer stops draining -> [Client.readLoop] blocks on the full
// notification channel -> EVERY Call to that worker hangs, including
// gofer/roster, gofer/kill, gofer/archive and session/prompt. The session
// becomes unkillable over its own socket and never recovers on its own, since
// nothing cancels the daemon's base context. A bounded write turns that
// permanent wedge into a logged, skipped delivery.
//
// The bound is per FAN-OUT, not per peer: N stalled peers must not multiply
// into N * relayWriteTimeout of demuxer stall, so they share one budget. A
// dropped relay frame is not a durability loss — the journal is the durable
// transcript and a client re-reads it as folded history on the next
// session/load — which is what makes a deadline the right trade here.
//
// 5s: a healthy peer drains a notification in microseconds, so this is orders
// of magnitude of headroom for a slow-but-live client while still being well
// inside the ping watchdog (pingInterval + pingTimeout) that eventually tears
// a genuinely dead connection down.
const relayWriteTimeout = 5 * time.Second

// Config configures a [Daemon].
type Config struct {
	// ListenAddr is the address Serve binds. Empty uses [DefaultListenAddr].
	ListenAddr string
	// BearerToken, when non-empty, is required of every WebSocket upgrade
	// (see [Daemon.Handler]). Empty disables auth — appropriate only for a
	// loopback-bound daemon.
	BearerToken string
	// DefaultModel is the fallback a session/new request resolves to when it
	// supplies no (or an empty) model, and the model a session/load request
	// always resolves to (ACP's LoadSessionRequest has no model field).
	// Callers resolve this the same way `gofer run` does (the sole logged-in
	// provider's model) before constructing Config; the daemon does not
	// re-derive it.
	DefaultModel string
	// Version is the daemon's build version (cmd/gofer's effectiveVersion()),
	// surfaced verbatim as gofer/hello's binaryVersion so a router/peer can
	// detect version skew in-band (design §6). Empty ("") when a caller does not
	// set it — hello then reports an empty binaryVersion rather than failing, the
	// same "unknown → skip" posture the Endpoint.Version skew check takes.
	Version string
	// Logger receives the daemon's structured logs (connection lifecycle,
	// per-request outcome, session lifecycle — see the package doc's Logging
	// section). Nil defaults to a discarding logger in [New], so embedders and
	// tests that pass no logger stay silent rather than hitting a nil
	// dereference.
	Logger *slog.Logger
	// AuthedProviders reports the set of provider ids the daemon host currently
	// has a usable credential for, so gofer/models can stamp each model's
	// Available flag (a remote client cannot observe the host's auth state
	// itself). Nil, or a non-nil error, is treated as "no provider
	// authenticated" — gofer/models still returns the full model list, every
	// entry Available:false — never a reason to fail model discovery. Mirrors
	// the TUI CommandEnv.Auth non-fatal contract (internal/tui/modelpicker.go).
	AuthedProviders func() (map[string]bool, error)

	// MaxSessions, when > 0, caps how many LIVE sessions this daemon will
	// host: once the roster already holds that many, session/new is refused
	// with a clean application error instead of creating another (see
	// handleSessionNew). Zero — the default — means unlimited, the historical
	// `gofer daemon` behavior, byte-for-byte unchanged. The M6 session-worker
	// (docs/milestones/M6-process-isolation.md) sets it to 1 so a worker IS a
	// single-session daemon; ordinary daemons leave it at 0.
	MaxSessions int

	// ReplayPendingPermissionsOnAttach makes session/load re-broadcast a
	// session's still-OUTSTANDING permission requests to the newly attached peer
	// (as gofer/permission_requested notifications, before the load response),
	// so a client that attaches AFTER a turn already asked re-surfaces the open
	// request and can answer it. The one caller that needs it is the M6 router
	// adopting a worker whose turn is blocked on a gate mid-approval
	// (docs/milestones/M6-process-isolation.md §7): the turn's original fan-out
	// died with the previous router's connection, so the retained request only
	// reaches the new router if the worker re-emits it on the adoption attach.
	// The single-session worker sets it; the default `gofer daemon` and
	// daemonless paths leave it false, so their session/load is byte-for-byte
	// unchanged.
	ReplayPendingPermissionsOnAttach bool
}

// Daemon hosts a [Supervisor] behind an ACP-over-WebSocket listener. See the
// package doc for the transport and streaming contract. The hosted registry is
// the interface, not the concrete *[supervisor.Supervisor], so the same surface
// can front a remote proxy (the M6 router→worker relationship).
type Daemon struct {
	sup Supervisor
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

	// promptMu guards promptHandlers.
	promptMu sync.Mutex
	// promptHandlers counts the live [handleSessionPrompt] loops per session id.
	// A non-zero count means THIS daemon is already fanning that session's events
	// out off its own subscription, so the M6 event relay (see event_relay.go)
	// must stand down for it or every attached peer would receive each event
	// twice. Entries exist only while a prompt is in flight — the handler's defer
	// deletes its own at zero — so the map is bounded by concurrent prompts, not
	// by sessions.
	promptHandlers map[string]int

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

	// pendingPermsMu guards pendingPerms.
	pendingPermsMu sync.Mutex
	// pendingPerms maps an OUTSTANDING permission request's call id to its full
	// requested params (session id + tool/spec/trace) — the payload needed to
	// re-broadcast it verbatim to a peer that attaches while the gate is still
	// held. Populated where a permission.requested is first broadcast (alongside
	// permRoutes), cleared on its permission.resolved: one entry per open gate,
	// the same lifetime and bound as permRoutes. Read only when
	// [Config.ReplayPendingPermissionsOnAttach] is set (the M6 worker); an
	// ordinary daemon populates and clears it but never replays from it, so its
	// attach path is unchanged.
	pendingPerms map[string]permissionRequestedParams

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
// tests). sup is any [Supervisor]; the in-process *[supervisor.Supervisor] is
// the usual one.
func New(sup Supervisor, cfg Config) *Daemon {
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
		promptHandlers: make(map[string]int),
		permRoutes:     make(map[string]string),
		pendingPerms:   make(map[string]permissionRequestedParams),
		permReqCancels: make(map[string]context.CancelFunc),
	}
}

// authedProviders resolves [Config.AuthedProviders] non-fatally: a nil closure
// or a non-nil error both yield a nil map, which [toModelInfoDTOs] treats as
// "no provider authenticated" (a nil-map index is false). Model discovery must
// never fail on the host's auth state being unreadable — see the field doc.
func (d *Daemon) authedProviders() map[string]bool {
	if d.cfg.AuthedProviders == nil {
		return nil
	}
	authed, err := d.cfg.AuthedProviders()
	if err != nil {
		return nil
	}
	return authed
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
// is broadcast. It returns whether this was the FIRST time id was routed (the
// route was absent): a caller that ALSO broadcasts uses the bool to broadcast a
// given request exactly once even when two observers see the same
// PermissionRequested — the ordinary prompt handler AND an adopted session's
// standing permission watcher (see [Daemon.RequestPermission]). Every existing
// (single-observer) caller sees each call id exactly once, so first is always
// true for them and their behavior is unchanged.
func (d *Daemon) recordPermRoute(id, sessionID string) (first bool) {
	d.permMu.Lock()
	_, existed := d.permRoutes[id]
	d.permRoutes[id] = sessionID
	d.permMu.Unlock()
	return !existed
}

// clearPermRoute drops id's route. Idempotent: it runs both eagerly in
// handlePermissionReply (to close the reply→resolved window) and again on the
// PermissionResolved event, and may run from two observers of an adopted
// session, so it must tolerate an already-absent route.
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

// recordPendingPerm remembers an outstanding permission request's full params
// so it can be re-broadcast verbatim to a peer that attaches while the gate is
// still held (see [Config.ReplayPendingPermissionsOnAttach]). Called alongside
// recordPermRoute as a permission.requested is first broadcast.
func (d *Daemon) recordPendingPerm(id string, params permissionRequestedParams) {
	d.pendingPermsMu.Lock()
	d.pendingPerms[id] = params
	d.pendingPermsMu.Unlock()
}

// clearPendingPerm drops id's retained request once it has resolved and reports
// whether an entry was actually present. Unlike the route (cleared eagerly in
// handlePermissionReply), the retained request is dropped ONLY on the
// PermissionResolved event, so its presence is the reliable signal for
// broadcasting a resolution exactly once across two observers of an adopted
// session (the standing watcher and a concurrent prompt handler) — the
// resolve-side counterpart of recordPermRoute's first bool.
func (d *Daemon) clearPendingPerm(id string) (cleared bool) {
	d.pendingPermsMu.Lock()
	_, existed := d.pendingPerms[id]
	delete(d.pendingPerms, id)
	d.pendingPermsMu.Unlock()
	return existed
}

// pendingPermsForSession snapshots every outstanding permission request for
// sessionID — the payloads [handleSessionLoad] replays to a peer that attaches
// mid-approval when [Config.ReplayPendingPermissionsOnAttach] is set. Order is
// unspecified (map iteration); a client answers by call id regardless.
func (d *Daemon) pendingPermsForSession(sessionID string) []permissionRequestedParams {
	d.pendingPermsMu.Lock()
	defer d.pendingPermsMu.Unlock()
	var out []permissionRequestedParams
	for _, params := range d.pendingPerms {
		if params.SessionID == sessionID {
			out = append(out, params)
		}
	}
	return out
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
