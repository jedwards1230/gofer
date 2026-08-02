package main

// capabilities_wire.go builds [tui.CommandEnv.Capabilities] — the /mcp and
// /skills panels' data source (gofer#303) — for each of the two TUI backends,
// and holds the classification seam between them.
//
// It is a file of its own because of an invariant that is easy to break by
// accident and impossible to notice afterwards:
//
//	A DAEMON-ATTACHED PANEL MUST NEVER RENDER THIS PROCESS'S SNAPSHOT.
//
// MCP connections and skill directories belong to whichever process owns the
// supervisor. This process's config.json and ~/.gofer/skills describe THIS
// machine; a daemon on another host has entirely different ones. A fallback
// from the wire to a local read would therefore not degrade — it would produce
// a complete, confident, plausible answer about the wrong computer, with
// nothing on screen to hint at the substitution.
//
// So the two builders below are strictly separate: the daemon one talks only
// to the wire, the local one only to the in-process supervisor, neither ever
// calls the other, and [buildCommandEnv] — the builder both backends share —
// leaves the field nil. Unavailability is a first-class answer, not a cue to
// go looking somewhere else.

import (
	"context"
	"errors"

	"github.com/jedwards1230/gofer/internal/capability"
	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/tui"
)

// daemonCapabilities returns the [tui.CommandEnv.Capabilities] closure for the
// DAEMON backend: one gofer/capabilities round trip on the CURRENT connection,
// classified through [classifyCapabilities]. cwd is this client's working
// directory, forwarded so the daemon reports the skills a session started here
// would load.
//
// client is a supplier, not a client, and that is load-bearing: it is resolved
// on every call so the panel follows the stale-daemon banner's restart onto the
// replacement connection — see [daemonbridge.Supervisor.Client], which explains
// what a cached one costs. Pass `b.Client`, never `b.Client()`.
//
// It has no local fallback and must never gain one — see the file doc.
func daemonCapabilities(client func() *daemon.Client, cwd string) func(context.Context) (capability.Answer, error) {
	return func(ctx context.Context) (capability.Answer, error) {
		snap, err := client().Capabilities(ctx, cwd)
		return classifyCapabilities(snap, err)
	}
}

// localCapabilities returns the [tui.CommandEnv.Capabilities] closure for the
// LOCAL in-process backend: a direct read of the supervisor this process owns,
// which is the only backend whose snapshot this process may legitimately show.
//
// It cannot fail — [supervisor.Supervisor.Capabilities] collapses every
// failure mode to the empty answer — so the answer is always Known.
func localCapabilities(sup *supervisor.Supervisor, cwd string) func(context.Context) (capability.Answer, error) {
	return func(context.Context) (capability.Answer, error) {
		return capability.Answer{Known: true, Snapshot: sup.Capabilities(cwd)}, nil
	}
}

// attachCommandEnv is `gofer attach`'s [tui.CommandEnv]: the shared local
// builder plus the daemon capability closure bound to the bridge's CURRENT
// connection (client is a supplier — see [daemonCapabilities]).
//
// It exists because the shared builder deliberately leaves Capabilities nil
// (see the file doc), and `gofer attach` is always daemon-backed — so calling
// buildCommandEnv alone left /mcp and /skills permanently UNKNOWN on the one
// entrypoint CLAUDE.md describes as the daemon-attached TUI, against a daemon
// that could answer perfectly. It failed SAFE rather than lying, which is why
// nothing caught it: an unwired closure and an unreachable daemon are
// indistinguishable on screen.
//
// Wrapping it in a named function rather than repeating two lines at the call
// site is the point: there is now one place per backend that binds this, and
// TestAttachWiresDaemonCapabilities pins that attach uses it.
func attachCommandEnv(client func() *daemon.Client, root, cwd string) tui.CommandEnv {
	env := buildCommandEnv(root, cwd)
	env.Capabilities = daemonCapabilities(client, cwd)
	return env
}

// classifyCapabilities turns a gofer/capabilities result into the
// ([capability.Answer], error) pair [tui.CommandEnv.Capabilities] is specified
// in terms of. It is split out from its single caller purely as a TEST SEAM,
// exactly as [classifyHelloDefault] is, and for the same reason: the branch
// that matters is the unsupported one, and reaching it through a real client
// would mean standing up a daemon that deliberately omits the method.
//
// Three inputs, three outcomes:
//
//   - no error — the daemon answered; Known=true, snapshot verbatim.
//   - [daemon.ErrCapabilitiesUnsupported] — a permanent "this daemon has no
//     answer" (it predates the method, or it is a `--workers` router whose
//     supervisor owns no MCP manager). Reported as UNKNOWN with NO error: the
//     user cannot act on it, and the panel already says so in words.
//   - anything else — a transport failure; UNKNOWN plus the error, which the
//     TUI also collapses to unknown. The distinction is drawn here so exactly
//     one place decides, and so a caller that wants to log it still can.
//
// Every branch returns an answer with Known=false rather than a zero Snapshot
// with Known=true. That is the whole point: a zero Snapshot means "no servers,
// no skills", which is a claim, and it is not the one being made.
func classifyCapabilities(snap capability.Snapshot, err error) (capability.Answer, error) {
	switch {
	case err == nil:
		return capability.Answer{Known: true, Snapshot: snap}, nil
	case errors.Is(err, daemon.ErrCapabilitiesUnsupported):
		return capability.Answer{}, nil
	default:
		return capability.Answer{}, err
	}
}
