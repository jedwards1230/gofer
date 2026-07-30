package mcpconn

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/jedwards1230/agent-sdk-go/mcp"

	"github.com/jedwards1230/gofer/internal/config"
)

// clientName/clientVersion identify gofer to every MCP server it connects
// to, via the "initialize" handshake's clientInfo. Version is left generic —
// this package has no access to cmd/gofer's build-stamped version (internal
// packages do not import cmd/, and threading it through Config for a
// server-log-only field is not worth the coupling).
const (
	clientName    = "gofer"
	clientVersion = "dev"
)

// Dialer connects to one configured, enabled server and returns a live,
// already-[mcp.Client.Initialize]d [mcp.Client]. ctx is the MANAGER's
// long-lived context, not a per-attempt one: for stdio it is what ties the
// spawned subprocess's lifetime to the manager (cancelling ctx kills the
// process — matching [mcp.Start]'s own contract), while the actual
// connect-timeout bound comes from the Client's own WithConnectTimeout
// option (composed with ctx's deadline, if any — see agent-sdk-go/mcp's
// Client.Initialize doc), which is why connectTimeout/callTimeout are passed
// as plain durations here rather than a bounded ctx: the Dialer decides how
// to apply them (as Client options), not the caller.
//
// [Manager] calls the configured Dialer (default: [Dial]) from a
// per-server goroutine; a test replaces it via [Config.Dialer] to connect
// through an in-memory transport with no subprocess and no network — see
// mcp.NewStdio, the SDK's own test seam, which this package's tests reuse.
type Dialer func(ctx context.Context, srv config.MCPServer, connectTimeout, callTimeout time.Duration) (*mcp.Client, error)

// Dial is the production [Dialer]: it spawns a subprocess (stdio) or builds
// an HTTP client, performs the "initialize" handshake, and returns the live
// Client — or closes whatever it started and returns the error.
func Dial(ctx context.Context, srv config.MCPServer, connectTimeout, callTimeout time.Duration) (*mcp.Client, error) {
	switch srv.Transport() {
	case config.MCPTransportStdio:
		return dialStdio(ctx, srv, connectTimeout, callTimeout)
	case config.MCPTransportHTTP:
		return dialHTTP(ctx, srv, connectTimeout, callTimeout)
	default:
		// EnabledServers already filters this out before Manager ever calls a
		// Dialer, so reaching here is a caller bug, not a runtime condition.
		return nil, fmt.Errorf("mcpconn: server %q: unsupported or unresolved transport", srv.Name)
	}
}

// stdioProcess adapts a spawned server subprocess's stdin/stdout pipes (plus
// its *exec.Cmd) into an io.ReadWriteCloser, exactly matching the shape
// agent-sdk-go/mcp's own unexported processStdio uses internally for
// [mcp.Start] — duplicated here (rather than reachable another way) because
// [mcp.Start] itself accepts no per-server environment, which THIS package
// needs for [config.MCPServer.Env] (resolved [config.SecretRef]s). Wiring
// through the SDK's own exported [mcp.NewStdio] — built for exactly this
// purpose, per its doc ("the seam tests use... the production constructor
// that spawns and wires a real subprocess" is [mcp.Start], but NewStdio is
// the general-purpose one) — keeps this an application-level extension, not
// a reach into SDK internals.
type stdioProcess struct {
	stdin  io.WriteCloser
	stdout io.ReadCloser
	cmd    *exec.Cmd
}

func (p *stdioProcess) Write(b []byte) (int, error) { return p.stdin.Write(b) }
func (p *stdioProcess) Read(b []byte) (int, error)  { return p.stdout.Read(b) }

// processCloseGrace bounds how long Close waits for a polite exit (stdin
// EOF) before force-killing the process — the stdio parallel to
// internal/lspdiag's shutdownGrace. A well-behaved MCP server reads stdin in
// a loop and exits on EOF; one that does not (hung, wedged, or simply
// ignoring stdin — sleep(1) is not a hypothetical, it is exactly what a
// misconfigured "command" can point at) must still never be left running:
// relying solely on stdin-close would leak it forever.
const processCloseGrace = 2 * time.Second

// Close signals EOF (closing stdin, so a well-behaved server exits on its
// own), closes stdout, and waits for the process — force-killing it if it
// has not exited within processCloseGrace — so a Dial failure or a
// Manager.Close can NEVER leave a zombie or a leaked pipe fd behind,
// regardless of whether the far end cooperates.
func (p *stdioProcess) Close() error {
	closeErr := p.stdin.Close()
	_ = p.stdout.Close()

	done := make(chan struct{})
	go func() {
		_ = p.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(processCloseGrace):
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		<-done // Wait() always returns once the process is gone, killed or not
	}
	return closeErr
}

