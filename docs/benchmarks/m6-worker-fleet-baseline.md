# M6 worker-fleet baseline (pre-Slice-3b)

Measured baseline for the M6 per-session worker fleet, captured **before** the
marshal-once event bridge and push-based roster cache landed. It exists so those
two optimizations are shown with numbers rather than claimed.

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
round-trips every second**, just to keep the roster painted — and the latency
table shows it degrading *super-linearly* (×65.3 cost across ×50 workers),
because per-call overhead compounds at the tail.

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

> **This metric is indicative, not authoritative, for two independent reasons.**
> It is suggestive of the marshal-once win; it does not prove it.
>
> 1. **It models the production decoder rather than calling it.** gofer's real
>    decoder is unexported in `internal/wirestream`, which this harness may not
>    modify, so `benchGoferEventWire` **replicates `goferEventWire`'s field set**
>    locally. If that wire type gains or changes fields, these numbers silently
>    drift from what production does. Anyone changing the event envelope should
>    assume this benchmark is stale until re-checked.
> 2. **`forward_verbatim` is a near-no-op by construction.** It rebinds a
>    `json.RawMessage`, so its 0 allocs and ~2.9 ns are a theoretical *floor*,
>    not a measurement of real forwarding work.

| Payload | Shape | ns/op | B/op | allocs/op |
|---|---|---:|---:|---:|
| `message.delta` | decode + re-encode (models today) | 2850 | 1209 | **14** |
| `message.delta` | forward verbatim (floor) | 2.9 | 0 | 0 |
| `tool.call.finished` | decode + re-encode (models today) | 11321 | 3824 | **17** |
| `tool.call.finished` | forward verbatim (floor) | 2.9 | 0 | 0 |

**Compare a post-3b run against 14 and 17 allocs/op — not against the floor.** A
real marshal-once implementation should land meaningfully below those figures but
above zero. Treating the floor as the target would overstate the win.

### 4. Roster / List latency vs fleet size

| Workers | Roster mean/p50/p90/max (ms) | List mean/p50/p90/max (ms) |
|--------:|---|---|
| 1 | 0.06 / 0.06 / 0.07 / 0.20 | 0.14 / 0.12 / 0.18 / 0.25 |
| 2 | 0.13 / 0.11 / 0.12 / 0.40 | 0.20 / 0.19 / 0.24 / 0.27 |
| 12 | 1.04 / 0.87 / 1.45 / 2.85 | 1.07 / 0.97 / 1.31 / 1.78 |
| 25 | 2.00 / 1.79 / 2.40 / 4.68 | 1.96 / 1.88 / 2.21 / 2.59 |
| 50 | 4.17 / 3.67 / 4.47 / 10.38 | 5.06 / 3.62 / 4.56 / 22.79 |

Roster mean grows **×65.3 across a ×50 increase in workers** — cost scales with
fleet size, slightly super-linearly at the tail. That slope is the wall-clock
shadow of the frame count above; the frame count is the authoritative version of
the same claim. `List`'s max of 22.79 ms at N=50 is a single outlier against a
p90 of 4.56 ms — the kind of tail figure not worth reading closely.

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

## After-run

Re-run at the post-3b tip under the same fleet size and machine, and record both
sets side by side.

1. **`gofer/roster` frames per call: N → 0.** Authoritative; this is the claim.
2. **Fan-out allocations: below 14 / 17 allocs/op** (not down to the floor).
   Indicative only, for the reasons above.

**Planned replacement for §3.** Once the raw-forward path exists in production,
allocations should be measured on the **actual fan-out path** — an end-to-end
test driving real events through the real bridge — rather than on a model. That
becomes the authoritative marshal-once evidence, and it is strictly better than
anything obtainable pre-3b, because before 3b there is no real raw-forward code
to compare against.
