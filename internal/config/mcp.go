package config

import (
	"fmt"
	"regexp"
	"time"
)

// MCPTransport is the resolved form of an [MCPServer]'s transport, explicit
// or inferred; see [MCPServer.Transport].
type MCPTransport string

const (
	MCPTransportStdio MCPTransport = "stdio"
	MCPTransportHTTP  MCPTransport = "http"
	// MCPTransportUnsupported is both the zero value and the fail-safe
	// result of an unrecognized or unresolvable transport; see
	// [MCPServer.Transport].
	MCPTransportUnsupported MCPTransport = ""
)

const (
	// DefaultMCPConnectTimeout is [MCP.ConnectTimeoutMS]'s default: 30s. A
	// cold npx/uvx stdio server can spend tens of seconds spawning and
	// answering initialize, and the connection manager connects
	// ASYNCHRONOUSLY, so a generous bound here never delays a session
	// create.
	DefaultMCPConnectTimeout = 30 * time.Second
	// DefaultMCPCallTimeout is [MCP.CallTimeoutMS]'s default: 60s. A remote
	// tool call is a network round trip plus real work on the far end; a
	// hung server must eventually let go rather than holding a turn open
	// forever.
	DefaultMCPCallTimeout = 60 * time.Second
	// DefaultMCPReadyTimeout is [MCP.ReadyTimeoutMS]'s default: 2s — the
	// same bounded best-effort wait and rationale as
	// [DefaultLoadSettleTimeout]: a short window to observe readiness
	// before proceeding without it.
	DefaultMCPReadyTimeout = 2 * time.Second
	// DefaultMCPRetryMaxInterval is [MCP.RetryMaxIntervalMS]'s default:
	// 60s. A server that is down tends to stay down for minutes, so
	// retrying once a minute finds it again promptly without hammering a
	// service that is legitimately offline.
	DefaultMCPRetryMaxInterval = 60 * time.Second
)

// mcpServerNameRE is [MCPServer.Name]'s grammar: it becomes part of the
// federated tool name `mcp__<name>__<tool>`, which the permission grammar
// globs on and which providers cap at 64 chars of [A-Za-z0-9_-] total —
// bounding the server name well below that leaves room for `__<tool>`.
var mcpServerNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// MCP configures the MCP servers gofer connects to and the timeouts that
// govern them. The zero value is fully valid: no servers configured, and
// every timeout resolves to its package default via the resolvers below.
type MCP struct {
	// Servers is the configured server list; see [MCPServer].
	Servers []MCPServer `json:"servers,omitempty"`
	// ConnectTimeoutMS is the section-wide connect bound; see
	// [MCP.ConnectTimeout]. A server may override it via
	// [MCPServer.ConnectTimeoutMS].
	ConnectTimeoutMS *int `json:"connect_timeout_ms,omitempty"`
	// CallTimeoutMS is the section-wide per-call bound; see
	// [MCP.CallTimeout]. A server may override it via
	// [MCPServer.CallTimeoutMS].
	CallTimeoutMS *int `json:"call_timeout_ms,omitempty"`
	// ReadyTimeoutMS bounds the best-effort wait for a server to report
	// ready before gofer proceeds without it; see [MCP.ReadyTimeout].
	ReadyTimeoutMS *int `json:"ready_timeout_ms,omitempty"`
	// RetryMaxIntervalMS caps the backoff between reconnect attempts to a
	// server that failed to connect; see [MCP.RetryMaxInterval].
	RetryMaxIntervalMS *int `json:"retry_max_interval_ms,omitempty"`
}

