# Per-Session Process Isolation — Design (M6)

> Proposed gofer milestone. Detached per-session worker processes behind a thin router daemon, so the daemon and CLI can be upgraded in place while live turns finish uninterrupted on the binary that started them.

## 1. Goal & the one-line thesis

The daemon and CLI are already separate processes. This milestone makes **each session its own detached OS process** (`gofer session-worker`). A session's whole turn machinery — the SDK runner, the prompt pump, the approval gate, the journal writer, the event broker — runs inside its worker. The daemon shrinks to a **router**: roster aggregation, client fan-out, worker discovery/adoption, and the ACP surface. Because nothing version-coupled to the SDK runs in the router, **the router (and the CLI) can be upgraded or restarted at will; in-flight turns keep running on their worker's old binary and simply finish; the next `session/new` spawns a worker on the new binary.** Mixed binary versions coexist by construction.

The design is not exotic: gofer **already** ships a lossless, process-crossing, per-session event stream (`internal/daemonbridge` reconstructs a session's typed `event.Event` broker from a remote daemon's wire). That machinery is the template; this milestone applies it one tier deeper.

## 2. Where the boundary goes (the key decision)

The supervisor already isolates the SDK behind one seam — the `Session` interface (`internal/supervisor/types.go:59`), the subset of `runner.Runner` the supervisor drives, with injectable `NewSession`/`ResumeSession` factories (`supervisor.go:50-54`). Everything above that interface (the `managed` pump, the prompt queue, the per-session `*loop.Gate`, `watchPermissions`, the roster) is thin, version-stable gofer code; everything the interface reaches (`runner.New`/`Resume`, `Prompt`, `SetModel`, `Emit`, `Cost`, the broker, the journal) is SDK-version-coupled.

The naive boundary — put a worker *below* the `Session` interface, keep the pump in the daemon — **does not achieve the goal.** The pump's `m.sess.Prompt(turnCtx, text)` call (`managed.go:254`) drives a turn; if it lives in the daemon and the daemon restarts, that call is severed and the turn is abandoned mid-flight. Likewise the `*loop.Gate`: if an approval is outstanding when the daemon dies and the gate lives in the daemon, the await is lost.

So the boundary goes **above** `managed`: the **worker owns the pump, the gate, `watchPermissions`, the runner, the broker, and the journal** — i.e. essentially today's `supervisor.managed` + the SDK runner. The elegant realization: **a worker is a single-session daemon.** It runs `supervisor.New` capped at one session and serves the *existing daemon wire* (ACP v1 + `gofer/*` native) over a unix socket. The router's "supervisor" then becomes a **`daemonbridge`-shaped remote proxy** — the exact class that already reconstructs a session's event stream from that wire.

```
        client wire (unchanged ACP + gofer/*)         router↔worker wire (the SAME wire + gofer/hello)
 CLI / phone / editor ──────────────►  gofer daemon (router) ──────────────►  gofer session-worker (one per session)
   ACP client              WebSocket       · roster (union: workers + disk)      unix socket  · supervisor(1 session)
                                           · peer fan-out (sessionPeers)                       · runner.Runner + broker + journal
                                           · discovery / adoption / spawn                      · pump + prompt queue
                                           · permission ROUTING (no gate)                      · *loop.Gate (the gate lives HERE)
                                           · ACP surface                                       · version-coupled: SDK loop runs HERE
```

Net effect: the two hops speak one protocol (the public client wire), and each tier reuses code that already exists — the worker reuses `daemon.Serve` + `supervisor`, the router reuses `daemonbridge` reconstruction + the `sessionPeers` fan-out.

## 3. Process model

**Spawn & detachment.** On `session/new`, the router forks/execs `gofer session-worker --session <uuid> --root <root> --model … --listen unix://<runtime>/workers/<uuid>.sock`. The worker is detached so it outlives the router: `SysProcAttr{Setsid: true}` (new session + process group, no controlling terminal), stdio redirected to a per-worker log file. Unix-only, matching the repo's posture (no Windows build — see `pidAlive`'s comment).

