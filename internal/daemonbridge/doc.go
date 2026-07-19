// Package daemonbridge adapts a [*daemon.Client] — a JSON-RPC-over-WebSocket
// connection to a running `gofer daemon` — to the TUI's narrow
// [tui.Supervisor] consumer interface, so the same roster/peek/attach TUI
// that renders a local in-process supervisor (see internal/tuibridge) can
// instead render a daemon's live roster: a session created from a phone or
// editor ACP client appears in the laptop TUI too.
//
// # A thin TUI adapter over the reconstruction core
//
// The event-stream reconstruction — draining the client's notification stream
// and rebuilding each session's typed [event.Event] stream from the daemon's
// lossless gofer/event wire — lives in the tui-free
// [github.com/jedwards1230/gofer/internal/wirestream] package, so the same
// decoder can also back the M6 router's worker proxy. This package composes a
// [*wirestream.Reconstructor] and layers the TUI-shaped translation on top:
// mapping the daemon's wire roster rows to [tui.SessionInfo]
// ([statusFromWire]/[toTUISessionInfo]) and exposing the
// create/kill/archive/set-model/interrupt/reply control surface [tui.Model]
// drives. See [Supervisor] and the wirestream package doc for the split.
package daemonbridge
