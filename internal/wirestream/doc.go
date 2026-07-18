// Package wirestream reconstructs a session's typed [event.Event] stream from
// a running `gofer daemon`'s wire — the tui-free reconstruction core shared by
// [github.com/jedwards1230/gofer/internal/daemonbridge] (which adapts it to the
// TUI's roster/attach consumer interface) and, in M6, the router's
// supervisor-shaped worker proxy (internal/router). One decoder, two consumers.
//
// # Why reconstruction is needed (this is not a thin pass-through)
//
// A [*daemon.Client] is a JSON-RPC-over-WebSocket connection to a supervisor
// running in a DIFFERENT process — or a different machine entirely. There is no
// shared memory to hand the supervisor's own [*event.Broker] through, only the
// wire: ACP session/update notifications and gofer's own gofer/* control
// methods. So a [*Reconstructor] REBUILDS each session's typed [event.Event]
// stream from the daemon's lossless gofer/event envelopes into a per-session
// [*event.Broker] that a consumer can [Reconstructor.Subscribe] to exactly as
// if the supervisor were local. See reconstruct.go for the reconstruction
// design and its event-ordering guarantee.
//
// # The two subscribe modes
//
// [Reconstructor.Subscribe] replays each session broker's retained must-deliver
// backlog to a late subscriber (peek/attach re-entering a session already in
// flight still sees its lifecycle events, and an open permission request
// re-surfaces). [Reconstructor.SubscribeLive] is the same subscription WITHOUT
// that backlog replay — a genuine live-only stream, for a consumer (the M6
// router's SubscribeLive) that has already sourced any needed history another
// way and wants only new events from the point of subscription forward.
package wirestream