- **Linux**: `setsid` reparents the worker to `init`/the nearest subreaper when the router exits.
- **macOS**: the router is already a launchd *user agent* (`service_darwin.go`). Workers are **not** launchd jobs — plain detached processes that reparent to `launchd` (pid 1) on router exit; launchd reaps their eventual exit but never tracks or kills them. When launchd restarts the router (KeepAlive), the new router re-adopts the orphaned workers by socket scan (§4). Workers are login-session-scoped, same as the daemon today.

**Orphan reaping.** While the router lives it is the worker's parent, so it runs a per-worker `cmd.Wait()` reaper goroutine to avoid zombies. On router shutdown it **does not signal workers** — it just stops waiting; orphaned workers reparent to pid 1, which reaps them. (No double-fork needed; the router learns each worker's pid from its endpoint file.)

**Crash isolation.**
- *Worker dies ≠ fleet dies.* A panic/OOM kills one session's process only. The router observes the socket close, marks that session **offline** (a resumable journal), fans a terminal `session.error` to attached peers, leaves every other worker untouched.
- *Router dies ≠ workers die.* Detached workers keep pumping turns, keep their gates, keep journaling. A new router adopts them; in-flight turns finish uninterrupted. **This is the milestone's whole point.**
- *Journals are the durable truth.* A session whose worker is gone with no live turn is just a JSONL journal — resume spawns a fresh worker.

## 4. Reattach / adoption protocol

**Per-worker endpoint convention.** Mirror the daemon's own endpoint-file precedent (`cmd/gofer/daemon.go`, `internal/daemon/endpoint.go`). Each worker writes, atomically at startup, `<runtime>/gofer-<uid>/workers/<uuid>.json` (mode 0600) advertising `{addr: unix://…/<uuid>.sock, pid, binaryVersion, wireVersion, startedAt}` and removes it on clean exit. (`acpProtocolVersion` is **not** listed here: it is advertised/confirmed via the `gofer/hello` handshake in §6, not negotiated at ACP `initialize` — which today does not run at all.) `<runtime>` follows the same scheme the daemon socket already uses (`$XDG_RUNTIME_DIR/gofer-<uid>…`, with the daemon's existing macOS fallback).

**Discovery on router start.** Scan `workers/`; for each endpoint file:
1. `pidAlive(pid)` (reuse `cmd/gofer/daemon.go:pidAlive` — signal-0 probe) **and**
2. dial + `gofer/hello` handshake (reuse `daemon.Probe` shape).

Both pass → **adopt**: open a `daemon.Client` to the worker, wrap it in the router's `daemonbridge`-shaped remote supervisor, pull the worker's `SessionInfo` into the roster, re-subscribe to its event stream (must-deliver replay §6 re-surfaces any open permission request). Either fails → **stale**: unlink the endpoint file (best-effort), treat the session as offline/resumable.

**Failure matrix.**

| Situation | Detection | Action |
|---|---|---|
| Stale socket (file present, no listener) | dial refused | unlink file; session offline |
| Dead worker, live socket file (pid gone) | `pidAlive` false | unlink file; session offline |
| Live pid, unresponsive worker | handshake timeout | leave file; mark degraded, do **not** route prompts; kill path reaps |
| Two routers racing | router endpoint guard (`guardLiveEndpoint`, already exists) | second router fails fast ("already running") — one router owns the roster |
| Duplicate worker for one session id | per-session lockfile (below) | second worker fails to acquire → exits |

**Single-writer guard.** The SDK journal is **not** protected by a cross-process advisory flock — the only flock in the SDK is `auth.lock` on the token store; journal writes are guarded by an *in-process* semaphore only. So gofer must enforce one-worker-per-session itself: (a) the router refuses to spawn a second worker for a session id it already has a live worker for, and (b) the worker acquires a gofer-side advisory lock on `<uuid>.lock` (`LOCK_EX|LOCK_NB`) before opening the runner and hard-fails if held — the authoritative guard against a racing adopt+spawn.

## 5. Event streaming across the boundary

