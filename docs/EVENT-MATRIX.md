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

Rows are the SDK's full `event` union (`agent-sdk-go/event/`, 21 types, verified against the
`v0.23.0` tag gofer pins in `go.mod`). Columns are the five hops, **in stream order**:

| # | Path | Where |
|---|---|---|
| 1 | **SDK ACP projection** | `agent-sdk-go`'s `acp/project_out.go` |
| 2 | **gofer in-turn broadcast** | `internal/daemon/handlers.go` `handleSessionPrompt` |
| 3 | **gofer out-of-turn relay** | `cmd/gofer/daemon.go` `OnRegister` (in-process) · `internal/daemon/observer.go` (worker) |
| 4 | **`wirestream` reconstruction** | `internal/wirestream/reconstruct.go` — `--workers` mode only |
| 5 | **gofer TUI** | `internal/tui/model.go` `Model.Ingest` |

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
  `gofer/event` frames. Unlike 2 and 3 it is an explicit per-kind switch, which is exactly why
  it is where kinds go missing. In the in-process daemon there is no wirestream hop at all.

## The matrix

Legend: **Y** carried · **—** not carried (annotated below) · **n/a** cannot arise on this path.

| Event kind | 1. ACP | 2. in-turn | 3. out-of-turn | 4. wirestream | 5. TUI |
|---|---|---|---|---|---|
| `session.created` | — a | Y | Y | Y | — d |
| `session.resumed` | — a | Y | Y | Y | — d |
| `session.forked` | — a | Y | Y | Y | — d |
| `session.compacted` | **— GAP 1** | Y | Y | Y | Y |
| `session.killed` | — a | Y | Y | Y | — d |
| `session.archived` | — a | Y | Y | Y | — d |
| `session.spawned` | — a | n/a b | n/a b | — b | n/a b |
| `session.info` | Y | Y | Y | Y | **— GAP 3** |
| `session.config` | Y | Y | Y | **— GAP 2** | **— GAP 2** |
| `plan` | Y | Y | Y | **— GAP 2** | **— GAP 2** |
| `turn.started` | — a | Y | Y | Y | Y |
| `turn.finished` | Y (cond.) c | Y | Y | Y | Y |
| `message.started` | — e | Y | Y | Y | Y |
| `message.delta` | Y | Y | Y | Y | Y |
| `message.finished` | Y (cond.) c | Y | Y | Y | Y |
| `tool.call.started` | Y | Y | Y | Y | Y |
| `tool.call.delta` | — f | Y | Y | Y | Y |
| `tool.call.finished` | Y | Y | Y | Y | Y |
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
and is **false at v0.23.0**, the version `go.mod` pins. The SDK models parentage as first-class
journal state: `session/entry.go:117,123` persist `parent_id`/`depth` in the root `session_meta`
entry, `runner/runner.go:275-276` writes them via `session.WithMetaParent`, and
`runner/runner.go:420,443-444` recovers them on resume.

What makes the cell correctly empty is narrower and more durable: **gofer opts out of the SDK's
parentage on both sides.**

- *Write side* — no `runner.Options` literal in gofer sets `ParentID` or `Depth`: not
  `supervisor.go:682-686` (Create), not `:831-835` (Resume), not
  `cmd/gofer/{exec.go:99, run.go:308, resume.go:118, session_worker.go:150}`. So the
  `opts.ParentID != ""` guard at `runner/runner.go:275` never fires and the journal's parentage
  fields stay omitted (both `omitempty`) on every gofer session.
- *Read side* — `session.MetaOf`, `Runner.ParentID()` and `Runner.Depth()` have **zero** call
  sites in gofer.

