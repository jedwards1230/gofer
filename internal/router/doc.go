// Package router is the production router-side remote supervisor of M6 process
// isolation (docs/milestones/M6-process-isolation.md, Phases 2-3). It implements
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
// # What ships so far: crash isolation, detachment, adoption, version policy
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
// # Spawn admission and discovery
//
// Because every session is a whole OS process (~10–20 MB RSS baseline plus its
// loop's working set — design §10), [Supervisor.Create] runs an admission gate
// BEFORE it forks anything: [Config.MaxWorkers] optionally caps how many workers
// this router hosts, and a Create over that cap is refused with the typed
// [ErrAtCapacity] — which the daemon's session/new handler surfaces to the
// client as an ordinary JSON-RPC application error — having forked nothing,
// dialed nothing, and written nothing to disk. The cap counts live handles
// (spawned and adopted) plus in-flight spawns, so concurrent session/new calls
// cannot overshoot it; ending a session (Kill/Archive/crash) frees the slot.
// [DefaultMaxWorkers] — the zero value — is unlimited, so an unconfigured router
// admits every Create exactly as before.
//
// Past admission, the router discovers the new worker by reading its endpoint
// file on a tight-then-backoff cadence (see waitForWorkerEndpoint), which trades
// a few extra stat() calls for most of the fork/exec discovery latency M6 §10
// calls out as a per-session cost.
//
// # Startup adoption: re-attaching to a prior router's live workers
//
// [New] runs [Supervisor.adoptExistingWorkers] (see adopt.go) before the router
// serves anything: it scans the per-worker endpoint files a prior router's
// detached workers left ([daemon.ListWorkerEndpoints]) and, for each, runs the
// §4 adopt/stale decision — pid-liveness probe, dial, and the authoritative
// gofer/hello handshake (whose versions are classified, not gated — see the
// version-skew section below). A still-alive, responsive worker is ADOPTED
// whatever its versions: the router opens a fresh [*daemon.Client] to its
// socket, wraps it in a Reconstructor, re-attaches via
// [wirestream.Reconstructor.Load] (which settles history and re-surfaces any
// still-open PermissionRequested into the broker, §7), and registers an ADOPTED
// handle (cmd==nil; its reaper watches [daemon.Client.Done] rather than a
// *exec.Cmd; pid comes from the endpoint file — except when that file names the
// ROUTER'S OWN pid, which is recorded as 0 so a later Kill/Archive cannot SIGKILL
// the daemon itself; see killHandleProcess). A worker that predates gofer/hello
// (method-not-found ⇒ [daemon.ErrHelloUnsupported]) is adopted too, as an
// unidentified worker on the strict side of the policy below. This is the whole point of M6
// process isolation: a router restart re-attaches to its in-flight sessions —
// including a turn blocked mid-approval — instead of orphaning them. Adoption is
// best-effort: a scan or per-worker failure never fails construction (an empty
// roster is still a usable router). Stale residue from crashed workers (dead pid,
// or a dialed-refused socket) is garbage-collected in the same scan.
//
// # Version skew: what the router does with an old worker (§6, Phase 3)
//
// The router records every worker's versions from the AUTHORITATIVE gofer/hello
// handshake — on both paths a handle can come into existence, Create and adopt —
// and classifies them against its own into a skewClass (see skew.go). The
// endpoint file's version fields remain the cheap pre-dial hint, never decisive:
// it can be stale, and no class refuses adoption anyway. It is logged only when
// the hint's class DISAGREES with the handshake's — that disagreement is the
// stale-endpoint-file condition worth an operator's attention; agreement is the
// normal case and stays silent.
//
// The class governs only what the router subsequently ASKS of that worker:
//
//   - WIRE skew (the router↔worker wire-contract version differs) — adopt for
//     the OBSERVE / PERMISSION-REPLY / FINISH subset only. [Supervisor.Send] and
//     [Supervisor.SetModel] (a model change is new work) are refused with
//     [ErrWorkerSkewed]; everything else — event reconstruction, roster,
//     permission replies, interrupt, kill, archive — still routes, so the
//     in-flight turn finishes normally. This is design §6's literal
//     compatibility window: a wire mismatch means the protocol itself cannot be
//     trusted, and only the additive event subset is guaranteed across the gap.
//   - BINARY skew (the binary version differs, wire matches) — adopt FULLY.
//     Recorded and surfaced, never refused. The wire is compatible, so prompting
//     an older worker just runs another turn on that binary; that is not a
//     hazard, it is SESSION PINNING — the isolation property M6 exists to sell.
//     A session stays on the binary it started on until it ends; new sessions
//     get the new binary. A later refuse-and-migrate policy will flip this to
//     move an old-binary session onto a fresh worker on its next prompt;
//     [Supervisor.Resume] already spawns a fresh worker for an OFFLINE session,
//     which is the mechanism that migration will build on.
//   - UNKNOWN (exactly one side identified its binary — e.g. a worker predating
//     the version-reporting wiring) — treated as BINARY skew: surfaced, not
//     refused. Refusing would brick every worker built before this slice.
//
// Binary comparison is EXACT, not N-1 (design §6: "N-0 full + skew-observe-only
// (Phase 3), widen if ever needed"), and a "-dirty" build is deliberately
// different from its base commit — it genuinely is a different binary.
//
// The session's owning binary is surfaced to clients as
// [supervisor.SessionInfo.BinaryVersion], stamped by the router from the handle
// (a worker's own roster does not know it is being proxied) and carried through
// the existing roster/ps wire as an additive, omitempty "binaryVersion". It is
// LIVE-ONLY: the journal does not record it, so an offline row leaves it empty.
// As of slice 3b it is also RENDERED — a BINARY column in `gofer ps` and a
// per-row suffix in the TUI roster — so §11's "session/list shows mixed
// binaryVersions" criterion is observable by an operator, not just by decoding
// the raw wire.
//
// # The standing per-session watcher
//
// Every live handle — SPAWNED by [Supervisor.Create] or ADOPTED by the startup
// scan — gets a [Supervisor.watchSession] goroutine started right after it is
// registered. It subscribes to that session's reconstructed broker WITH replay
// and drives the two pieces of router state that are projections of a session's
// event stream: the pushed roster cache (below) and the permission relay (§7,
// below). It exits when the broker closes or the router shuts down, and is joined
// by Close.
//
// # Pushed roster cache (§8)
//
// [Supervisor.Roster] — and so [Supervisor.List], every `gofer ps` and every TUI
// roster tick — serves from a per-handle CACHED [supervisor.SessionInfo]: seeded
// once per handle by a single gofer/roster call and thereafter maintained from
// the event stream the watcher is already reading. Steady state costs ZERO worker
// RPCs, where the pre-3b path cost one RPC PER LIVE WORKER per read.
//
// It is an AVAILABILITY fix first. Those per-worker RPCs ran SERIALLY, each
// bounded by wireCallTimeout (15s), so ONE wedged worker stalled every roster
// read fleet-wide for up to fifteen seconds — including the TUI's ~1Hz poll,
// which runs ungated over a non-cancellable context. A cache read is an atomic
// load and cannot be held hostage by any worker. Being cheaper is a secondary
// consequence, not the motivation.
//
// Concurrency: the handle's own watchSession goroutine is the SOLE writer, and it
// publishes whole IMMUTABLE snapshots through an [atomic.Pointer]; every reader
// does a lock-free Load, so no reader observes a half-updated row and no roster
// read can block on the writer. There is deliberately NO TTL — the row's lifetime
// IS the handle's, so reap and take are its only evictors — and no staleness
// field, since [supervisor.SessionInfo.Updated] already carries the snapshot's
// own time. A handle with no cached row (a failed or not-yet-landed seed) falls
// back to a live RPC for that handle alone. Full rationale: rostercache.go.
//
// # Fleet cost/usage aggregation
//
// With each session's usage living in its own worker, the fleet-wide total is no
// longer any single row. [Supervisor.FleetUsage] recovers it by summing the
// pushed roster cache — every live handle's already-correct running Cost/Usage —
// with the SAME lock-free [atomic.Pointer] loads Roster uses and zero worker
// RPCs. It is a plain sum of correct rows, so it does not reintroduce the
// seed-vs-first-delta double-count seam rostercache.go documents (that seam is
// bounded to one turn of one row and is the cache's concern, not the sum's).
//
// It is surfaced, not hidden: the daemon exposes it over gofer/fleet (via the
// optional [daemon.FleetUsager] capability the router implements and the
// in-process supervisor does not), and `gofer ps` prints a fleet-total footer
// from it. An in-process or older daemon reports no total and the footer is
// omitted.
//
// # Event bridge (§5)
//
// A turn running on a worker has no daemon prompt handler in this process fanning
// its events out, so a client attached to such a session would watch a silent
// stream. Each handle's reconstruction core is therefore built with a
// [wirestream.EventSink] ([Supervisor.eventSink]) that hands the daemon's
// [daemon.EventRelay] — injected via [Supervisor.SetEventRelay] — the VERBATIM
// frame bytes for its gofer clients plus the already-decoded event for the ACP
// session/update projection. Two fan-outs, one goroutine, wire order, no
// re-encode and no second decode. The daemon suppresses the relay while one of
// its own prompt handlers is driving that session, so a client-driven turn is
// never delivered twice.
//
// What the bridge does NOT reach: a worker has no continuous broker drain
// outside its own session/prompt handler (see internal/daemon's
// advertiseModelChange). So the tail of a turn whose DRIVING CONNECTION was
// severed mid-flight — the pre-upgrade turn in a daemon hot-upgrade, whose
// client went away with the old daemon — is published to the worker's broker
// and never put on the wire at all. The router cannot forward a frame that was
// never sent, so that tail is NOT STREAMED LIVE. It is not lost, though: the
// worker journals it, and it comes back as folded history on the session's next
// session/load, so a client that re-attaches sees the complete transcript.
// Streaming it live instead would need a standing observer on the WORKER side
// (in internal/daemon, which the worker runs), gated behind a Config flag
// beside ReplayPendingPermissionsOnAttach and relying on exactly the
// promptHandlerActive guard this slice shipped to avoid double delivery. That is
// deferred, not oversighted.
//
// # Event-decode skew: which direction is supported (§5, §6)
//
// A gofer/event frame whose kind this router cannot decode is DROPPED — not
// published to the reconstructed broker and not handed to the sink, so it is
// neither projected nor forwarded. That makes ROUTER-NEWER-THAN-WORKER the only
// supported skew direction for event decode, and it is a deliberate choice
// rather than an accident of the decoder.
//
// It is chosen because it matches how skew actually arises here. The upgrade
// story M6 sells (§11) upgrades the DAEMON first and leaves old workers draining
// underneath it, so the router is by construction the newer half; a worker
// emitting a kind the router has never heard of would mean a worker built after
// the router that adopted it, which this milestone's rollout does not produce.
// The alternative — forward the raw frame and skip the projection — is not
// free: the router's own broker would then hold a stream that DISAGREES with
// what its clients received, so the roster cache, the permission relay and every
// projection driven off that broker would be reasoning about a different session
// history than the client is displaying. A frame the router cannot understand is
// better dropped consistently on both fan-outs than delivered to one of them.
//
// What this costs is bounded and additive-only: an OLDER router silently omits a
// NEWER worker's new event kinds from both surfaces. Widen the decoder (in
// wirestream) before shipping a rollout that can invert the version order.
//
// What this removes is specifically the ROUTER's SECOND-HOP re-encode — the cost
// M6 §10 flags when it says the second hop doubles the per-event encode cost.
// The daemon→client hop was ALREADY marshal-once per event: internal/daemon's
// broadcastGoferEvent marshals once and reuses those bytes for every peer, and
// the only remaining per-peer cost is peer.writeJSON's JSON-RPC envelope, which
// copies the payload rather than re-encoding the typed event. So this is not
// "making fan-out marshal-once" and the win does not scale with attached peers:
// the router simply no longer decodes and re-encodes a frame it is forwarding
// unchanged. That work is not made faster — it is no longer done.
//
// # Permissions across a router restart (§7)
//
// An adopted session's turn runs on its worker, so — unlike a session a client
// drives via session/prompt — no daemon prompt handler is watching its event
// stream to record permission routes and fan requests out. The router bridges
// that gap: after the daemon is constructed it injects a [daemon.PermissionRelay]
// via [Supervisor.SetPermissionRelay], which the standing watchers drive. A
// watcher's replaying subscription delivers a request re-surfaced by Load, so the
// daemon records the call→session route (making handlePermissionReply resolve for
// adopted sessions) and broadcasts the request to attached clients. A client
// of the restarted router then attaches via session/load ([Supervisor.Resume]
// returns the live snapshot for a session this router already hosts), sees the
// re-surfaced request, and answers it — the reply routes through the daemon to
// [Supervisor.Reply] and the worker's gate. So a turn blocked mid-approval
// survives a router restart end to end.
//
// # Resume: live attach vs offline spawn (§8)
//
// [Supervisor.Resume] serves session/load two ways. For a session this router
// already hosts LIVE (adopted or created) it returns the live snapshot — the §7
// attach path above. For an OFFLINE id — a journal on disk with no live worker
// (it crashed, was killed, or a restart could not adopt it) — it SPAWNS a fresh
// worker (the same [Supervisor.spawnWorker] bring-up Create uses) and rebuilds it
// from the journal by issuing session/load on the worker. A genuinely unknown id
// (no live handle, no journal) returns a [session.ErrSessionNotFound]-wrapped
// error rather than spawning over nothing.
//
// The offline replay must NOT reach clients: the worker replays its journal as
// gofer/event frames, and re-broadcasting them through the event sink would
// double the transcript for an already-attached peer (the daemon's own
// handleSessionLoad replays the History to the resuming client once). Resume
// SUPPRESSES the sink for the duration of the replay via a per-resume guard the
// sink consults, cleared only after [wirestream.Reconstructor.Load] reports the
// replay fully settled — see [Supervisor.resumeOffline] for the guard's
// lost-event-free ordering argument, and [Supervisor.SetEventRelay] for how the
// two Loads (adoption's and resume's) each honor the no-double-broadcast
// invariant.
//
// # Graceful drain (§11 hot-upgrade)
//
// [Supervisor.Drain] is the controlled first step of the daemon hot-upgrade
// story: it flips the router into a draining state (idempotent) so [Supervisor.admit]
// refuses a new [Supervisor.Create] with [ErrDraining], then blocks — bounded by
// its ctx — until every live handle reports idle, reusing the SAME settle signal
// [Supervisor.AwaitSettled] uses (each handle's settleCh, poked by its watcher on
// the needs-input transition). Draining refuses only BRAND-NEW sessions;
// resuming an already-live session to finish its work stays allowed (Resume does
// not run through admit).
//
// Drain does NOT kill workers — it is the step BEFORE [Supervisor.Close], which
// itself deliberately leaves detached workers running to be re-adopted (§3). The
// sequence a graceful daemon shutdown runs is: stop serving, Drain (let in-flight
// turns settle while still attached and relaying their events), then Close
// (detach). On a ctx timeout Drain returns ctx.Err() and the caller proceeds to
// Close anyway: a still-working worker finishes its turn detached and is
// re-adopted on the next start regardless. The drain window is bounded by an
// operator-tunable timeout (config.Daemon.DrainTimeoutMS), wired in cmd/gofer's
// serveDaemonForeground.
//
// # Deliberate cuts for this slice (documented, not oversights)
//
//   - Refusing a PROMPT on BINARY skew — which design §6's prose describes as
//     "the next prompt waits for a fresh worker" — is deliberately NOT
//     implemented, and the wire-mismatch case above is the only refusal.
//     Refuse-and-migrate needs to MOVE a live old-binary session onto a fresh
//     worker on its next prompt; [Supervisor.Resume] can now spawn a worker for an
//     OFFLINE session, but the drain-and-migrate of a still-LIVE one (interrupt
//     the old worker, hand the turn to a new one) is not wired, so refusing an
//     old-binary session's prompts would still strand it. The migration policy is
//     the remaining follow-up; the resume-spawns-a-worker mechanism it will build
//     on is in place.
//   - Asking a pure-ACP peer (a phone, via session/request_permission) to answer
//     a RE-SURFACED permission on an adopted session is Phase 3: the standing
//     watcher fans the gofer-native notification (serving the TUI/daemonbridge)
//     and records the route so ANY client's routed reply resolves, but does not
//     itself issue the spec-ACP request.
//   - STRUCTURED DECISIONS (the ask_user tool — see internal/decision) do not
//     cross this hop. The relay landed for a single daemon hosting the in-process
//     supervisor: that supervisor runs a standing per-session watcher over each
//     session's decision gate and drives the daemon's fan-out from it (see
//     internal/daemon's DecisionRelay). A worker's own daemon installs no such
//     relay, and this Supervisor implements no [daemon.DecisionAnswerer], so under
//     --workers an ask_user reaches no client and an answer routed here is refused
//     with a clear error rather than silently swallowed. Closing it needs the same
//     treatment permissions got — relaying the worker's decision updates up and a
//     decision.answer Notify back down — plus decision methods in the skew subset
//     (skew.go), since an old-binary worker will not speak them.
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
