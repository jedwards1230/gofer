# Event kind × transport path

Which of the SDK's event kinds survive each hop out of the broker, and — for every kind that
does not — whether that is **deliberate** or a **gap**.

This table exists because an empty cell meaning "we chose not to carry this" and an empty cell
meaning "this is silently broken" look identical in the code. Answering "is kind X visible to
client Y?" otherwise means reading five files across two repos, and the last three people who
tried got a different wrong answer each time.

**Update this table when you add an event kind, a transport, or a handler.** A kind added
without a row here is a kind whose gaps nobody will notice.

## How to read it

Rows are the SDK's full `event` union (`agent-sdk-go/event/`, 23 types, verified against the
`v0.24.0` tag gofer pins in `go.mod`). Columns are the five hops, **in stream order**:

| # | Path | Where |
|---|---|---|
| 1 | **SDK ACP projection** | `agent-sdk-go`'s `acp/project_out.go` |
| 2 | **gofer in-turn broadcast** | `internal/daemon/handlers.go` `handleSessionPrompt` |
| 3 | **gofer out-of-turn relay** | `cmd/gofer/daemon.go` `OnRegister` (in-process) · `internal/daemon/observer.go` (worker) |
| 4 | **`wirestream` reconstruction** | `internal/wirestream/reconstruct.go` — `--workers` mode only |
| 5 | **gofer TUI** | `internal/tui/model.go` `Model.Ingest`, plus `internal/tui/compact.go` `App.applyCompactionEvent` for the two `Y o` rows |

**Column 1 is first because omitting it is how you get a false negative.** The first hop out of
the event stream is not in gofer at all — it is in the SDK. An investigation that greps only
`gofer/internal` and `gofer/cmd` concludes that `plan` and `session.config` are unhandled
everywhere. They are not: both project to ACP, so they reach a remote ACP client while being
invisible to the local TUI. That inversion is counter-intuitive enough that it has been
re-derived incorrectly more than once.

Two structural notes that explain most of the table:

- **Columns 2 and 3 are generic.** Neither switches on kind — both marshal whatever arrives and
  hand it to the same guarded relay methods (`BroadcastRawEvent` / `BroadcastSessionUpdate`).
  So they have no per-kind gaps by construction, and a new event kind joins them for free.
  Permission events are the one exception: they are deliberately special-cased onto dedicated
  wire methods rather than `gofer/event`.
- **Column 4 applies only under `--workers`,** where it is the *sole* decode path for a worker's
  `gofer/event` frames. In the in-process daemon there is no wirestream hop at all. It *used* to
  be an explicit per-kind switch, which is exactly why it was where kinds went missing; it now
  delegates to the SDK's own `event.Unmarshal`, the maintained inverse of the `MarshalJSON` that
  wrote the frame, so it is generic like 2 and 3 and a new kind joins it for free.
  `TestReconstructCarriesEveryEventKind` fails the build if that ever regresses.

## The matrix

Legend: **Y** carried · **—** not carried (annotated below) · **n/a** cannot arise on this path.

| Event kind | 1. ACP | 2. in-turn | 3. out-of-turn | 4. wirestream | 5. TUI |
|---|---|---|---|---|---|
| `session.created` | — a | Y | n/a k | Y | — d |
| `session.resumed` | — a | Y | n/a k | Y | — d |
| `session.forked` | — a | Y | Y | Y | — d |
| `session.compacted` | **— GAP 1** | Y | Y | Y | Y |
| `session.compaction_started` | — a | Y | Y | Y o | Y o |
| `session.compaction_failed` | — a | Y | Y | Y o | Y o |
| `session.killed` | — a | Y | Y | Y | — d |
| `session.archived` | — a | Y | Y | Y | — d |
| `session.spawned` | — a | n/a b | n/a b | — b | n/a b |
| `session.info` | Y | Y | n/a k | Y | **— GAP 2** |
| `session.config` | Y | Y | Y | Y n | **— GAP 2** |
| `plan` | Y | Y | n/a l | Y n | **— GAP 2** |
| `turn.started` | — a | Y | Y | Y | Y |
| `turn.finished` | Y (cond.) c | Y | Y | Y n | Y |
| `message.started` | — e | Y | Y | Y | Y |
| `message.delta` | Y | Y | Y | Y | Y |
| `message.finished` | Y (cond.) c | Y | Y | Y | Y |
| `tool.call.started` | Y | Y | Y | Y | Y |
| `tool.call.delta` | — f | Y | Y | Y | Y |
| `tool.call.finished` | Y | Y | Y | Y n | Y |
| `permission.requested` | Y g | Y h | — i | Y h | Y |
| `permission.resolved` | Y g | Y h | — i | Y h | Y |
| `session.error` | — j | Y | Y | Y | Y |