Parentage lives solely in `internal/supervisor/sidecar.go`'s `sessionMeta{ParentID, Agent,
Depth}` → `<id>.meta.json` (writer `sidecar.go:163`, call site `supervisor.go:706`), per
CLAUDE.md's "visible artifacts over hidden state", with gofer's own depth cap in `resolveParent`
(`supervisor.go:760-781`) rather than the SDK's. The event's sole emitter is `runner.Runner.Spawn`
(`runner/runner.go:546`, the only publish site in the SDK), which gofer never calls — zero
`.Spawn(` hits across `internal/` and `cmd/`. So it is structurally never emitted here.

**Adding a handler would create a second source of truth for parentage.** The roster tree and
drill-in already work from the sidecar (`overview.go:287,310,555`; `app.go:851`). This is not a
"gap": a gap is information gofer wants and is not getting, and gofer has complete parentage from
an artifact it owns.

**Forward note.** This is a live fork in the road, not settled forever. If agent-initiated spawn
is ever built on `Runner.Spawn`, the SDK would start both emitting `session.spawned` *and* writing
`WithMetaParent` on paths gofer controls — producing exactly the dual source of truth this
annotation exists to prevent. Whoever does that work has to pick one owner for parentage first.
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
(`handlers.go:1010`, `:1035`) and wirestream (`reconstruct.go:578-603`) special-case permissions
onto `gofer/permission_requested` / `_resolved`. `reconstruct.go:367-369` documents it:
"permission.* deliberately never arrives via gofer/event".

**i — Emitted out of turn in one case, and dropped downstream. Harmless, but not unreachable.**
The obvious reading is that permissions only fire inside an active turn, so `promptHandlerActive`
(`event_relay.go:97-99`) always suppresses the out-of-turn relay for them. That is *almost* right
and the exception matters: a turn can run on a worker with **no** prompt handler in the worker
daemon — an adopted session finishing a turn its original router started
(`internal/router/router.go:1063`), where the original `session/prompt` died with its connection
and `endPromptHandler` already ran while the turn continued on the supervisor pump. In that
window a `permission.requested` *is* marshalled and shipped as a `gofer/event` frame.

Nothing breaks, because `handleGoferEvent`'s switch has no permission case, so the frame hits
`default` and is discarded — the same outcome the deliberate design intends
(`reconstruct.go:367-369`). The real permission path is the dedicated `gofer/permission_*`
methods either way. Recorded rather than smoothed over because "unreachable" and "reachable but
dropped" are different invariants, and a future filter added to `handleGoferEvent` would turn the
second into a live duplicate-delivery bug. This predates the worker-side observer: the in-process
watcher relays `liveSub.C` equally unfiltered.

**j — ACP has no error stop reason.** `StopReasonFor` returns `ok=false` for `"error"`
(`acp/project_out.go:209-226`), documenting that "a session.error event carries that signal
instead". A protocol limitation, not a gofer choice.

## The gaps

**GAP 1 — `session.compacted` never reaches a pure-ACP peer, in any mode.** `ToSessionUpdate`
has no `event.SessionCompacted` case; it falls to `default` (`acp/project_out.go:200-202`). This
is upstream of gofer and wider than the `--workers` relay: it affects the in-process daemon too.
Tracked as `jedwards1230/agent-sdk-go#139`. Note this is *orthogonal* to the worker-side relay —
fixing one does not fix the other.

**GAP 2 — `plan` and `session.config` are dropped by wirestream, so they are invisible under
`--workers`.** `handleGoferEvent`'s switch (`internal/wirestream/reconstruct.go:437-486`) has 16
cases and no `event.KindPlan` / `event.KindSessionConfig`; both fall to a `default` that returns
before reaching either `rec.broker.Publish(ev)` or the `r.sink(...)` push seam. Consequences:

- Neither reaches `Supervisor.SubscribeLive` (`internal/router/methods.go:64`), which is what the
  outer daemon's in-turn loop drains — so these are dropped **in-turn as well as out-of-turn**.
  This is strictly worse than the compaction gap, which was out-of-turn only.
- The drop happens one layer *below* the out-of-turn relay, so the worker-side observer added
  for `session.compacted` does not incidentally fix it.
- Both kinds are genuinely reachable: `plan` from the builtin `update_plan` tool
  (`internal/config/tools.go:25`, emitted at `agent-sdk-go/loop/loop.go:386`), `session.config`
  from `advertiseModelChange` (`internal/daemon/handlers.go`) on every model swap.
- In-process is unaffected — that path has no wirestream hop and its watchers are generic.

**GAP 3 — `session.info` has no live path into the TUI.** No `case` for it in `Model.Ingest`,
and it is *not* named in the deliberate fall-through comment at `model.go:561-564`. Titles are
sourced instead from the roster snapshot on the ~1s tick (`status.go:77-80`,
`overview_render.go:554`, `peek.go:76`), so a rename during an active attach is invisible until
the next refresh. Minor, and self-correcting — recorded because "not handled and not documented
as deliberate" is exactly the ambiguity this table exists to remove.

## Related

- [`PRD.md`](PRD.md) — the compaction-visibility requirement these paths serve.
- [`TESTING.md`](TESTING.md) — which layer can actually observe which of these hops.
- `internal/router/doc.go` — the `--workers` transport in detail.
