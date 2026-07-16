package daemon

import (
	"fmt"
	"net"
)

// ValidateListen reports an error when addr is a non-loopback bind with no
// bearer token configured.
//
// This matters because a reachable daemon runs tool calls — including
// bash — against whatever permission mode the caller's session is
// configured with; a network-reachable daemon with no bearer token lets
// anyone who can route a packet to it drive tool calls as if they were an
// authenticated client. Reaching the daemon without a token is therefore
// equivalent to running arbitrary code as the daemon's user.
// Binding a non-loopback address (a tailnet IP, a LAN IP, or a bind-all
// address like "0.0.0.0"/"::") with no bearer token would leave that RCE
// surface open to anyone who can route a packet to it.
//
// A loopback bind (127.0.0.0/8, ::1, or the "localhost" hostname) is exempt
// — it is reachable only from processes already running as the same user on
// the same machine, so it carries no additional token-free exposure and stays
// the dev-friendly default. Every other host, including an empty host
// ("" — Go's net package binds that as "all interfaces", same as
// "0.0.0.0"/"::") and any address net.ParseIP does not recognize, is treated
// as non-loopback and requires a token.
func ValidateListen(addr, token string) error {
	if token != "" {
		return nil
	}
	if isLoopbackAddr(addr) {
		return nil
	}
	return fmt.Errorf("refusing to bind non-loopback %q without a bearer token; pass --token or set GOFER_TOKEN", addr)
}

// isLoopbackAddr reports whether addr's host is loopback-only: "localhost",
// or an IP for which [net.IP.IsLoopback] is true. A bind-all/unspecified
// address (empty host, "0.0.0.0", "::") and anything net.ParseIP can't parse
// are NOT loopback — they require a token per [ValidateListen].
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// addr has no ":port" suffix (or is otherwise malformed) — fall back
		// to treating addr itself as the host, so a bare "127.0.0.1" (no
		// port) is still recognized as loopback rather than defaulting to
		// "require a token" on a technicality.
		host = addr
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