// MCPServer configures one MCP server connection: stdio (Command/Args/Env)
// or HTTP (URL/Headers/Auth), never both — see [Config.validate]. Allow/Deny
// filter which of the server's tools gofer projects, in the same glob
// grammar [Rule.Specifier] uses.
type MCPServer struct {
	// Name identifies the server in config and becomes part of the
	// federated tool name `mcp__<name>__<tool>`; see [mcpServerNameRE] and
	// [Config.validate].
	Name string `json:"name"`
	// TransportMode is the explicit transport ("stdio" or "http"); empty
	// infers from which of Command/URL is set. Named *Mode (like
	// [Tools.SchemaMode]/[Session.PermissionMode]) rather than plain
	// Transport, because the resolved value's own type already claims that
	// name — see [MCPServer.Transport].
	TransportMode string `json:"transport,omitempty"`
	// Enabled defaults to true (nil or true): a server you bothered to
	// configure should run — the same *bool default-on rationale as
	// [TUI.Autoscroll]. See [MCPServer.IsEnabled].
	Enabled *bool `json:"enabled,omitempty"`

	// Command + Args + Env are the stdio transport's launch spec.
	Command string               `json:"command,omitempty"`
	Args    []string             `json:"args,omitempty"`
	Env     map[string]SecretRef `json:"env,omitempty"`

	// URL + Headers + Auth are the http transport's connection spec.
	URL     string               `json:"url,omitempty"`
	Headers map[string]SecretRef `json:"headers,omitempty"`
	Auth    SecretRef            `json:"auth,omitempty"`

	// Allow/Deny filter this server's projected tools by name glob; an
	// empty Allow means "every tool this server offers".
	Allow []string `json:"allow,omitempty"`
	Deny  []string `json:"deny,omitempty"`

	// ConnectTimeoutMS/CallTimeoutMS override the section defaults for
	// THIS server only; nil falls through to
	// [MCP.ConnectTimeout]/[MCP.CallTimeout] via
	// [MCPServer.ConnectTimeout]/[MCPServer.CallTimeout].
	ConnectTimeoutMS *int `json:"connect_timeout_ms,omitempty"`
	CallTimeoutMS    *int `json:"call_timeout_ms,omitempty"`
}

// ConnectTimeout resolves [MCP.ConnectTimeoutMS]'s effective value:
// [DefaultMCPConnectTimeout] when unset or non-positive.
func (m MCP) ConnectTimeout() time.Duration {
	if m.ConnectTimeoutMS == nil || *m.ConnectTimeoutMS <= 0 {
		return DefaultMCPConnectTimeout
	}
	return time.Duration(*m.ConnectTimeoutMS) * time.Millisecond
}

// CallTimeout resolves [MCP.CallTimeoutMS]'s effective value:
// [DefaultMCPCallTimeout] when unset or non-positive.
func (m MCP) CallTimeout() time.Duration {
	if m.CallTimeoutMS == nil || *m.CallTimeoutMS <= 0 {
		return DefaultMCPCallTimeout
	}
	return time.Duration(*m.CallTimeoutMS) * time.Millisecond
}

// ReadyTimeout resolves [MCP.ReadyTimeoutMS]'s effective value:
// [DefaultMCPReadyTimeout] when unset or non-positive.
func (m MCP) ReadyTimeout() time.Duration {
	if m.ReadyTimeoutMS == nil || *m.ReadyTimeoutMS <= 0 {
		return DefaultMCPReadyTimeout
	}
	return time.Duration(*m.ReadyTimeoutMS) * time.Millisecond
}

// RetryMaxInterval resolves [MCP.RetryMaxIntervalMS]'s effective value:
// [DefaultMCPRetryMaxInterval] when unset or non-positive.
func (m MCP) RetryMaxInterval() time.Duration {
	if m.RetryMaxIntervalMS == nil || *m.RetryMaxIntervalMS <= 0 {
		return DefaultMCPRetryMaxInterval
	}
	return time.Duration(*m.RetryMaxIntervalMS) * time.Millisecond
}

// EnabledServers returns the subset of m.Servers gofer should connect to:
// [MCPServer.IsEnabled] and a resolvable ([MCPServer.Transport] !=
// [MCPTransportUnsupported]) server. A server with an unrecognized
// transport is skipped here, silently as far as the caller is concerned —
// see [MCPServer.Transport]'s doc for why that server is a warning, not a
// load error.
func (m MCP) EnabledServers() []MCPServer {
	out := make([]MCPServer, 0, len(m.Servers))
	for _, s := range m.Servers {
		if s.IsEnabled() && s.Transport() != MCPTransportUnsupported {
			out = append(out, s)
		}
	}
	return out
}

// IsEnabled resolves [MCPServer.Enabled]'s effective value: true (the
// default) when nil, else the explicit stored value.
func (s MCPServer) IsEnabled() bool {
	return s.Enabled == nil || *s.Enabled
}

