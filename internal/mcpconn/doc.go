// Package mcpconn is gofer's MCP connection manager: the consuming-side
// piece the SDK's optional agent-sdk-go/mcp package deliberately does not
// build (see that package's doc — "server configuration, credential
// resolution, the connection manager, and any tool-index decorator are the
// consuming application's job").
//
// # Lifecycle
//
// One [Manager] is built and [Manager.Start]ed once per gofer process — the
// same shape as internal/lspdiag.Manager, and for the same reason: it is
// closed exactly once, by whatever built it (internal/supervisor.Supervisor,
// mirroring its lspManager field), not per session. Start connects every
// [config.MCP.EnabledServers] server ASYNCHRONOUSLY — it returns immediately,
// launching one goroutine per server — so building and starting a Manager
// never blocks on a slow or unreachable server. Each server goroutine
// connects, and on failure retries with capped exponential backoff
// ([config.MCP.RetryMaxInterval]); once connected it is periodically
// health-checked (a cheap "tools/list"), and a failed check tears the
// connection down and re-enters the same connect/backoff loop — so a server
// that dies mid-run is reconnected without anyone asking.
//
// # A session's tool set is fixed at create
//
// [Manager.Snapshot] is the ONLY way tools reach a session, and it is a
// point-in-time read: whatever a session's sessionGuard captures at Create
// (see internal/supervisor) is that session's MCP tool set for its whole
// life. Nothing in this package ever mutates a []tool.Tool it has already
// handed out — a server connecting after a session started joins the NEXT
// session's snapshot, never the live one. This mirrors, and is required by,
// the SDK mcp package's own hard invariant (see its doc and
// docs/DESIGN.md's "MCP (M7)" section) — the toolindex decorator
// (agent-sdk-go#114) stakes a prompt-cache byte-identity guarantee on a
// session's registered tool array never growing after Wrap.
//
// # Reconnect without re-registration
//
// The SDK's own mcp.Project binds each projected tool.Tool to ONE *mcp.Client
// instance — fine for a one-shot snapshot, but useless for "a dead server's
// tools work again after reconnect" as required here, because a dead
// client's transport cannot come back to life; only a fresh Client can. So
// this package does not register the SDK's projected tools directly. Instead
// [proxyTool] (tool.go) captures a tool's name/description/schema once (from
// mcp.Project, so naming/sanitization/schema-conversion stay the SDK's
// single implementation) and resolves the CURRENT live *mcp.Client through
// the Manager on every Run — so the exact same tool.Tool object a session
// registered keeps working across a reconnect with no registry mutation.
//
// # Failure is always advisory to the caller of Run
//
// A [proxyTool] whose server has no live connection returns
// tool.Result{IsError: true}, never a Go error (unless ctx itself ended the
// call) — same resilience contract as mcp.projectedTool.Run, extended to
// "never connected yet" as well as "died mid-call".
package mcpconn