Already exists at the client↔daemon tier; applied verbatim at router↔worker.

- The worker owns a real `event.Broker` (two-tier delivery: lossy deltas dropped under backpressure, `TierMustDeliver` lifecycle/terminal events blocked-with-bound; `WithReplay(n)` retains the last *n* must-deliver events for late subscribers). Its journal is the append-only JSONL truth. **Backlog and replay live in the worker**, next to the runner that produces them — never proxied.
- The worker emits every event on the wire as `gofer/event`: **the source event's own `MarshalJSON` envelope, verbatim** (`internal/daemon/handlers.go:broadcastGoferEvent`). This is lossless — `tool.call.delta` input fragments and `tool.call.finished` `Diagnostics`/`Spill*` fields, both absent from ACP's `session/update`, survive.
- The router reconstructs each session's typed stream by decode-dispatch-publish into a per-session broker via the SDK's `event.New*` constructors — **exactly `handleGoferEvent`**, which M6 extracted from `daemonbridge` into `internal/wirestream/reconstruct.go`. Attach/peek fidelity, the history-before-live ordering guarantee, and the `TurnFinished`-vs-last-event ordering all carry over because the reconstruction is already written and tested — each property by a different test, so cite them separately: **content** fidelity (delta fragments and `Diagnostics`/`Spill*` surviving the wire) by `daemonbridge/fidelity_test.go`'s `TestFidelityToolCallDeltaAndSpill`, and **history-before-live ordering** by `daemonbridge/bridge_test.go`'s `TestAttachReplaysHistory` and `wirestream/subscribe_firstref_test.go`'s `TestSubscribeLiveFirstReferenceReplaysHistory`. `fidelity_test.go` proves nothing about ordering.
- The router re-fans reconstructed events to attached clients through the **existing** `sessionPeers` registry (`broadcastGoferEvent`/`broadcastUpdate`). Clients see no change: the same `session/update` + `gofer/event` they see today.

The one genuinely new cost is the **second hop** (worker→router→client vs daemon→client): local unix-socket RTT + a JSON re-encode per event — µs-scale, already paid once at the client hop, now paid twice. Below human-perceptible latency for interactive use; noted honestly as the per-event tax.

## 6. The versioned router↔worker wire contract

**It is the daemon's existing public wire** — ACP v1 JSON-RPC plus the `gofer/*` native methods. This is deliberate: **not a net-new compatibility surface**, but the surface gofer already maintains for clients, now also spoken internally. The event envelope *is* the journal serialization (both are `event.Event.MarshalJSON`), so wire and journal share one compat surface, not two.

**Negotiation.** There is no in-protocol version handshake to inherit: `daemon.Client.Dial` performs no ACP `initialize`, and ACP's `InitializeResponse` carries no version field. So version exchange must be **designed into the router↔worker contract from day one — it cannot be assumed from ACP.** Two mechanisms, both gofer-native, layered:

1. **Endpoint-file advertisement (discovery-time).** The worker's `<uuid>.json` advertises `{binaryVersion, wireVersion}` — mirroring the daemon/CLI version-warning approach of carrying the daemon's version via `daemon.json`. The router reads this during the adoption *scan* (§4), before it even dials, to make an adopt/spawn/skew-route decision from the file alone.
2. **In-protocol handshake (authoritative).** A gofer-native `gofer/hello` the router calls first on every worker connection, returning `{binaryVersion, wireVersion, acpProtocolVersion}`. Required, not optional: router and worker **must** know each other's versions to route around skew (the whole point), and the endpoint file alone is insufficient — it can be stale and doesn't cover a re-dial after the file is gone. The handshake is the source of truth; the endpoint file is the cheap pre-dial hint.

**Why both, and the forward payoff:** the file keeps adoption cheap and reuses the existing mechanism; `gofer/hello` makes version knowledge authoritative and connection-scoped. Crucially, this gofer-native handshake is the **template that later subsumes the endpoint-file limitation for remote/explicitly-addressed daemons** — a client or fleet router that can only reach a daemon by address, with no local endpoint file, can ask it its version in-band via the same `gofer/hello`.

