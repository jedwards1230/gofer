// Package router is the production router-side remote supervisor of M6 process
// isolation (docs/milestones/M6-process-isolation.md, Phase 2). It implements
// [github.com/jedwards1230/gofer/internal/daemon.Supervisor] — the exact set of
// methods the daemon's ACP-over-WebSocket surface drives — by running EACH
// session in its own DETACHED `gofer session-worker` process and proxying the
// daemon's calls to it over the very same public wire (ACP v1 + gofer/*) the
// daemon already speaks to clients. The daemon, wired to this supervisor via the
// `--workers` flag, becomes a thin router: roster aggregation, client fan-out,
// and the ACP surface, with every version-coupled turn machine (the SDK runner,
// the pump, the approval gate, the journal writer, the event broker) living one
// tier down in a worker.
//
// # What ships in this slice: crash isolation + detachment + adoption
//
// A worker is a single-session daemon (see internal/worker). The router spawns
// one per [Supervisor.Create] via [daemon.SpawnDetached] (Setsid, stdio to a
// per-worker log file) with the session uuid pre-generated here and pinned by
// the worker (design Option A), dials it as an ORDINARY [*daemon.Client] over its
// unix socket, and wraps that connection in a [*wirestream.Reconstructor] — the
// same tui-free reconstruction core internal/daemonbridge uses — so the router
// reconstructs each session's typed [event.Event] stream from the worker's wire
// without ever linking the SDK runner/loop. Because a worker is its own OS
// process, a panic, OOM, or `kill -9` takes down exactly that one session: the
// router observes the worker's connection drop (which closes the reconstructed
// broker, terminating every attached subscriber's stream) and its process exit
// (delivered by [daemon.Reap]), drops the dead session's live handle, and leaves
// every sibling worker untouched. The killed session then reappears as an offline
// journal via [Supervisor.List]'s live∪disk union.
//
// Detachment means a worker outlives the router: on shutdown [Supervisor.Close]
// STOPS signalling workers (it only stops the reapers), so detached workers
// reparent to pid 1 and keep pumping their turns.
//
// # Startup adoption: re-attaching to a prior router's live workers
//
// [New] runs [Supervisor.adoptExistingWorkers] (see adopt.go) before the router
// serves anything: it scans the per-worker endpoint files a prior router's
// detached workers left ([daemon.ListWorkerEndpoints]) and, for each, runs the
// §4 adopt/stale decision — pid-liveness probe, wire-version check (endpoint
// hint then authoritative gofer/hello), and dial. A still-alive, version-matched,
// responsive worker is ADOPTED: the router opens a fresh [*daemon.Client] to its
// socket, wraps it in a Reconstructor, re-attaches via
// [wirestream.Reconstructor.Load] (which settles history and re-surfaces any
// still-open PermissionRequested into the broker, §7), and registers an ADOPTED
// handle (cmd==nil; its reaper watches [daemon.Client.Done] rather than a
// *exec.Cmd; pid comes from the endpoint file). This is the whole point of M6
// process isolation: a router restart re-attaches to its in-flight sessions —
// including a turn blocked mid-approval — instead of orphaning them. Adoption is
// best-effort: a scan or per-worker failure never fails construction (an empty
// roster is still a usable router). Stale residue from crashed workers (dead pid,
// or a dialed-refused socket) is garbage-collected in the same scan.
//
// # Deliberate cuts for this slice (documented, not oversights)
//
//   - Wire-version SKEW routing — adopting an old-binary worker for the
//     observe/reply/finish subset only (design §6) — is Phase 3. This slice
//     leaves a version-skewed worker running detached but unadopted (it is not
//     GC'd — it holds a live session — merely not routed to).
//   - [Supervisor.Resume] returns [ErrResumeUnsupported]; spawning a fresh worker
//     for an offline/old-binary session is Phase 4. A consequence for THIS slice:
//     ACP session/load (which the daemon routes through Resume) fails cleanly for
//     every session, so attach-via-load is unavailable — the working path is
//     session/new + session/prompt (Create + Send), which is all crash isolation
//     needs.
//   - [Supervisor.EmitConfigOptions] returns [ErrEmitConfigUnsupported]: there is
//     no wire method for a client to make a daemon emit config options, and it is
//     off the crash-isolation critical path. The live config_option_update a model
//     swap produces still reaches clients — the WORKER's own daemon emits it and
//     the router reconstructs it (see [Supervisor.SetModel]).
//
// # Invariants honored
//
// Everything-is-a-client: the router↔worker hop is an ordinary ACP+gofer client
// relationship — the worker gets no privileged path. Contract-only SDK
// consumption: the router links the SDK solely for event decode (via wirestream)
// and read-only journal-metadata/fold reads (via the session package), NEVER the
// runner or loop. Journals are never deleted: Kill/Archive terminate the worker
// and drop the handle but leave the JSONL journal, which is the offline/adoption
// source of truth.
package router