// Transport resolves [MCPServer.TransportMode]'s effective value: the
// explicit spelling when it is one gofer recognizes, else INFERRED from
// which connection fields are set — Command present means stdio, URL
// present means http — else [MCPTransportUnsupported].
//
// Unlike a guardrail knob, this fails safe to Unsupported rather than a
// hard [Config.validate] error, and [MCP.EnabledServers] skips it with a
// warning rather than reinterpreting it as a transport that happens to be
// wired up. The asymmetry with e.g. a bad permission verdict is
// deliberate: a permission typo can silently WIDEN policy — a real
// security hazard, so it fails the whole config load. An unknown transport
// (a future "ws", a typo) costs exactly one absent capability; taking the
// entire daemon down over one misconfigured server would be the worse
// failure.
func (s MCPServer) Transport() MCPTransport {
	switch MCPTransport(s.TransportMode) {
	case MCPTransportStdio, MCPTransportHTTP:
		return MCPTransport(s.TransportMode)
	case MCPTransportUnsupported: // TransportMode is unset ("") — infer.
		switch {
		case s.Command != "":
			return MCPTransportStdio
		case s.URL != "":
			return MCPTransportHTTP
		default:
			return MCPTransportUnsupported
		}
	default:
		// TransportMode is set but not one gofer recognizes (a typo, a
		// future "ws"): fail safe to Unsupported WITHOUT falling through
		// to inference — an explicit value the operator wrote must never
		// be silently reinterpreted as a different transport just because
		// Command or URL happens to be set too.
		return MCPTransportUnsupported
	}
}

// ConnectTimeout resolves this server's connect timeout: its own
// ConnectTimeoutMS override when positive, else m's section default via
// [MCP.ConnectTimeout].
func (s MCPServer) ConnectTimeout(m MCP) time.Duration {
	if s.ConnectTimeoutMS != nil && *s.ConnectTimeoutMS > 0 {
		return time.Duration(*s.ConnectTimeoutMS) * time.Millisecond
	}
	return m.ConnectTimeout()
}

// CallTimeout resolves this server's call timeout: its own CallTimeoutMS
// override when positive, else m's section default via [MCP.CallTimeout].
func (s MCPServer) CallTimeout(m MCP) time.Duration {
	if s.CallTimeoutMS != nil && *s.CallTimeoutMS > 0 {
		return time.Duration(*s.CallTimeoutMS) * time.Millisecond
	}
	return m.CallTimeout()
}

// validate rejects an [MCPServer] with no Name, a Name that does not match
// [mcpServerNameRE], a duplicate Name (silently shadowing another server's
// tools is a real hazard, not a style nit), both a Command and a URL set
// (which transport wins would be undefined), or any [SecretRef] with no
// recognized scheme.
func (m MCP) validate() error {
	seen := make(map[string]bool, len(m.Servers))
	for i, s := range m.Servers {
		if s.Name == "" {
			return fmt.Errorf("mcp.servers[%d]: name is required", i)
		}
		if !mcpServerNameRE.MatchString(s.Name) {
			return fmt.Errorf("mcp.servers[%d]: name %q must match %s", i, s.Name, mcpServerNameRE.String())
		}
		if seen[s.Name] {
			return fmt.Errorf("mcp.servers[%d]: duplicate server name %q", i, s.Name)
		}
		seen[s.Name] = true
		if s.Command != "" && s.URL != "" {
			return fmt.Errorf("mcp.servers[%d] %q: command and url are mutually exclusive", i, s.Name)
		}
		if err := s.Auth.validate(); err != nil {
			return fmt.Errorf("mcp.servers[%d] %q: auth: %w", i, s.Name, err)
		}
		for k, v := range s.Env {
			if err := v.validate(); err != nil {
				return fmt.Errorf("mcp.servers[%d] %q: env[%s]: %w", i, s.Name, k, err)
			}
		}
		for k, v := range s.Headers {
			if err := v.validate(); err != nil {
				return fmt.Errorf("mcp.servers[%d] %q: headers[%s]: %w", i, s.Name, k, err)
			}
		}
	}
	return nil
}
