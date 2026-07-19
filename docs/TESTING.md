# gofer — testing strategy

The SDK owns loop/provider/session/permission testing (see agent-sdk-go's
`docs/TESTING.md`). gofer's layers:

| Layer | Type | CI | Approach |
|---|---|---|---|
| TUI | unit + golden | every push | two tiers: (1) `Update(msg)` → assert model state (the majority); (2) `x/exp/golden` vs `testdata/*.golden` for the few render-critical components, lipgloss direct — **no PTY**, fully deterministic. The `testkit` harness pins fixed sizes, forces `termenv.Ascii`, and uses `theme.Test()` |
| daemon · ws/ACP | integ | every push | real in-process daemon over a WebSocket / JSON-RPC 2.0 (ACP) transport; a real ws client drains `session/update` notifications and asserts a liveness window (no-event-for-N-ms = still open) |
| binary e2e | e2e | gated | build the real binary, spawn N clients against a temp socket (race regression). Skipped under `-short` and on Windows; runs on the full (push) lane |

## Rules

- Golden-file tests come **first** for any new render-critical component —
  before styling work, never after.
- Script turns in code (typed builders); JSONL fixtures only for captured
  session histories.
- Never test through a PTY; teatest is not a first move.
- An edit-committing view (e.g. `/config`) is tested by supplying a fake
  writer closure (`CommandEnv.SaveConfig`) that captures what was written,
  asserted alongside the golden render — never a real file on disk.

## CI

Fast PR lane (unit + golden); `go test -race` runs on push to main and
release tags (`.github/workflows/ci.yml`). The e2e socket test runs on the
push lane now that the M2 daemon has landed.

**Visual capture (advisory).** A separate lane
(`.github/workflows/vhs-capture.yml`) fires on PRs touching `internal/tui/**`,
`vhs/**`, or `scripts/tui-vhs.sh`: it renders every `vhs/*.tape` and embeds the
frames inline in the job summary and a sticky PR comment, so TUI changes can be
eyeballed without pulling the branch. Frames are published to the orphan
`vhs-captures` branch under `pr-<n>/<sha>/`. This is **not** a required check —
it complements, never gates. Fork PRs get a read-only token and degrade to a
`vhs-frames` artifact upload instead of a push+comment.

## Worker-fleet benchmark (M6, off by default)

`internal/router/bench_test.go` spawns ~50 real detached worker processes and
reports measurements — RSS, `Roster`/`List` cost, event throughput, spawn
latency. It is a measurement harness, not a correctness test: it asserts almost
nothing, and a failure means a measurement could not be taken (or a process
leaked), not that a number moved.

It is gated behind the **`workerbench` build tag**, not `testing.Short()`. CI
runs bare `go test ./...` and `go test -race ./...` with no `-short` flag, and
`testing.Short()` is false by default — so a Short-gated benchmark would run on
every push and spawn 50 processes inside the runner. The build tag is the only
gate that actually excludes it, and it matches the repo's existing
conditional-compilation idiom.

```bash
# fleet measurements + roster wire-frame count
GOFER_BENCH_LOAD_NOTE="<what else was running>" \
GOFER_BENCH_OUT=fleet.txt GOFER_BENCH_FRAMES_OUT=frames.txt \
  go test -tags workerbench -run 'TestWorkerFleetBenchmark|TestRosterWireFrameCount' \
  -v -timeout 30m ./internal/router/

# allocations on the event fan-out path
go test -tags workerbench -run '^$' -bench BenchmarkEventForward -benchmem ./internal/router/
```

Fleet size and load are env-tunable (`GOFER_BENCH_WORKERS`,
`GOFER_BENCH_CALL_ITERS`, `GOFER_BENCH_FANOUT_*`) so a smaller machine can
produce a comparable, lower-N run rather than failing. **A run is only
comparable to another run with the same settings on the same machine** — always
record them.

Results are only meaningful against a stamped commit. See
[`docs/benchmarks/m6-worker-fleet-baseline.md`](benchmarks/m6-worker-fleet-baseline.md)
for the pre-Slice-3b baseline, and for which metrics are authoritative versus
merely indicative — wall-clock numbers move with machine load, and one benchmark
models production code rather than calling it.

## M3 exit gate — satisfied

M3's close required a **live multi-client pass**: two clients on one session
(one of them a phone) exercising fan-out + approvals — met at milestone close
(#53). Automated PR review caught zero of M2's cross-connection/ordering bugs;
live client testing caught all of them, so the golden/integration matrix could
not stand in for it here.
