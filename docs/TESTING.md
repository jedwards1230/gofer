# gofer — testing strategy

The SDK owns loop/provider/session/permission testing (see agent-sdk-go's
`docs/TESTING.md`). This document covers **every kind of verification gofer
runs**, what each one can and cannot see, and which gate enforces it.

The organising idea: each layer below exists because the layer above it is
*blind* to something. Unit tests can't see colour; goldens can't see colour
either; VHS can't assert; benchmarks can't tell you whether the output is
correct. Knowing which blindness you're covering is how you pick the right
tool — and how you avoid writing a test that looks thorough and proves nothing.

## The layers at a glance

| Layer | Type | Gate | Sees | Blind to |
|---|---|---|---|---|
| Unit / model | `Update(msg)` → assert state | every PR | logic, state transitions | anything about rendering |
| Golden render | `testkit.AssertGolden` vs `testdata/*.golden` | every PR | exact text layout, `termenv.Ascii` | **colour**, ANSI width |
| VHS capture | rendered PNG/GIF frames | advisory, never gates | colour, styling, real terminal output | nothing is asserted — a human looks |
| Integration | in-process daemon over ws/ACP | every PR | wire protocol, fan-out, ordering | process boundaries |
| Process isolation | real detached `session-worker` processes | every PR | crash isolation, socket lifecycle | performance |
| Race | `go test -race ./...` | every PR + push | data races | logic errors a race doesn't expose |
| LSP live | real `gopls` round trip | `lsp-live` job | actual language-server behaviour | — (**skips** without gopls; see below) |
| Benchmarks | `scripts/bench.sh --check` | every PR (allocs only) | work that scales with input size | correctness; **waiting**; CPU-bound compare/sort |
| Cost assertions | ordinary `go test` on wall-clock | every PR | settle waits, super-linear CPU work | anything allocation-shaped (the gate has that) |
| Worker fleet | ~50 real processes, `workerbench` tag | manual only | RSS, spawn latency at scale | asserts almost nothing by design |

## Unit and golden tests

Two tiers in `internal/tui`:

1. `Update(msg)` → assert model state. **The majority**, and the default choice.
2. `testkit` helpers (`AssertGolden`/`AssertGoldenStyled`) against
   `testdata/*.golden`, for the few render-critical components. Lipgloss
   direct — **no PTY**, fully deterministic. `testkit` pins fixed sizes, forces
   `termenv.Ascii`, and uses `theme.Test()`.

Rules:

- Golden-file tests come **first** for any new render-critical component —
  before styling work, never after.
- Script turns in code (typed builders); JSONL fixtures only for captured
  session histories.
- Never test through a PTY; teatest is not a first move.
- An edit-committing view (e.g. `/config`) is tested by supplying a fake writer
  closure (`CommandEnv.SaveConfig`) that captures what was written, asserted
  alongside the golden render — never a real file on disk.
- Where a test harness mirrors production wiring, the production copy still
  needs its own test. `internal/router`'s faux worker re-implements the M6
  session-id pinning for router-side tests; the pin that actually ships lives in
  `cmd/gofer`'s `runSessionWorker` and is asserted there
  (`cmd/gofer/session_worker_test.go` drives the real entrypoint in-process:
  temp root, handshake off an `io.Pipe`, `session/new` over the worker's socket,
  no network). Deleting the pin must fail the suite.

## Integration and process isolation

- **daemon · ws/ACP**: a real in-process daemon over a WebSocket / JSON-RPC 2.0
  (ACP) transport; a real ws client drains `session/update` notifications.
  Deadlines in this suite are **failure timeouts** — a test fails if an expected
  event does not arrive in time.
- **process isolation**: real detached `session-worker` processes over unix
  sockets. `cmd/gofer/session_worker_test.go` covers the worker entrypoint and
  its pinned session id; `internal/router/crashisolation_test.go` covers a
  killed worker leaving the router and its other sessions intact.

## Visual capture (VHS)

Real rendered frames, because the goldens render `termenv.Ascii` and **cannot
see colour** — the #61 colour-scatter regression shipped past green goldens.
Anything whose meaning lives in colour, styling, or motion needs a tape.

VHS **asserts nothing**. It produces frames a human reads. That is its whole
value and its whole limit: it is the only layer that can catch "this looks
wrong," and the only layer that will never tell you so on its own.

