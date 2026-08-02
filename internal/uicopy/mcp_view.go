package uicopy

// mcp_view.go holds the /mcp tab's copy (internal/tui/mcp_view.go).
//
// The schema-mode names ("index", "preload") the tab switches on are CONFIG
// values arriving from the backend, not copy, and stay in internal/tui — only
// the sentences built around them live here.

import "strconv"

// Subjects the /mcp tab hands the shared loading/unknown capability bodies.
const (
	MCPLoadingSubject = "MCP servers"
	MCPUnknownSubject = "MCP"
)

// The empty state, which must never read like the UNKNOWN body above it: they
// are opposite claims, and an Ascii golden sees only the words.
const (
	MCPNoServers     = "MCP servers: none configured."
	MCPAddServerHint = "Add one under mcp.servers in config.json."
)

// MCPServerCounts heads the list with how many configured servers are up.
func MCPServerCounts(connected, configured int) string {
	return "MCP servers: " + strconv.Itoa(connected) + " connected of " + strconv.Itoa(configured) + " configured"
}

// Per-server state words. Each situation reads differently in plain text,
// since the goldens pinning this view are colour-blind by construction.
const (
	MCPStateDisabled             = "disabled"
	MCPStateUnsupportedTransport = "unsupported transport"
	MCPStateConnected            = "connected"
	MCPStateNotConnected         = "not connected"
)

// MCPTransportConfigured labels a server's transport as the CONFIGURED one
// rather than an observed connection.
func MCPTransportConfigured(transport string) string {
	return transport + " (configured)"
}

// MCPFederatedTools totals the tools reachable across connected servers.
func MCPFederatedTools(n int) string {
	return "Federated tools: " + strconv.Itoa(n) + " total across connected servers"
}

// MCPConfigDrifted explains why a just-configured server is missing from the
// list: the process read config.json at startup.
const MCPConfigDrifted = "config.json changed since startup — restart gofer to apply"

// MCPOmissions names what the tab deliberately does not show. Naming the issue
// makes the line self-retiring: when gofer#302 lands, the data exists and this
// line goes with it.
const MCPOmissions = "Not reported (gofer#302): per-server tools, down-reason, down-since"

// MCPSchemaModeIndex reports the index schema mode's resident/index-only split.
func MCPSchemaModeIndex(indexOnly, resident int) string {
	return "Tool schemas: index (configured) — " +
		strconv.Itoa(indexOnly) + " index-only, " +
		strconv.Itoa(resident) + " resident"
}

// MCPSchemaModePreload reports the preload schema mode.
const MCPSchemaModePreload = "Tool schemas: preload (configured) — every schema preloaded"

// MCPSchemaModeUnrecognized names a schema mode this build has never heard of
// — a newer daemon's — WITHOUT claiming preload semantics for it.
func MCPSchemaModeUnrecognized(mode string) string {
	return "Tool schemas: " + mode + " (configured) — unrecognized by this gofer"
}
