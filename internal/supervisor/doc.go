// Package supervisor is gofer's registry of live coding-agent sessions: the
// M2 daemon's core. A later TUI and ws/ACP layer both drive sessions
// exclusively through this package's exported API — read this doc and the
// method signatures below and you have the whole contract.
//
// # What the supervisor owns, vs the SDK
//
// The SDK ([github.com/jedwards1230/agent-sdk-go]) owns one session's loop,
// provider, tool registry, and durable JSONL journal — see
// [github.com/jedwards1230/agent-sdk-go/runner], which wraps exactly one
// SDK session. The supervisor owns everything a second application would NOT
// need unchanged (the SDK-promotion test from the repo's docs/PRD.md):
// registering many sessions under one shared [session.FileStore], a
// run-state machine per session (idle/running), a FIFO prompt queue with
// real steering, and the kill/archive lifecycle operations with their
// must-deliver lifecycle events. The supervisor never reaches into runner or
// SDK internals — it drives each session through the [Session] interface,
// the same shape [github.com/jedwards1230/agent-sdk-go/runner.Runner]
// implements.
//
// # One journal per session, one shared store
//
// The supervisor builds (or accepts, via [Config.Store]) a single
// [session.FileStore] and hands it to every [Session] it constructs, so all
// live sessions' journals live under one root. Journals are addressed by
// project slug (derived from a session's cwd) and session id, at
// <root>/sessions/<slug>/<id>.jsonl — the same layout the SDK's FileStore
// uses internally. [Supervisor.List] walks that layout directly (the SDK
// exposes no store-wide enumeration) and overlays live roster state onto
// whatever it finds on disk.
//
// # Lifecycle: idle, running, kill, archive
//
// A session enters the roster live (idle, [StatusNeedsInput]) via
// [Supervisor.Create] or [Supervisor.Resume]. Clients observe the derived
// [SessionStatus] on a [SessionInfo] snapshot; the internal pump run-state
// (idle⇄running) is not exported. [Supervisor.Create] with an empty prompt
// registers an idle session with no first turn; a non-empty prompt is
// enqueued as its first turn. [Supervisor.Resume] is idempotent: resuming an
// already-live id returns its existing snapshot rather than building a second
// runner over the same journal — the SDK's store caches one live journal per
// id, and two runners driving it concurrently would race on appends.
//
// [Supervisor.Kill] interrupts any in-flight turn, drops the session from
// the roster, emits session.killed on its event stream, and closes it.
// [Supervisor.Archive] drops a session that has already finished its work —
// it rejects (returns [ErrRunning]) a session with a turn in flight OR with
// prompts still queued behind it, so a caller must kill a session that is
// still working rather than archive it. Both keep the
// on-disk journal: gofer's hard invariant (docs/CLAUDE.md) is that journals
// are never deleted, only the roster forgets them.
//
// # Subagent sessions
//
// A subagent is not a black box inside a turn: it is a real session with its
// own journal, cost and transcript, plus a link to the session that spawned it.
// [CreateOptions.ParentID] makes one — Create resolves the parent (live roster
// first, then disk), derives Depth = parent+1, and enforces
// [Config.MaxSubagentDepth] ([ErrNoParent] / [ErrDepthExceeded]).
// [CreateOptions.Agent] is forwarded to [runner.Options.Agent] so the child's
// tool-call events carry its agent id.
//
// The link is DURABLE and gofer-native: it is written beside the journal as
// <root>/sessions/<slug>/<id>.meta.json (see the sidecar file), so
// [Supervisor.List] reports it for offline sessions too and [Supervisor.Resume]
// restores a child's attribution. Only a session that actually has a parent or
// an agent writes a sidecar; a plain root session writes none and every session
// predating this feature reads back as a root, unchanged.
//
// [DiskMeta] is that sidecar's exported reader, for the OTHER offline-row
// builder: internal/router keeps its own List over the same store, and under M6
// it is the daemon clients actually talk to — so both must read the link, or an
// offline subagent tree would flatten on the isolated path only.
//
// # Prompt queue and steering
//
// [Supervisor.Send] never rejects a busy session. A prompt sent to an idle
// session dispatches immediately; a prompt sent to a running session queues
// FIFO and dispatches automatically once the in-flight turn (and every
// prompt queued ahead of it) has settled. [Supervisor.Interrupt] cancels the
// current turn without touching the queue — the session returns to idle and
// picks up the next queued prompt, if any. [Supervisor.QueueList] and
// [Supervisor.QueueClear] make that queue inspectable and clearable by any
// client, matching the PRD's "inspectable and clearable" queue requirement.
//
// # Observing the roster
//
// [Supervisor.Roster] returns a point-in-time snapshot of live sessions,
// newest-first. [Supervisor.WatchRoster] returns a channel that delivers a
// fresh snapshot on subscribe and again on every roster change (create,
// kill, archive, idle⇄running transition, per-turn cost update). Delivery is
// coalescing drop-old: a slow watcher never blocks the supervisor or any
// pump — it may skip intermediate snapshots but always converges to the
// latest. [Supervisor.List] additionally enumerates archived/offline
// sessions still on disk, overlaying live state.
//
// # Concurrency
//
// Every session in the roster runs its own goroutine (its "pump") that
// dispatches queued prompts one at a time. The supervisor's own lock is
// never held across a call into a session (Prompt, Close, or waiting for a
// pump to exit) — only around roster bookkeeping — so one session blocked
// mid-turn never stalls an operation on another. [Supervisor.Close] kills
// every live session (emitting session.killed for each, per the must-deliver
// contract), joins every WatchRoster goroutine, and closes the store it
// owns, then returns.
package supervisor