**Compatibility window — the honest simplification.** The router does not need to fully support N versions of the worker wire. Two facts shrink the obligation:
1. Workers are **short-lived** — one session each. The max skew a router must tolerate is bounded by the *longest running session*, not by release cadence.
2. A skewed in-flight session only needs to **finish**, not accept new work. So the router must forward-support only the **observe + permission-reply + terminal-event subset** across a version gap — which the additive event wire already is — and may **refuse to route a *new prompt* to a version-skewed worker**, letting the current turn finish and the next prompt wait for a fresh worker on the router's own binary.

Recommended policy: router supports the current wire fully and **N-1 for the observe/reply/finish subset only**. Start at N-0 full + skew-observe-only (Phase 3), widen if ever needed. Governance rides the **existing** promote-if-stable policy + additive-field discipline — `daemonbridge` already tolerates unknown methods (silently dropped) and additive fields.

## 7. Permissions across processes

The gate stays with the turn — **in the worker.** The round trip:

1. Worker's `RuleGuard` hits an ask → holds the worker's `*loop.Gate`, blocks the turn, emits `event.PermissionRequested` on its stream.
2. Router observes it (it's subscribed), records the call-id→session route (`recordPermRoute`), fans it out to peers **exactly as today**: `gofer/permission_requested` to gofer clients **and** the spec-ACP `session/request_permission` request to ACP peers. First answer from any surface wins.
3. A peer replies → router `handlePermissionReply` looks up the route and calls its supervisor's `Reply(sessionID, …)`. In the router that supervisor is the remote proxy, so `Reply` forwards `permission.reply` over the **owning worker's** IPC (precisely what `daemonbridge.Supervisor.Reply` already does). The worker's local supervisor resolves its gate; the turn proceeds.

**Survives a router restart mid-approval:** the worker keeps holding its gate and blocking the turn; the new router adopts the worker, re-subscribes, and the still-open `PermissionRequested` re-surfaces via must-deliver **replay**; it re-fans to peers; the reply routes through. Nothing is lost because the gate never left the worker and the backlog never left the worker's broker.

> **Adoption-time pure-ACP exception (through Phase 2):** the standing per-adopted-session watcher re-fans a re-surfaced permission only as the gofer-native `gofer/permission_requested` notification (and records its call-id→session route, so *any* client's routed `permission.reply` resolves it). It does **not** re-issue the spec-ACP `session/request_permission` request, so a pure-ACP peer (a phone) cannot *answer* a re-surfaced permission on an adopted session until Phase 3 wires the ACP ask into the adoption path. gofer clients (TUI/daemonbridge) are unaffected.

## 8. Lifecycle deltas

| Op | Today (in-process) | With workers |
|---|---|---|
| `session/new` | `supervisor.Create` → `runner.New` goroutine | router spawns+detaches a worker, adopts it, proxies the first prompt |
| `session/prompt` | pump `Prompt` in-process | router forwards `session/prompt` to the worker; the worker's pump drives it |
| `kill` | cancel `baseCtx`, reap goroutine, emit `session.killed` | router sends `gofer/kill` → worker stops its pump, emits `session.killed`, exits; router reaps pid, unlinks endpoint |
| `archive` | drop from roster, keep journal | worker not required live; router archives from roster/disk; a live worker is killed first |
| `resume` | `runner.Resume` from journal | router spawns a **fresh** worker with `--resume <uuid>`; also the normal "pick up an offline/old-binary session on the new binary" path |
| `session/list` | roster (live) ∪ disk enumeration | router serves **union**: adopted live workers (each reports its own `SessionInfo`) ∪ disk journals for offline/archived. Router links the SDK **session** package for journal-metadata reads + event decode only — never the runner/loop |
| usage/cost | `runner.Cost()` from journal | worker reports `Cost` in its `SessionInfo` snapshots; router reads cost from the journal directly for offline sessions |
| `-local` / daemonless | in-process supervisor | **unchanged — stays in-process.** A one-shot `gofer exec` or daemonless TUI run *is* its own process; forcing a worker there adds a fork for no upgrade benefit. Process isolation is a daemon-hosted-path feature only |

## 9. What lives where, and the SDK question

- **Worker links the SDK runner/loop** (version-coupled execution). **Router does not** — it links the SDK only for event decode + read-only journal-metadata parsing, both forward-compatible (unknown kinds dropped, additive JSONL fields ignored). This keeps the router upgradeable independent of workers and the compat surface honest.
- **The version handshake needs no SDK change.** `gofer/hello` is gofer-native, owned the same way `gofer/event` is.
- **One new SDK seam WAS required and taken: `runner.Options.SessionID`** (shipped in SDK v0.11.0, consumed at `cmd/gofer/session_worker.go`). *This corrects an earlier claim in this section that "no new SDK seam is required."* Option A (§4) has the router pre-generate a session's uuid to key the worker's socket, endpoint file and lock **before** the worker starts, so the worker must make its session adopt that exact id. gofer first bridged this with a stateful `IDGen` whose first draw returned the pinned uuid; the SDK then added `SessionID` specifically to replace that bridge, and the bridge was deleted. The seam is strictly better than what it replaced: it pins at session creation rather than "whenever the store happens to draw first," and it validates the id before it becomes a journal filename.
- **`event.Unmarshal` has landed in the SDK** (v0.11.0, `event/unmarshal.go`) — the canonical inverse of `MarshalJSON` this section anticipated. gofer has **not yet adopted it**; the hand-rolled per-kind switch still lives in `internal/wirestream/reconstruct.go`, *not* in `daemonbridge` as this section originally said — the reconstruction core was extracted to its own package during M6. Swapping the hand-rolled decoder for `event.Unmarshal` is tracked separately and is not a milestone blocker; the original judgement that gofer should ship without waiting on it held.

## 10. Costs & risks (stated honestly)

- **Memory: N processes, not N goroutines.** Each worker is a full Go runtime (~10–20 MB RSS baseline) + the loop's working set, vs a goroutine's KB. 20 sessions ≈ 20 processes. Acceptable for the target ("an operator running N agents" — tens, not thousands); the isolation is the product. Mitigated by `router.Config.MaxWorkers` (`gofer daemon --workers --max-workers N`, uncapped by default), which refuses `session/new` with `ErrAtCapacity` before forking once N workers are live.
- **Startup latency per session.** fork/exec + runtime init + `runner.New` + journal open (tens of ms) vs a goroutine (µs). Fine for human-initiated `session/new`; would matter only for rapid programmatic fan-out.
- **Per-event IPC tax.** The second hop doubles the per-event encode/socket cost already paid at the client hop. µs-scale, sub-perceptual for interactive use.
- **Compat-surface maintenance.** A supported internal wire with a version window. Mitigated because it is the *same* surface as the public client wire (already maintained), and the in-flight-only compat simplification (§6) shrinks the forward-support obligation to the additive event subset.
- **Portability.** `setsid` detachment is Unix-only — but the repo ships no Windows build, so no new loss.
- **What does NOT change:** the client-facing ACP surface; the journal format; the SDK contract; `-local` mode. Blast radius is `internal/daemon` (supervisor dependency → interface), a new `gofer session-worker` command reusing `daemon.Serve`+`supervisor`, and a router-side remote supervisor reusing `daemonbridge`.

**Deliberate cuts:** process isolation does **not** apply to `-local`/daemonless mode (§8), and full N-version bidirectional wire compat is deferred in favor of the in-flight-only subset (§6).

## 11. Phased rollout

Each phase lands shippable value on its own.

- **Phase 0 — prerequisite (shipped):** build-version identification (`vX.Y.Z` / VCS-derived pseudo-versions) + the daemon/CLI version-mismatch warning. Feeds the endpoint file's `binaryVersion` and `gofer/hello`.
- **Phase 1 — worker behind a flag; router must outlive it.** Extract the daemon's concrete `*supervisor.Supervisor` dependency to an **interface** (the methods `handlers.go` calls). Add `gofer session-worker` = a single-session `daemon.Serve` over a unix socket. Add a router-side remote supervisor (daemonbridge-shaped) that spawns one worker per `session/new` and proxies. **Not detached yet** — worker is an ordinary child; if the router dies, the worker dies (documented). Ships: **crash isolation**. *Demo:* `kill -9` a worker; the daemon and every other session survive; the killed session shows offline and resumes.
- **Phase 2 — detach + adoption.** `Setsid`; per-worker endpoint files + `<uuid>.lock`; router scans and adopts on startup; router stops signalling workers on shutdown; reaper handles zombies; the §4 failure matrix. *Demo:* restart the daemon while a session sits idle-attached; it survives and re-adopts; peers reconnect to live state.
- **Phase 3 — version skew + the hot-upgrade story.** `gofer/hello` handshake + endpoint version fields; router negotiates; the in-flight-only compat subset + skew tests; a skewed worker is observed + reply-able + allowed to finish, but new prompts route to a fresh worker. *Demo (the milestone criterion):* upgrade the daemon binary mid-turn; the running session finishes uninterrupted on the old worker; the next `session/new` runs the new binary; `session/list` shows mixed `binaryVersion`s.
- **Phase 4 — lifecycle completeness (polish).** `resume` spawns a fresh worker; cost/usage aggregation across workers; graceful worker drain on `gofer daemon uninstall`; roster reconciliation edge cases.

**Riskiest phase: Phase 1** — relocating the pump + gate into the worker and rewiring the daemon's supervisor to a remote proxy (the interface extraction).

**Cheapest de-risking prototype (do this first):** wire the **existing** `daemonbridge` as a router→worker proxy with **zero new wire code** — run one `gofer daemon` as the "worker" on a socket, and a second `gofer daemon` as the "router" whose supervisor is a `daemonbridge` pointed at the first. This proves reconstruction fidelity and the *double-hop* event + permission round-trip end-to-end before committing to the single-session-worker refactor.

## 12. Milestone framing

Slots as the **next** gofer milestone after M5 (ACP featureset), pushing ecosystem and auto+polish back one. **No SDK *milestone* was required** — the SDK's Event/Op contract, two-tier broker, and journal already supported out-of-process reconstruction. That held: M6 needed no SDK milestone, only two additive seams shipped in v0.11.0 (`runner.Options.SessionID`, adopted; `event.Unmarshal`, available but not yet adopted). See §9, which records the correction — the original "no new SDK seam is required" claim did not survive contact with Option A's id-pinning requirement.

**Honors the invariants:** contract-only SDK consumption (the boundary is the existing typed wire); everything-is-a-client (the router↔worker hop *is* an ACP+gofer client relationship — the worker gets no privileged path, and neither does the router); journals never deleted (they become the adoption/resume source of truth); SDK-promotion membership test applied to both v0.11.0 seams (see §9 — `runner.Options.SessionID` was required and is adopted; `event.Unmarshal` shipped but is not yet consumed).

---

**Implementation pointers (code-verified):** boundary seam is `internal/supervisor/types.go:59` (`Session` interface) + `supervisor.go:50-54` (injectable factories); the reusable cross-process template is `handleGoferEvent`, extracted during M6 from `daemonbridge` into `internal/wirestream/reconstruct.go` (this pointer originally named `internal/daemonbridge/reconstruct.go`, which no longer exists); the fan-out to preserve is `internal/daemon/handlers.go:broadcastGoferEvent`/`broadcastPermission`; the adoption primitives to mirror are `cmd/gofer/daemon.go:pidAlive`/`guardLiveEndpoint` + `internal/daemon/endpoint.go`.

Two facts verified against the code: (1) the SDK journal has **no** cross-process flock (only `auth.lock` exists) → single-writer-per-session is a gofer-side `<uuid>.lock`, not an SDK freebie; (2) there is **no** version in any handshake today → version negotiation is built into the gofer-native wire from day one (endpoint file + `gofer/hello`), not inherited from ACP.
