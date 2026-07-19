# M6 worker-fleet benchmark — baseline and after-run

Measurements for the M6 per-session worker fleet, taken **before and after** the
marshal-once event bridge and push-based roster cache landed (Slice 3b). It
exists so those two optimizations are shown with numbers rather than claimed.

- **[Results: before → after](#results-before--after-slice-3b)** — the comparison.
- **Baseline detail** — everything below it: the pre-3b run, its run conditions,
  and how to read each metric.

| | |
|---|---|
| **Commit measured** | `5051675362ec2dacc83dcc6183da2babcd49fa7f` (`m6` tip, Slice 3a merged) |
| **Harness** | `internal/router/bench_test.go` (build tag `workerbench`) |
| **Date (UTC)** | 2026-07-19 |
| **Machine** | `go1.25.6 darwin/arm64`, Apple M2 Pro, NumCPU 12, GOMAXPROCS 12 |
| **Machine load** | Otherwise idle — concurrent agents were down during an API outage; verified with `pgrep` before starting |
| **Fleet** | 50 workers requested, **50 achieved**; router `MaxWorkers = 50` (admission control engaged, not bypassed) |
| **Roster iters** | 20 timed calls per checkpoint |

The commit is stamped because it is immutable: this baseline is re-derivable at
any time by checking out `5051675` and re-running the harness. A comparison run
that does not match the machine and fleet settings above is not a comparison.

---

# RESULTS: before → after Slice 3b

Measured after the marshal-once event bridge and push-based roster cache landed.
**After-run commit `2e3c721`**, same machine, same fleet size (50/50), machine
idle both times — the run conditions match, so the comparison is valid.

## 1. Roster RPCs — the claim, confirmed

| Fleet | Frames per `Roster()` — before | after | per `List()` — before | after |
|--:|--:|--:|--:|--:|
| 1 | 1.0 | **0.00** | 1.0 | **0.00** |
| 2 | 2.0 | **0.00** | 2.0 | **0.00** |
| 12 | 12.0 | **0.00** | 12.0 | **0.00** |
| 25 | 25.0 | **0.00** | 25.0 | **0.00** |
| 50 | 50.0 | **0.00** | 50.0 | **0.00** |

**Exactly zero at every fleet size.** The per-call RPC is *gone*, not cheaper —
`Roster` now reads an atomic snapshot published by each handle's watcher
(`internal/router/methods.go`), with no lock, no RPC, and no copy.

**A one-time warm-up cost replaces it**, reported separately because folding it
into a per-call average would both understate the win and invent a phantom
recurring cost:

| Fleet | One-time warm-up (total frames) |
|--:|--:|
| 1 | 3 |
| 12 | 25 |
| 25 | 51 |
| 50 | 101 |

That is `2N+1`: one seed RPC per worker at adoption, plus roughly one cache-miss
fallback per worker when an early `Roster` call races the async seed. It is paid
**once**, not per call — and the fallback is deliberate design, not a defect: a
nil snapshot is `Roster`'s cache-miss signal, so a worker that fails to seed
degrades to the pre-cache behaviour instead of vanishing from the roster.

> **Measuring this honestly required separating the two costs.** A cold-router
> measurement conflates them: seeding is asynchronous, so seed RPCs land during
> the first calls and appear as a small fractional "per-call" cost. An early
> version of this after-run reported **0.4–0.6 frames per `Roster()` call** for
> exactly that reason. That number was an artifact of the measurement window, not
> a property of the cache. The harness now settles the cache before measuring
> steady state, and reports warm-up separately.

## 2. Fan-out allocations — work eliminated, not reduced

**The path the baseline measured no longer exists.** `Daemon.BroadcastRawEvent`
(`internal/daemon/event_relay.go:97`) forwards the worker's `gofer/event` params
**verbatim** — there is no `json.Marshal` anywhere on that path.

So the baseline's **14 allocs/op** (`message.delta`) and **17 allocs/op**
(`tool.call.finished`) are not the "before" of a faster version of the same
operation. They are the cost of an operation that **is no longer performed** on
the forwarding path. The honest statement is *this work is no longer done* — not
a percentage improvement.

> **Do not re-run `BenchmarkEventForward` and quote it as an "after" number.** It
> models the decode+re-encode in a self-contained way, so it still reports ~14/17
> — but it is now measuring a path production does not take. Measuring the real
> forwarding path (and deleting this model) is tracked separately; until then,
> the baseline's allocations section describes removed work, not current work.

## 3. Roster latency — collapsed, and now genuinely flat

| Fleet | Roster mean, before (ms) | after (ms) |
|--:|--:|--:|
| 1 | 0.06 | 0.01 |
| 12 | 1.04 | 0.03 |
| 25 | 2.00 | 0.01 |
| 50 | **4.17** | **0.03** |

Consistent with the frame count above and with the structural argument — but
this is **indicative only**. See the baseline's "Roster / List latency vs fleet
size" section below, which explains why this table cannot carry a scaling claim
in either direction; the frame count is the authoritative version. What is worth noting is that the after-numbers no longer
*rise* with fleet size at all: ~0.01–0.03 ms flat from N=1 to N=50, where before
they tracked N.

**The 15-second stall risk is gone too.** Before, `Roster` iterated workers
serially with a 15 s per-worker timeout, so one wedged worker stalled every call.
Now a wedged worker's snapshot is simply served stale — fleet visibility is no
longer hostage to its slowest member.

## 4. Unchanged, as expected

| Metric | Before | After |
|---|--:|--:|
| Per-worker RSS (p50) | 12.9 MiB | 12.7 MiB |
| Fleet total RSS (N=50) | 647.5 MiB | 636.1 MiB |
| Spawn latency (p50) | 24.90 ms | 23.91 ms |
| Throughput | 18,562/sec | 27,125/sec |

RSS and spawn are unchanged within noise, as they should be — 3b touched neither.
Throughput rose ~46%, but that is an **indicative** wall-clock figure, and the
run-to-run instability documented in the baseline sections applies to it; do not
quote it as a result.

---

## Reproducing

```bash
git checkout 5051675
GOFER_BENCH_LOAD_NOTE="<describe machine load>" \
GOFER_BENCH_OUT=fleet.txt GOFER_BENCH_FRAMES_OUT=frames.txt \
  go test -tags workerbench -run 'TestWorkerFleetBenchmark|TestRosterWireFrameCount' -v -timeout 30m ./internal/router/
go test -tags workerbench -run '^$' -bench BenchmarkEventForward -benchmem ./internal/router/
```

## How to read these numbers

Metrics are tiered by how much they can be trusted as evidence. **Only the
authoritative ones may carry a claim.** Two separate things can weaken a metric:

- **Machine contention** — wall-clock numbers move with whatever else is running.
- **Modeling** — a benchmark that reimplements production code measures a
  *replica*, and a replica can drift from the real thing silently.

A metric is authoritative only if neither applies: it must count or measure
**real code paths** and be **contention-insensitive**.

This is why the tiers are worth the trouble rather than caveating everything
uniformly: **the authoritative tier is immune to the weaknesses that affect the
indicative one.** Neither the frame count nor RSS goes through the modeled
decoder, and neither is a duration — so even if the replica in §3 turns out to
have drifted from production, or a run is measured on a busy machine, the
baseline's load-bearing claims still stand.

---

## What the numbers say (the headline finding)

**The roster fan-out, not memory, is what binds first on this hardware.**

At ~12.9 MiB per worker, 50 workers cost 647 MiB — comfortable on any machine
that would run this. Memory is not the constraint.

The roster call pattern is. The TUI polls `Roster()` on a **1-second cycle**
(`rosterInterval`, `internal/tui/app.go:29`), and each call fans out **one
sequential RPC per live worker**. So an operator running 50 agents pays **50 wire
round-trips every second**, just to keep the roster painted.

**The decisive problem is structural, not statistical.** `Roster` iterates live
workers **serially**, each call bounded by `wireCallTimeout = 15 s`
(`internal/router/router.go:39`, `methods.go:170`). So **a single wedged worker
stalls every `Roster` call for up to 15 seconds** — and with the TUI polling on a
1-second cycle, the roster simply stops updating for that whole window. Fleet
roster latency is hostage to its slowest member. That does not depend on a slope,
an anchor, or how busy the machine was; it follows from the loop.

Two things make this worse than it first looks:

- **It is unconditional.** The cycle is self-clocking and ungated — handling a
  roster response re-arms the tick, and the tick unconditionally refetches
  (`internal/tui/app.go:433,435`). There is no focus, view, or reachability gate,
  so the cost is paid on the **idle path**, with nothing happening on screen.
  (Strictly the period is `rosterInterval` *plus* the call's own latency, since
  the next tick is armed on response rather than on a fixed schedule — at N=50
  that is ~1.004 s, so "once a second" is accurate in practice.)
- **It is per attached client.** Each attached TUI runs its own cycle, so the
  steady-state cost is `clients × workers` round-trips per second. Two operators
  on a 50-worker fleet is 100/sec; three is 150/sec.

So the load is proportional to the **product of fleet size and attached
clients** — which is the shape that actually stops scaling, since both factors
grow exactly when the tool is being used most.

**It self-throttles, and that converts the failure mode rather than removing
it.** Because the next tick is armed on response, each client has at most one
roster call in flight — requests cannot pile up or queue, so a large fleet does
not overload the daemon. Instead, as N grows and each call slows, the effective
poll rate *drops*: the operator's roster silently goes **stale**. At 50 workers
that is ~1.004 s and invisible. At a fleet large enough for roster calls to take
hundreds of ms, the TUI would display meaningfully out-of-date state while still
looking live — arguably worse than an obvious slowdown, because nothing signals
to the user that what they are reading is old.

Serving the roster from a cache removes both halves: the call cost becomes ~free,
so the cycle stays at its interval and displayed state stays fresh no matter how
large the fleet grows.

That reframes the push-based roster cache from an optimization to **the single
highest-leverage scalability fix in this milestone**. Taking the frame count from
N to 0 is not a micro-win: it removes a sustained, `clients × workers` load from
the steady-state idle path.

---

## AUTHORITATIVE

### 1. `gofer/roster` frames per call

The strongest evidence in this document, and the one that fully carries the
push-based-roster claim on its own. It counts **real frames produced by real
router code**, and it is a count rather than a duration, so no amount of CPU
contention can change it.

| Workers | Frames per `Roster()` | Frames per `List()` |
|--------:|----------------------:|--------------------:|
| 1 | 1.0 | 1.0 |
| 2 | 2.0 | 2.0 |
| 12 | 12.0 | 12.0 |
| 25 | 25.0 | 25.0 |
| 50 | 50.0 | 50.0 |

**Exactly N at every checkpoint — perfectly linear.** Today the router asks every
live worker for its roster on every call. A push-based roster cache replaces
those per-call RPCs with a local read, so the expected post-3b value is **0**.
That is a structural before/after no drift or noise can fake.

*(The worker end here is an in-process counting fake adopted through the router's
real adoption path — deliberate, because frames-per-call is a property of the
router's call pattern and counting frames requires a controllable worker end.
Every other metric below used 50 real detached processes.)*

### 2. Process RSS

M6 §10 claims "~10–20 MB RSS baseline" per worker. Memory holds far steadier
under CPU contention than time does, and this is the number that answers the real
question: how many agents fit on one box.

| | |
|---|---|
| Workers sampled | 50 |
| Per-worker RSS | min 11.4 MiB · p50 **12.9 MiB** · p90 13.2 MiB · max 13.9 MiB |
| Fleet total | **647.5 MiB** |
| Router (test proc) | 11.5 MiB → 22.1 MiB (delta 10.5 MiB) |

**The §10 estimate holds** — p50 of 12.9 MiB sits in the low half of the
predicted 10–20 MB band, and the spread is tight (11.4–13.9 MiB). At ~13 MiB
each, a 50-worker fleet costs roughly 0.65 GiB of worker RSS.

---

## INDICATIVE ONLY

Recorded for context under the load stated above. The *shape* is the signal;
absolute values are not. Do not tune against these, and do not let a conclusion
rest on them.

### 3. Allocations per forwarded event — MODELED, not measured in production

This measures the **router's second hop**: decoding a worker's `gofer/event`
envelope into a typed `event.Event` and re-encoding it. That is the cost the M6
worker split introduced and that a marshal-once bridge removes.

> **Common misconception, worth stating plainly:** this is **not** the
> daemon→client hop, and it does **not** scale with subscriber count.
> `daemon.broadcastGoferEvent` already marshals each event **once** and reuses
> the bytes for every peer (`internal/daemon/handlers.go`). Marshal-once already
> exists at that hop; the router's re-encode is what is new.

> **Indicative, not authoritative:** it **models** the production decoder rather
> than calling it. gofer's real decoder is unexported in `internal/wirestream`,
> which this harness may not modify, so `benchGoferEventWire` mirrors the
> envelope's *shape* for the kinds benchmarked. It is not field-for-field
> identical and does not need to be — but it can drift, so re-check it if the
> wire envelope changes. Measuring the real path is queued for post-3b.

| Payload | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| `message.delta` | 2850 | 1209 | **14** |
| `tool.call.finished` | 11321 | 3824 | **17** |

**Compare a post-3b run against 14 and 17 allocs/op.** A real marshal-once
implementation should land meaningfully below these and above zero.

There is deliberately **no "forward verbatim" comparison row**. Rebinding a
`json.RawMessage` compiles to nothing measurable — `b.Loop` does not protect a
body containing no function call, and the measured result was dominated by loop
overhead (the *larger* payload reported a *smaller* ns/op, which is only possible
if the number is noise). Publishing it beside 2850 ns would also read as a
"~1000×" win no caveat could undo.

### 4. Roster / List latency vs fleet size

| Workers | Roster mean/p50/p90/max (ms) | List mean/p50/p90/max (ms) |
|--------:|---|---|
| 1 | 0.06 / 0.06 / 0.07 / 0.20 | 0.14 / 0.12 / 0.18 / 0.25 |
| 2 | 0.13 / 0.11 / 0.12 / 0.40 | 0.20 / 0.19 / 0.24 / 0.27 |
| 12 | 1.04 / 0.87 / 1.45 / 2.85 | 1.07 / 0.97 / 1.31 / 1.78 |
| 25 | 2.00 / 1.79 / 2.40 / 4.68 | 1.96 / 1.88 / 2.21 / 2.59 |
| 50 | 4.17 / 3.67 / 4.47 / 10.38 | 5.06 / 3.62 / 4.56 / 22.79 |

> ## ⚠️ This table cannot support a scaling claim — in either direction
>
> Do not derive "it scales linearly", "super-linearly", or "sub-linearly" from
> these numbers. **The data is too noisy at this sample size to establish any of
> them.** Two runs on the same commit and machine give:
>
> | Segment | Run 1 | Run 2 |
> |---|---:|---:|
> | 1 → 2 | 1.08 | 0.94 |
> | 2 → 12 | 1.33 | 0.98 |
> | 12 → 25 | 0.92 | 0.93 |
> | 25 → 50 | 1.04 | 1.24 |
>
> (per-segment `dlog(mean)/dlog(workers)`; 1.0 would be linear)
>
> Individual segments move by up to 0.35 between runs and disagree on which side
> of 1.0 they fall. **Any scaling story told from this table is a story about
> noise.**
>
> **Two ways this has already gone wrong**, both recorded so they are not
> repeated:
> 1. An earlier draft quoted the **first-to-last ratio** — mean latency rises
>    ×65.3 from N=1 to N=50 — and called it super-linear growth. That is an
>    artifact of anchoring at N=1, where fixed per-call overhead dominates and
>    then amortizes away; the amortization is the *opposite* of a scaling worry.
> 2. The obvious correction — "so it's linear, or even sub-linear" — is the same
>    mistake with the sign flipped. Run 2 does not reproduce Run 1's segments.
>
> **The authoritative statement about how roster cost scales is the frame count
> in §1** (exactly N, an integer, reproducible), *not* this wall clock. And the
> argument for the roster cache is the structural one above — serial iteration ×
> a 15 s per-worker timeout — which needs no curve at all.
>
> `render()` prints per-segment slopes rather than a first-to-last ratio so the
> ×65-style misreading is harder to make. Reproducing the numbers across runs
> before quoting any of them is still on the reader.

For contrast, that same second run reproduced the **allocations** in §3 *exactly*
(14 and 17 allocs/op) while every wall-clock figure here moved. That is the
clearest demonstration in this document of why the tiers are drawn where they
are: counts reproduce, durations do not.

`List`'s max of 22.79 ms at N=50 is a single outlier against a p90 of 4.56 ms — a
tail figure not worth reading closely.

### 5. Event throughput through the fan-out

| | |
|---|---|
| Sessions driven | 4 concurrently, 5 turns each |
| Subscribers | 8 per session (32 peers total) |
| Delivered | 3104 notifications in 167 ms |
| Throughput | ~18,562 notifications/sec |

### 6. Spawn latency (fork/exec → discovered → dialed → handshaked → adopted)

| | |
|---|---|
| Samples | 50 sequential `Create`s |
| Latency | mean 25.35 ms · p50 **24.90 ms** · p90 26.30 ms · max 42.48 ms |

**M6 §10's "tens of ms" startup estimate holds** — p50 of 24.9 ms, with a tight
p90. Creates are sequential on purpose: a concurrent fan-out would measure
machine parallelism rather than per-session spawn cost.

## After-run — DONE

Completed at `2e3c721`; see [Results: before → after](#results-before--after-slice-3b)
at the top. Outcome in brief:

1. **`gofer/roster` frames per call: N → 0, confirmed** at every fleet size, with
   the cache's one-time warm-up (`2N+1` total) reported separately rather than
   amortized into a per-call figure.
2. **Fan-out allocations: the measured work is gone from the path**, not reduced.
   `BroadcastRawEvent` forwards verbatim with no `json.Marshal`, so the 14/17
   figures describe an operation production no longer performs there.

**Still outstanding — measure allocations on the real path.** The
`BenchmarkEventForward` model is now measuring a path production does not take,
so it can no longer serve as evidence about current behaviour and should be
replaced by an end-to-end measurement through the real bridge, then deleted
rather than kept alongside it. That is tracked separately; until it lands, this
document's allocation numbers describe **removed work only**.