`ToSessionUpdate` (`acp/project_out.go:41-203`) projects **8** event types — not the 9 an earlier
investigation reported. `event.ConfigOptionBoolean` is a nested `event.ConfigOptionKind` value
discriminated *inside* the `ConfigOptionsUpdated` case body (`:175`), not a top-level event type.

## Why the empty cells are empty

**a — Session lifecycle is outside the ACP `session/update` surface.** Deliberate; the SDK says
so at `acp/project_out.go:23-25`, which names `turn.started` explicitly. ACP has no notion of a
session roster, so there is nothing to project these onto.

**b — `session.spawned` is deliberate: gofer owns parentage end-to-end, on both sides of the SDK
boundary.**

Do not justify this row with "the SDK has no concept of a session parent" — that was true once
and is **false at v0.24.0**, the version `go.mod` pins (re-verified against that tag: the line
citations below all still resolve). The SDK models parentage as first-class
journal state: `session/entry.go:117,123` persist `parent_id`/`depth` in the root `session_meta`
entry, `runner/runner.go:275-276` writes them via `session.WithMetaParent`, and
`runner/runner.go:420,443-444` recovers them on resume.

What makes the cell correctly empty is narrower and more durable: **gofer never calls the SDK's
`Spawn`, and never reads the SDK's parentage back.**

- *Write side* — `Create` DOES forward `ParentID`/`Depth` into `runner.Options`
  (`supervisor.go:804-809`), deliberately: those three assignments put the child runner in
  exactly the state `runner.Spawn` would construct, without calling it (the reasoning is at
  `supervisor.go:785-803`). So for a subagent session the `opts.ParentID != ""` guard at
  `runner/runner.go:275` *does* fire and the journal's root meta entry *does* record
  `parent_id`/`depth`. That copy is a write-only PROJECTION. `Resume` (`:960-965`) passes only
  `MaxDepth` — `runner.Resume` recovers lineage from the journal itself — and
  `cmd/gofer/{exec.go:99, run.go:308, resume.go:118}` pass neither, so a root session still
  omits both fields (`omitempty`).
- *Read side* — `session.MetaOf`, `Runner.ParentID()` and `Runner.Depth()` have **zero** call
  sites in gofer. Nothing reads that projection back, which is what keeps the sidecar the single
  authority.

