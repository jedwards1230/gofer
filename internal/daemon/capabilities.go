package daemon

// capabilities.go is the gofer/capabilities method: the runtime MCP/skills
// report a daemon-attached TUI's /mcp and /skills panels render (gofer#303),
// end to end — the optional supervisor capability, the method const, its
// registration, the handler, and the *[Client] half.
//
// It is a file of its own rather than three edits spread across handlers.go /
// client.go purely to avoid a file-level collision with concurrent work in
// this package (gofer#280 holds daemon.go/handlers.go/observer.go). The one
// consequence is the init() below; everything else follows the existing
// shapes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jedwards1230/gofer/internal/capability"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// methodGoferCapabilities is gofer-native runtime capability reporting: which
// MCP servers this daemon has configured and connected, and which skills a
// session created in the caller's cwd would load. Read-only.
//
// A daemon whose supervisor cannot answer replies {supported:false} rather
// than failing, and an older daemon replies method-not-found; a client treats
// both as UNKNOWN — see [ErrCapabilitiesUnsupported].
const methodGoferCapabilities = "gofer/capabilities"

// CapabilityReporter is an OPTIONAL [Supervisor] capability: a point-in-time
// report of the MCP connections and skills the supervisor itself owns. The
// in-process supervisor implements it — it holds the [mcpconn.Manager] and the
// store root; the M6 router deliberately does NOT, and cannot: under
// `gofer daemon --workers` the router process owns no supervisor and no MCP
// manager at all (each session's worker process owns its own), so there is no
// single fleet-wide answer for it to give.
//
// It is a separate interface rather than a method on [Supervisor] for exactly
// the reason [FleetUsager] and [DecisionAnswerer] are: so a supervisor that
// cannot serve it stays untouched, and so the gap is explicit rather than a
// silently empty answer. [handleGoferCapabilities] type-asserts for it and
// reports {supported:false} when it is absent — which a client must render as
// UNKNOWN, never as "no MCP servers configured".
//
// cwd is the CLIENT's working directory, forwarded so the report answers "what
// would a session started there load"; the paths are resolved on the DAEMON's
// filesystem, which is the only filesystem a session it hosts can read.
type CapabilityReporter interface {
	Capabilities(cwd string) capability.Snapshot
}

// The in-process supervisor satisfies the capability report — a signature
// drift in either package fails the build here rather than at the assertion.
var _ CapabilityReporter = (*supervisor.Supervisor)(nil)

// init registers the method rather than adding a line to handlers.go's
// [methodTable] literal, to keep this change off a file another workstream
// holds open (see the file doc). It is safe and ordered: package-level
// variable initialization completes before any init function runs, so
// methodTable is a live map by the time this executes, and [peer.handleFrame]
// only reads it once a connection is served. [isGoferNativeMethod] classifies
// the name from its "gofer/" prefix, so no second registration is needed.
func init() {
	// Assignment into a map is silent about collisions, and this registration
	// is the one in the package that is not visible in handlers.go's literal —
	// so a future entry there for the same method would clobber one of the two
	// handlers with nothing to notice. Fail at process start instead: this is a
	// programming error in gofer's own source, reachable by no input.
	if _, dup := methodTable[methodGoferCapabilities]; dup {
		panic("daemon: duplicate registration of " + methodGoferCapabilities)
	}
	methodTable[methodGoferCapabilities] = handleGoferCapabilities
}

// capabilitiesParams is gofer/capabilities' request shape.
type capabilitiesParams struct {
	// Cwd is the client's working directory, for resolving the project skills
	// directory (<cwd>/.gofer/skills). Optional: an omitted value reports on
	// the daemon's store root ALONE, with no project directory — it is never
	// resolved against the daemon's own working directory, which would answer
	// about a directory the caller never named. See
	// [supervisor.Supervisor.Capabilities].
	Cwd string `json:"cwd,omitempty"`
}

// capabilitiesResult is gofer/capabilities' response shape. Supported=false
// carries a zero Snapshot that a client must NOT read — mirroring
// [fleetUsageDTO]'s supported flag.
type capabilitiesResult struct {
	Supported bool                `json:"supported"`
	Snapshot  capability.Snapshot `json:"snapshot"`
}

// handleGoferCapabilities answers gofer/capabilities. Read-only; never fails
// beyond malformed params.
func handleGoferCapabilities(d *Daemon, _ context.Context, _ *peer, params json.RawMessage) (any, *rpcError) {
	var p capabilitiesParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, invalidParams(err)
		}
	}
	cr, ok := d.sup.(CapabilityReporter)
	if !ok {
		return capabilitiesResult{Supported: false}, nil
	}
	return capabilitiesResult{Supported: true, Snapshot: cr.Capabilities(p.Cwd)}, nil
}

// ErrCapabilitiesUnsupported is returned by [Client.Capabilities] when the
// daemon cannot report its capabilities: it predates gofer/capabilities
// (method-not-found, JSON-RPC -32601), or it answered {supported:false}
// because its supervisor is not a [CapabilityReporter] — a `--workers` router.
//
// The two collapse deliberately. Both mean exactly "this daemon has no answer
// for you", the caller's response to either is identical (render UNKNOWN), and
// keeping them apart would only invite a caller to treat one of them as an
// error the user could act on. Distinguish it with errors.Is; the same shape
// [ErrHelloUnsupported] uses.
var ErrCapabilitiesUnsupported = errors.New("daemon does not report capabilities")

// Capabilities reads the daemon's runtime MCP/skills report for a session
// created in cwd. It returns [ErrCapabilitiesUnsupported] (via %w for the
// method-not-found case) when the daemon has no answer, which the caller maps
// to UNKNOWN rather than to an error or — the bug this whole path exists to
// prevent — to a locally recomputed snapshot of a completely different
// process's configuration.
func (c *Client) Capabilities(ctx context.Context, cwd string) (capability.Snapshot, error) {
	raw, err := c.Call(ctx, methodGoferCapabilities, capabilitiesParams{Cwd: cwd})
	if err != nil {
		if IsMethodNotFound(err) {
			return capability.Snapshot{}, fmt.Errorf("%w: %v", ErrCapabilitiesUnsupported, err)
		}
		return capability.Snapshot{}, fmt.Errorf("daemon client: gofer/capabilities: %w", err)
	}
	var res capabilitiesResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return capability.Snapshot{}, fmt.Errorf("daemon client: decode gofer/capabilities result: %w", err)
	}
	if !res.Supported {
		return capability.Snapshot{}, ErrCapabilitiesUnsupported
	}
	return res.Snapshot, nil
}