A PR that adds or changes a visible TUI state should add or update a tape in the
same PR — the authoring rules, and the traps (deterministic frames, holding
mid-flight calls, mocking session history) live in
[`CONTRIBUTING.md`](../CONTRIBUTING.md#capture-new-tui-states-as-vhs-tapes-as-you-go).
The tape inventory and render workflow are in [`vhs/README.md`](../vhs/README.md).

Two CI lanes back it, **neither of them a required check**:

- `vhs-capture.yml` fires on PRs touching `internal/tui/**`, `vhs/**`, or
  `scripts/tui-vhs.sh`, renders every tape, and embeds the frames in the job
  summary and a sticky PR comment. Frames publish to a per-PR
  `vhs-captures-pr-<n>` branch, deleted when the PR closes
  (`vhs-capture-cleanup.yml`). Fork PRs get a read-only token and degrade to a
  `vhs-frames` artifact upload.
- `vhs-baseline.yml` re-renders every tape on push to `main` and commits the
  key-frames to `vhs/snapshots/`, so TUI changes show as native GitHub image
  diffs. **This is why a time-varying frame churns forever** (gofer#297) — see
  the determinism rule in CONTRIBUTING.

**Command-pane tab coverage.** Every command-pane tab has both an Ascii golden
(`testdata/app_panel_<tab>.golden`, driven through the App via `dispatchSlash`)
and a VHS tape (`vhs/panel-<tab>.tape`) reproducing the same state in colour:
**status, config, model, thinking, usage, stats, help, resume**. Each tape
reproduces exactly the state its golden pins, so the two never disagree.

## Performance

Benchmarks are the only layer that catches **work that scales with input size**.
Every correctness test above passes just as happily against an accidentally
quadratic implementation — which is not hypothetical here: gofer#298 (the roster
re-reads every journal every tick) and gofer#308 (`Model.Ingest` deep-copies the
whole transcript per event, making replay quadratic) were both found this way,
after users felt them.

```bash
scripts/bench.sh            # run everything, print results
scripts/bench.sh --check    # gate against the baseline (what CI runs)
scripts/bench.sh --update   # rewrite the baseline
BENCH_PKGS=./internal/tui/ scripts/bench.sh   # narrow, for local iteration
```

**The gate is on `allocs/op` and `B/op` only — never wall-clock.** ns/op on a
shared runner is too noisy for a threshold that both catches regressions and
avoids false alarms, and a gate that cries wolf gets ignored. At `-benchtime 1x`
allocation counts are exact for one iteration, so they reproduce across machines.
Tolerance is 25%: loose enough for a slice-doubling boundary, far tighter than
anything accidentally super-linear.

Both metrics are needed. `allocs/op` alone would have nearly missed gofer#308 —
the quadratic copy moved the count by only −15% while `B/op` moved **854×**,
because the cost was a few enormous copies rather than many small ones. `B/op`
alone would miss a leak of many tiny objects.

**One stated exemption: concurrent benchmarks gate on `allocs/op` only**, marked
`allocs-only` in the baseline. At `-benchtime 1x` a fan-out benchmark runs a
single iteration, so how much buffer its peer goroutines happen to allocate is
decided by scheduling — `BenchmarkBroadcastRawEvent`'s `B/op` was measured
swinging 3,920 → 12,120 (**+209%**) between identical runs of identical code. No
threshold there both catches a real regression and stays quiet. Its allocation
*count* is stable, and is what that benchmark's own doc calls its evidence. The
exemption is per-benchmark and visible in the baseline, not a blanket loosening.

In CI the lane writes a **job summary** — every benchmark's current allocs/op,
B/op and ns/op with its delta against the baseline — on a pass as well as a
failure. On a failure it is where you see *which* numbers moved without opening
the log; on a pass it is the per-PR record of what the hot paths currently cost,
which is the entire point of measuring and is invisible if it only ever lands in
a log nobody opens on a green run. ns/op appears there and nowhere else, labelled
indicative.

`bench/baseline.txt` is **committed**. Updating it is deliberate and shows up in
review as a diff, so a regression cannot be absorbed silently. A baseline entry
that stops running fails the gate as `MISSING`, so deleting or renaming a
benchmark cannot quietly retire its coverage.

What to benchmark: anything whose cost grows with **session count**, **transcript
length**, or **attached client count**. Sweep the axis rather than reporting one
number — the question is never "how fast is it" but "what happens as this grows",
and a single point cannot distinguish linear from quadratic. Where two axes exist,
sweep them **separately** (`BenchmarkOverviewRoster` vs
`…JournalDepth`): a fix that flattens one and not the other looks complete
against a combined benchmark.

