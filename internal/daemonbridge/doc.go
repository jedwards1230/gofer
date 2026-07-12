// Package daemonbridge adapts a [*daemon.Client] — a JSON-RPC-over-WebSocket
// connection to a running `gofer daemon` — to the TUI's narrow
// [tui.Supervisor] consumer interface, so the same roster/peek/attach TUI
// that renders a local in-process supervisor (see internal/tuibridge) can
// instead render a daemon's live roster: a session created from a phone or
// editor ACP client appears in the laptop TUI (M2's bar; see
// docs/M2-PROOF.md §4).
//
// # Why this is not a thin pass-through
//
// [internal/tuibridge.Adapter] wraps an in-process [*supervisor.Supervisor]:
// the TUI and the supervisor share memory, so Subscribe is a direct pass
// through to the supervisor's own [*event.Broker]. daemonbridge has no such
// shared memory — the daemon's supervisor runs in a different process (or a
// different machine entirely) and exposes only the wire: ACP session/update
// notifications and gofer's own control methods. So [Supervisor] must
// RECONSTRUCT the typed [event.Event] stream [tui.Model.Ingest] expects from
// that narrower wire projection. See reconstruct.go for the reconstruction
// design and its event-ordering guarantee.
package daemonbridge
