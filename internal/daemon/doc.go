// Package daemon hosts a [supervisor.Supervisor] behind a WebSocket listener
// speaking the Agent Client Protocol (ACP) v1 over JSON-RPC 2.0, plus a small
// set of gofer-native control methods (namespaced "gofer/*") for the CLI
// client. It serves two roles. As the M2 daemon it is the process an ACP
// client (an editor, or an iOS app on the tailnet) and gofer's own CLI both
// connect to. Under M6 process isolation the same code is also the
// single-session worker: [internal/worker] builds one with
// [Config.MaxSessions] of 1 over a unix socket, so a worker IS a
// single-session daemon (worker.go:254). Nothing here is router-aware —
// the router reaches a worker as an ordinary client of this package.
//
// # Transport ownership
//
// The SDK's acp package ([github.com/jedwards1230/agent-sdk-go/acp]) owns ACP
// message TYPES and the pure projection functions to/from the typed
// Event/Op contract; it does no networking and no JSON-RPC framing by design
// (see its package doc). This package owns everything acp does not: the
// WebSocket transport ([github.com/coder/websocket], chosen for its
// context-first API, zero transitive dependencies, and active maintenance —
// see [Daemon.Handler]), the JSON-RPC 2.0 envelope and error codes, and the
// method router that dispatches an inbound request either to an ACP
// projection or to a gofer-native handler.
//
// # One session subscription per session/prompt, fanned out to attached peers
//
// [Daemon]'s session/prompt handler subscribes ONCE to the target session's
// event stream, sends the prompt, and streams every projectable event back as
// a session/update notification until the next terminal turn.finished, which it
// answers with the JSON-RPC response. The contract is one outstanding prompt
// per session: a client must wait for a session/prompt response before sending
// another for the same session id, since a concurrent second subscription
// would race the first for the same terminal event. See
// [Daemon.handleSessionPrompt].
//
// Delivery is NOT originator-scoped. The daemon keeps a session->peers fan-out
// registry ([Daemon.sessionPeers]): a peer attaches to a session on
// session/load or by driving a session/prompt, and each projected
// session/update is broadcast to every attached peer — so a turn one client
// drives is seen live by every other client attached to the same session (a
// TUI observes a turn a phone drove). The peer set is snapshotted under a
// dedicated RWMutex and released before any socket write, so no client's write
// stalls the registry; a non-originating peer's write failure is logged and
// skipped, while only the originator's failure aborts its own RPC. One
// exception to the broadcast: the user-message echo (a settled user prompt) is
// suppressed to the originator, which already knows what it typed, and
// delivered to every other attached peer. Peers are deregistered on connection
// close (see [Daemon.detachPeer]). See [Daemon.broadcastUpdate].
//
// # Auth
//
// A daemon configured with [Config.BearerToken] requires it on every
// WebSocket upgrade, checked in constant time before the HTTP connection is
// upgraded (see [Daemon.Handler]). ACP's own "authenticate" method is
// answered as a no-op success — the bearer token is the daemon's only auth
// boundary in M2.
//
// # Logging
//
// [Config.Logger] (default: a discarding logger, so an embedder that passes
// none stays silent) receives structured, leveled logs: connection open/
// close and accept failures at INFO/WARN (remote addr only — see
// [Daemon.serveWS]), per-request method/id/outcome/duration at INFO and
// unknown-method/parse-failure warnings at WARN (see [peer.handleFrame], the
// router's single logging chokepoint), notifications at DEBUG, and session
// lifecycle (create/resume/kill/archive) at INFO (see handlers.go). The hard
// rule everywhere: log method names, JSON-RPC ids, error codes/messages,
// durations, remote addrs, and session ids — never request/response params,
// prompt text, message content, tool inputs/outputs, or the bearer token.
package daemon
