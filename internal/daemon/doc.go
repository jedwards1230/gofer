// Package daemon hosts a [supervisor.Supervisor] behind a WebSocket listener
// speaking the Agent Client Protocol (ACP) v1 over JSON-RPC 2.0, plus a small
// set of gofer-native control methods (namespaced "gofer/*") for the CLI
// client. It is the M2 daemon: the process an ACP client (an editor, or an
// iOS app on the tailnet) and gofer's own CLI both connect to.
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
// # One session subscription per session/prompt
//
// [Daemon]'s session/prompt handler subscribes to the target session's event
// stream, sends the prompt, and streams every projectable event back as a
// session/update notification until the next terminal turn.finished, which it
// answers with the JSON-RPC response. M2's contract is one outstanding prompt
// per session: a client must wait for a session/prompt response before
// sending another for the same session id, since a concurrent second
// subscription would race the first for the same terminal event. See
// [Daemon.handleSessionPrompt].
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