// dialStdio spawns srv.Command/Args with srv.Env resolved and merged over
// the current process's environment (never REPLACING it — a server that
// shells out to, say, git or npx still needs PATH). command/args/env are
// operator-configured server launch settings from config.json, not
// user/model input — the same posture [mcp.Start] and lsp.Start already
// take on this gosec finding.
func dialStdio(ctx context.Context, srv config.MCPServer, connectTimeout, callTimeout time.Duration) (*mcp.Client, error) {
	env, err := resolveEnv(srv.Env)
	if err != nil {
		return nil, fmt.Errorf("mcpconn: server %q: resolve env: %w", srv.Name, err)
	}

	cmd := exec.CommandContext(ctx, srv.Command, srv.Args...) // #nosec G204 -- operator-configured server launch command (config.json), not user/model input
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcpconn: server %q: stdin pipe: %w", srv.Name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("mcpconn: server %q: stdout pipe: %w", srv.Name, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mcpconn: server %q: start %s: %w", srv.Name, srv.Command, err)
	}

	rw := &stdioProcess{stdin: stdin, stdout: stdout, cmd: cmd}
	client := mcp.NewStdio(rw, mcp.WithConnectTimeout(connectTimeout), mcp.WithCallTimeout(callTimeout))
	if _, err := client.Initialize(ctx, mcp.ClientInfo{Name: clientName, Version: clientVersion}); err != nil {
		_ = client.Close() // reaps the subprocess — see stdioProcess.Close
		return nil, fmt.Errorf("mcpconn: server %q: initialize: %w", srv.Name, err)
	}
	return client, nil
}

// dialHTTP builds an HTTP client against srv.URL, applying srv.Headers
// (resolved [config.SecretRef]s) and — unless Headers already sets an
// explicit "Authorization" (case-insensitively, since Headers is the more
// specific/general mechanism) — srv.Auth as a bearer token. This is this
// package's chosen convention for Auth, since config.MCPServer's doc does
// not itself prescribe a header format.
func dialHTTP(ctx context.Context, srv config.MCPServer, connectTimeout, callTimeout time.Duration) (*mcp.Client, error) {
	httpOpts, hasAuthHeader, err := resolveHeaders(srv.Headers)
	if err != nil {
		return nil, fmt.Errorf("mcpconn: server %q: resolve headers: %w", srv.Name, err)
	}
	if !hasAuthHeader && srv.Auth != "" {
		token, err := srv.Auth.Resolve()
		if err != nil {
			return nil, fmt.Errorf("mcpconn: server %q: resolve auth: %w", srv.Name, err)
		}
		httpOpts = append(httpOpts, mcp.WithHTTPHeader("Authorization", "Bearer "+token))
	}

	client := mcp.NewHTTP(srv.URL, httpOpts, mcp.WithConnectTimeout(connectTimeout), mcp.WithCallTimeout(callTimeout))
	if _, err := client.Initialize(ctx, mcp.ClientInfo{Name: clientName, Version: clientVersion}); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("mcpconn: server %q: initialize: %w", srv.Name, err)
	}
	return client, nil
}

// resolveEnv resolves every SecretRef in env to "KEY=VALUE" pairs, sorted by
// key for deterministic ordering (map iteration order is not stable, and a
// stable argv/env matters for reproducing a failure).
func resolveEnv(env map[string]config.SecretRef) ([]string, error) {
	if len(env) == 0 {
		return nil, nil
	}
	keys := sortedKeys(env)
	out := make([]string, 0, len(env))
	for _, k := range keys {
		v, err := env[k].Resolve()
		if err != nil {
			return nil, fmt.Errorf("env[%s]: %w", k, err)
		}
		out = append(out, k+"="+v)
	}
	return out, nil
}

// resolveHeaders resolves every SecretRef in headers into [mcp.HTTPOption]s,
// sorted by key for deterministic request headers, and reports whether an
// "Authorization" header (case-insensitive) was among them.
func resolveHeaders(headers map[string]config.SecretRef) (opts []mcp.HTTPOption, hasAuthHeader bool, err error) {
	if len(headers) == 0 {
		return nil, false, nil
	}
	keys := sortedKeys(headers)
	opts = make([]mcp.HTTPOption, 0, len(headers))
	for _, k := range keys {
		v, rerr := headers[k].Resolve()
		if rerr != nil {
			return nil, false, fmt.Errorf("headers[%s]: %w", k, rerr)
		}
		opts = append(opts, mcp.WithHTTPHeader(k, v))
		if strings.EqualFold(k, "Authorization") {
			hasAuthHeader = true
		}
	}
	return opts, hasAuthHeader, nil
}

// sortedKeys returns m's keys in ascending order.
func sortedKeys(m map[string]config.SecretRef) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