Assert something inside the loop. A benchmark whose subject silently stops doing
the work reads as a spectacular optimisation.

### A benchmark's fixture decides what its number means

Two ways a benchmark can run, pass, and measure nothing — both found in gofer's
own suite (gofer#315), both invisible while green:

- **It never reaches the branch it is named after.** `BenchmarkOverviewRoster`
  built its fixture with `store.CreateWithID` and `supervisor.New`, which
  registers nothing live, so `liveByID()` was empty and the roster's live branch
  was never taken. It measured exactly the half that could not contain the cost
  being looked for. The fix is a guard *inside* the benchmark — see
  `assertAllLive` in `internal/supervisor/roster_live_bench_test.go`. A row count
  cannot catch this: a live row and a disk row are both one entry in the slice.
- **It stubs the expensive dependency to zero.** Every App render benchmark built
  through `GoldenCommandEnv`, whose `Config` closure returns an empty struct
  without touching disk — so it measured a frame in which the per-frame config
  reads were free, which the shipped app's are not.

A third, softer version: a fixture that under-weights the content. The transcript
benchmarks ingested one line of plain text per message, which never reaches the
markdown, code-block or tool-result paths; realistic content costs ~9x the
allocations. Every absolute figure in the baseline was that optimistic, and a
regression confined to those paths barely moved the gated number.

When one of these is fixed the numbers go **up**, and that is not a regression —
it is the benchmark starting to measure what it always claimed to. Say so in the
PR, per benchmark, with the before and after.

### Two classes the gate cannot see, by construction

The allocation gate is blind to two kinds of cost, and no amount of tuning it
helps. Both need an ordinary wall-clock assertion instead:

- **Waiting.** A settle wait, a backoff, a poll interval — all allocate nothing.
  gofer#313's root cause was in this class. `internal/mcpconn`'s
  `TestAwaitReadyBurnsItsBoundOnEveryCallWhileAServerHangs` covers the surviving
  instance: a hanging MCP server leaves the manager unsettled, so every session
  create pays the full ready timeout again.
- **CPU-bound compare and sort.** `matchFilePaths` scans and sorts up to 5,000
  paths on every keystroke while an `@` token is open — in **five** allocations,
  so a benchmark of it would gate on nothing at all. See
  `internal/tui/filemention_cost_test.go`.

Write these as a **ratio against the same code at a smaller input**, not as a
millisecond budget. A budget loose enough to survive a shared runner is loose
enough to pass the regression it was written for — the first draft of the
`matchFilePaths` assertion used a 20ms ceiling and stayed green against a
deliberately quadratic mutation that took ~10ms. Comparing 4x the input against
1x removes the machine from the assertion: linear work lands near 4x and
quadratic near 16x on a fast laptop and a loaded runner alike. Pick the threshold
from *both* measured endpoints — healthy and mutated — never from theory.

## Worker-fleet benchmark (M6, off by default)

`internal/router/bench_test.go` spawns ~50 real detached worker processes and
reports RSS, `Roster`/`List` cost, event throughput, and spawn latency. It is a
**measurement harness, not a correctness test**: it asserts almost nothing, and a
failure means a measurement could not be taken (or a process leaked), not that a
number moved.

It is gated behind the **`workerbench` build tag**, not `testing.Short()`. CI
runs bare `go test ./...` with no `-short` flag and `testing.Short()` is false by
default, so a Short-gated benchmark would run on every push and spawn 50
processes inside the runner. The build tag is the only gate that actually
excludes it. CI still **compiles** it (`go vet -tags workerbench ./...`) — a
tagged predecessor survived the deletion of the very code it described precisely
because nothing ever built it.

```bash
GOFER_BENCH_LOAD_NOTE="<what else was running>" \
GOFER_BENCH_OUT=fleet.txt GOFER_BENCH_FRAMES_OUT=frames.txt \
  go test -tags workerbench -run 'TestWorkerFleetBenchmark|TestRosterWireFrameCount' \
  -v -timeout 30m ./internal/router/
```

Fleet size and load are env-tunable (`GOFER_BENCH_WORKERS`,
`GOFER_BENCH_CALL_ITERS`, `GOFER_BENCH_FANOUT_*`) so a smaller machine can
produce a comparable, lower-N run rather than failing. **A run is only comparable
to another run with the same settings on the same machine** — always record them,
along with the commit SHA.

Not every metric carries the same weight. **Counts are authoritative**: frames per
call, allocations per operation, and process RSS reproduce across machines and are
safe to quote. **Wall-clock is indicative only**: latency and throughput move with
machine load and core count. Quoting a wall-clock figure without those conditions
attached says nothing.

## Tests that skip are worse than tests that are missing

The LSP live tests (`internal/lspdiag`, `internal/supervisor`) drive a real
language server and **skip themselves** when none is on `PATH`. A skipping test
is indistinguishable from a passing one in the output, so without `gopls` they
pass silently while proving nothing.

CI's `lsp-live` job installs `gopls` and **fails if those tests skip**. Locally,
install it before touching LSP wiring:
`go install golang.org/x/tools/gopls@v0.21.1` (the version CI pins).

## Verifying the test, not just the code

A green test is not evidence until you have seen it go red. Before treating a
new test as proof:

- **Mutation-test it.** Break the thing under test and confirm the check fails.
  A mutation is only evidence if the mutated tree still **builds** and the
  **named** test fails — a compile error proves nothing, and a mutation that
  matched nothing leaves the suite passing vacuously.
- **Match the assertion's shape to the property.** A timeout is a ceiling, not a
  floor; asserting a call takes *at least* N ms tests nothing against a
  fast-failing dependency.
- **Pair negative assertions with a positive.** "Stayed silent" and "never ran"
  are the same observation, so a silence-assertion alone cannot discriminate.
- **Check the harness can express the failure.** `tea.Batch` reaches `App.Update`
  as a `tea.BatchMsg` it has no case for, so the one-Cmd test helpers swallow it
  and both effects vanish — a test driving a batch without expanding it goes
  quietly vacuous rather than red (`TestSyncMenuReturnsAtMostOneCmd` exists to
  catch exactly this).

### Worked example: pinning an untested invariant before trading it away

gofer#308 is the canonical case. `Model.Ingest` deep-copied the whole transcript
per event to keep a prior `Model` observable — quadratic, 1.6s and ~9GB to open
a 5,000-turn session. The blocker was not the fix, it was that **the entire
`internal/tui` suite passed with the copy deleted**: nobody could tell a correct
optimization from a broken one.

So the invariant was written down as a test *first*, and mutation-tested three
ways before a line of it was optimized. `TestIngestDoesNotAliasPriorModel`
(since retired, see below) asserted a retained prior stayed unchanged, and each
of these mutations **built** and made that **named** test fail:

| mutation | observed failure |
|---|---|
| delete `m.items = append([]item(nil), m.items...)` | `prior.items[1] mutated by a child Ingest: got text:prior-childA-childB, want text:prior` |
| delete the `m.toolIndex` clone | `prior.toolIndex = map[call-child-a:3 call-child-b:3 call-prior:2], want map[call-prior:2]` |
| delete the `m.toolAgents` clone | `prior.toolAgents = map[call-child-a:… call-prior:…], want map[call-prior:…]` |

That is what made the *next* step decidable. The first failure is an in-place
element write (`m.items[idx].text += …` on a `MessageDelta`), which no
spare-capacity or ownership-watermark scheme can avoid — so value semantics and
an O(1) `Ingest` are genuinely incompatible, and the invariant had to be dropped
rather than optimized around. `Ingest` took a pointer receiver; the compiler
then enumerated every call site, and the only three outside tests
(`app.go` ×2, `adapter.go`) already discarded the parent.

**A pinning test earns its keep even when the fix retires it** — it converts
"this might break something" into a specific, priced trade. What replaces it
must pin whatever invariant *survives*: here that is
`TestWithHelpersDoNotAliasBaseModel`, since the render-local `With*` helpers are
now the only value-semantics surface. It is mutation-tested the same way —
making `WithThinking` or `WithBackgroundAgents` append into the shared array
builds fine and fails it with `tail is kind 2 after an Ingest on the base`.

## CI summary

`.github/workflows/ci.yml` on every PR: `go build ./...`, `go vet ./...`,
`go test ./...`, `go vet -tags workerbench ./...`, `golangci-lint`,
`scripts/bench.sh --check`, and `go test -race ./...`. Race runs on **every PR**
and on push to main / release tags — gofer is concurrency-heavy, so a data race
blocks the PR, not merely the release. The `lsp-live` job runs the LSP tests
against a real `gopls`.

Advisory, never gating: the two VHS lanes above.

## M3 exit gate — satisfied

M3's close required a **live multi-client pass**: two clients on one session (one
of them a phone) exercising fan-out + approvals — met at milestone close (#53).
Automated PR review caught zero of M2's cross-connection/ordering bugs; live
client testing caught all of them, so the golden/integration matrix could not
stand in for it here.