Parentage's authority is `internal/supervisor/sidecar.go`'s `sessionMeta{ParentID, Agent,
Depth}` → `<id>.meta.json` (writer `sidecar.go:204`, call site `supervisor.go:829`), per
CLAUDE.md's "visible artifacts over hidden state", with gofer's own depth cap in `resolveParent`
(`supervisor.go:883-904`) rather than the SDK's. The event's sole emitter is `runner.Runner.Spawn`
(`runner/runner.go:546`, the only publish site in the SDK), which gofer never calls — zero
`runner.Runner.Spawn` call sites across `internal/` and `cmd/`. (`internal/subagent/tool.go:209`
calls `Spawn` on gofer's *own* `Spawner` seam, which is a different method.) So it is
structurally never emitted here.

**Adding a handler would create a second source of truth for parentage.** The roster tree and
drill-in already work from the sidecar (`overview.go:287,310,555`; `app.go:878,1642,1814`). This
is not a "gap": a gap is information gofer wants and is not getting, and gofer has complete
parentage from an artifact it owns.

**Forward note.** This is a live fork in the road, not settled forever. The `WithMetaParent`
write already happens (see the write side above); what a spawn built on `Runner.Spawn` would add
is the SDK *emitting* `session.spawned` on a path gofer controls, and a second reader for
parentage — producing exactly the dual source of truth this annotation exists to prevent.
Whoever does that work has to pick one owner for parentage first.
Related: `runner.New` does **not** enforce `Depth > MaxDepth` — `ErrMaxDepth`
(`runner/runner.go:48`) is returned only from `Spawn` (`:530-531`) — so gofer's `resolveParent`
cap is currently the *only* depth enforcement on every path it uses. Do not assume the SDK caps
depth on the new/resume path.

**c — Conditional, by design.** `turn.finished` projects to `usage_update` only when
`ContextWindow > 0 && used > 0` (`:117`) — there is nothing to report otherwise.
`message.finished` projects only for `MessageKind == MessageUser` (`:53`); assistant text and
reasoning already streamed as deltas, so re-projecting the finished message would duplicate them.

**d — Deliberate, and already documented in the TUI.** `internal/tui/model.go:561-564` names
these lifecycle kinds and states they "carry no transcript-visible state in the minimal attach
surface", falling through untouched. The roster, not the transcript, is where they surface.

**e — `message.started` has no ACP counterpart.** Agent text and reasoning stream via deltas;
ACP has no separate "message beginning" signal (`acp/project_out.go:12-18`).

**f — ACP has no incremental tool-output chunk** (`acp/project_out.go:19-22`). The finished
tool call carries the output.

**g — Projected, but through a different function.** `ToRequestPermission`
(`acp/project_out.go:239-245`) emits a `session/request_permission` *request*, not a
`session/update` notification — which is why they are absent from `ToSessionUpdate`'s switch.
Reading only that switch makes these look like gaps; they are not.

**h — Carried on dedicated wire methods, not `gofer/event`.** Both the in-turn broadcast
(`handlers.go:1037`, `:1070`) and wirestream (`reconstruct.go:581-606`) special-case permissions
onto `gofer/permission_requested` / `_resolved`. `reconstruct.go:413-414` documents it:
"permission.* deliberately never arrives via gofer/event".

**i — Emitted out of turn in one case, and dropped downstream. Harmless, but not unreachable.**
The obvious reading is that permissions only fire inside an active turn, so `promptHandlerActive`
(`event_relay.go:97-99`) always suppresses the out-of-turn relay for them. That is *almost* right
and the exception matters: a turn can run on a worker with **no** prompt handler in the worker
daemon — an adopted session finishing a turn its original router started
(`internal/router/router.go:1287-1288`), where the original `session/prompt` died with its connection
and `endPromptHandler` already ran while the turn continued on the supervisor pump. In that
window a `permission.requested` *is* marshalled and shipped as a `gofer/event` frame.

Nothing breaks, because `handleGoferEvent` drops both permission kinds with an EXPLICIT kind
check (`reconstruct.go:534-536`) rather than by falling off a decode — `event.Unmarshal` decodes
them perfectly well, so the contract is enforced there or nowhere. The real permission path is
the dedicated `gofer/permission_*` methods either way. Recorded rather than smoothed over
because "unreachable" and "reachable but dropped" are different invariants, and REMOVING that
check would deliver such a frame twice: once here and once on its own method. This predates the
worker-side observer: the in-process watcher relays `liveSub.C` equally unfiltered.

**j — ACP has no error stop reason.** `StopReasonFor` returns `ok=false` for `"error"`
(`acp/project_out.go:209-226`), documenting that "a session.error event carries that signal
instead". A protocol limitation, not a gofer choice.

## The gaps

**GAP 1 — `session.compacted` never reaches a pure-ACP peer, in any mode.** `ToSessionUpdate`
has no `event.SessionCompacted` case; it falls to `default` (`acp/project_out.go:200-202`). This
is upstream of gofer and wider than the `--workers` relay: it affects the in-process daemon too.
Tracked as `jedwards1230/agent-sdk-go#139`. Note this is *orthogonal* to the worker-side relay —
fixing one does not fix the other.

**GAP 2 — `session.info`, `session.config` and `plan` have no live path into the TUI.** No
`case` for any of them in `Model.Ingest`, and none is named in the deliberate fall-through comment
at `model.go:561-564`. For `session.info` the effect is minor and self-correcting: titles come
instead from the roster snapshot on the ~1s tick (`status.go:77-80`, `overview_render.go:554`,
`peek.go:76`), so a rename during an active attach is invisible only until the next refresh. For
`plan` and `session.config` there is no such fallback — the TUI simply never renders a plan, in
ANY mode, and tracks a model change only through the roster.

This is recorded rather than fixed because "not handled and not documented as deliberate" is
exactly the ambiguity this table exists to remove. **Do not confuse it with the transport gap
below**: fixing wirestream (note n) put both kinds on the wire and into every ACP client, and
changed nothing about the TUI, because the two failures are at different layers.

## Fixed

**n — `plan`, `session.config`, `TurnFinished.ContextWindow` and `ToolCallFinished.Edits` used to
be dropped by wirestream under `--workers`; they are carried now.** `handleGoferEvent` decoded
frames with a hand-rolled 16-case switch over `event.New*` constructors plus a local struct
mirroring the union's payload fields. Two decoders for one encoding is a synchronization
obligation, and it had been unmet four ways at once — silently, because a kind nothing decodes is
indistinguishable from a kind nothing sends:

- `plan` and `session.config` had no case at all and fell to a `default` returning before BOTH
  `rec.broker.Publish(ev)` and the `r.sink(...)` push seam. Neither reached
  `Supervisor.SubscribeLive` (`internal/router/methods.go:64`) either, which is what the outer
  daemon's in-turn loop drains — so both were dropped **in-turn as well as out-of-turn**, strictly
  worse than the compaction gap that was out-of-turn only, and one layer BELOW the out-of-turn
  relay, so the worker-side observer did not incidentally fix them.
- `TurnFinished.ContextWindow` and `ToolCallFinished.Edits` were shed field-wise: the SDK sets both
  on the built event after construction, so no constructor signature could carry them. The first
  was user-visible — ACP gates its `usage_update` projection on `ContextWindow > 0` (note c), so a
  pure-ACP peer attached through a router received no usage update at all.

The fix is to delegate to the SDK's `event.Unmarshal`, the maintained inverse of the `MarshalJSON`
that wrote the frame, and delete the local mirror — which removes the obligation instead of
re-discharging it. `TestReconstructCarriesEveryEventKind` pins every kind's full payload round
trip against it, so a reintroduced per-kind path fails the build.

**o — the two kinds SDK v0.24.0 added, and what they cost under each decode.**
`session.compaction_started` and `session.compaction_failed` are published by `runner.Compact`
(`runner/compact.go:231,269,283,297`) — the SAME out-of-turn path `session.compacted` takes, so
they became live gofer events the moment the SDK was bumped.

Be precise about what this demonstrates, because the obvious stronger claim is false: they were
NOT silently dropped. `jedwards1230/gofer#300` (PR #351) hand-added both cases to the switch,
along with a `Messages` field and a paragraph explaining why the wire key is `messages` and not
`session.compacted`'s `messages_compacted` — an asymmetry the SDK's own decoder already handles.
That is the actual cost: **two kinds arriving meant a human had to notice, hand-write two cases,
add a payload field, and get a naming subtlety right, in a PR about a TUI indicator.** Under the
delegation they are carried with no change to `internal/wirestream` at all. The kinds that DID go
silently missing are note n's four, which no such PR happened to cover.

`internal/router/outofturn_compact_test.go` asserts `session.compaction_started` end to end
rather than trusting the unit round trip, since it is the newest kind to cross the whole chain.

Column 5 is `Y` for both: PR #351's compaction indicator consumes them directly
(`internal/tui/compact.go:78,107`), and `app.go:160` records that `session.compaction_started` is
the ONLY signal an automatic compaction has begun. That is worth pairing with the transport
above — the indicator is a TUI feature whose entire premise is that these two events arrive, and
under `--workers` they arrive only because something decodes them.

**k — unreachable, not carried.** `session.created` and `session.resumed` are published *before*
any daemon-side subscription can exist (SDK `session/session.go:113`; `runner/runner.go:462`), and
both out-of-turn relays subscribe LIVE (no retained backlog — see `observer.go`'s subscribe-timing
note). `session.info`'s only out-of-turn emit is the first-title capture on the
Create-with-prompt path (`internal/supervisor/managed.go:354`), which is pre-subscribe for the
same reason; on `session/prompt` the emit is in-turn. The mechanism in these cells is generic and
would carry them — nothing ever arrives on it. This is the same reachable-vs-carried distinction
note i draws for permissions.

**l — `plan` is in-turn only.** Its sole publish site in the SDK is inside the agent loop
(`loop/loop.go:386`, from the `update_plan` builtin), so a prompt handler is necessarily active in
the worker daemon and the out-of-turn relay never sees it.

**m — a stale-roster sibling, still open.** The daemon's `advertiseModelChange`
(`internal/daemon/handlers.go:1600`) would re-advertise `session.config` locally, which would
have masked the transport gap above. It does
not fire, because `applyRosterEvent` (`internal/router/rostercache.go:119-160`) folds
Status/Title/Usage/Cost but **not** `Model`, and the cache is seeded once
(`rostercache.go:85-104`) with no re-seed. So under `--workers` the router serves a stale `Model`
for a handle's whole life after any `SetModel` — `gofer ps`, `session/list` and the TUI status /
`/model` surfaces report the OLD model — and `current == prev` suppresses the advertisement.
Wrong data rather than absent data, and out of scope here (roster aggregation, not event
transport): tracked as `jedwards1230/gofer#352`.

## Related

- [`PRD.md`](PRD.md) — the compaction-visibility requirement these paths serve.
- [`TESTING.md`](TESTING.md) — which layer can actually observe which of these hops.
- `internal/router/doc.go` — the `--workers` transport in detail.
