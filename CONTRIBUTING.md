# Contributing to gofer

Daemon + TUI for supervising coding agents, built on
[`agent-sdk-go`](https://github.com/jedwards1230/agent-sdk-go). All changes go
through the workflow below.

## Prerequisites

Go ≥ 1.25 and `golangci-lint`.

`gopls` is optional but recommended: the LSP live tests
(`internal/lspdiag`, `internal/supervisor`) drive a real language server and
**skip themselves** when none is on `PATH`. Without it they pass silently
without proving anything, so install it before touching LSP wiring —
`go install golang.org/x/tools/gopls@v0.21.1` (the version CI pins). CI's
`lsp-live` job installs it and fails if those tests skip.

## Build, test & lint

```bash
go build ./...
go vet ./...
go vet -tags workerbench ./...
go test ./...
golangci-lint run
scripts/bench.sh --check
```

`go test -race ./...` runs on every PR and on push to `main` / release tags.
Still run it locally before anything touching concurrency so you catch a race
before CI does — but a data race now blocks the PR, not just the release.

[`docs/TESTING.md`](docs/TESTING.md) is the full map of what gofer verifies and
what each layer is blind to. Read it before adding a test of a kind you haven't
written here before — picking the wrong layer is how you end up with a test that
looks thorough and proves nothing.

## Performance is a gate, not a follow-up

`scripts/bench.sh --check` runs on every PR and **fails on allocation
regressions** against the committed `bench/baseline.txt`.

This exists because every other check in this repo passes just as happily
against an accidentally quadratic implementation. Both of the performance bugs
found so far — the roster re-reading every journal on every one-second tick
(gofer#298) and `Model.Ingest` deep-copying the whole transcript per event
(gofer#308, 1.6s and ~9 GB to open a 5,000-turn session) — were invisible to the
entire correctness suite and surfaced only when a user said it felt slow.

- **Add a benchmark when your change touches anything whose cost grows** with
  session count, transcript length, or attached client count.
- **Sweep the axis; never report one number.** A single point cannot distinguish
  linear from quadratic, and the question is always "what happens as this grows".
  Where two axes exist, sweep them separately — a fix that flattens one and not
  the other looks complete against a combined benchmark.
- **The gate is allocations only** (`allocs/op`, `B/op`), never wall-clock:
  ns/op on a shared runner is too noisy for a threshold that both catches
  regressions and avoids false alarms, and a gate that cries wolf gets ignored.
  Concurrent benchmarks are marked `allocs-only` in the baseline and skip the
  `B/op` half — at one iteration their byte total is decided by goroutine
  scheduling and swings ~200% with no code change.
- **A legitimate cost increase means updating the baseline** —
  `scripts/bench.sh --update` — and saying in the PR why the extra work is worth
  it. The baseline is committed precisely so that argument happens in review
  rather than silently.
- Narrow it while iterating: `BENCH_PKGS=./internal/tui/ scripts/bench.sh --check`.

## A green test is not evidence until you have seen it go red

Before treating a new test as proof, **mutation-test it**: break the thing under
test and confirm the check fails. A mutation only counts if the mutated tree
still **builds** and the **named** test fails — a compile error proves nothing
about your assertion, and a mutation that matched nothing leaves the suite
passing vacuously.

Two traps this repo has actually hit: an assertion whose *shape* doesn't match
the property (a timeout is a ceiling, not a floor, so "assert it takes ≥3s" tests
nothing against a fast-failing dependency), and a harness that cannot express the
failure (`tea.Batch` arrives at `App.Update` as a `tea.BatchMsg` it has no case
for, so the one-Cmd test helpers swallow it and the test goes quietly vacuous
rather than red).

`docs/TESTING.md` has the longer list.

## Local install

Use `make install` (not a bare `go install ./cmd/gofer`) to install gofer
locally. It stamps the binary with `git describe` via ldflags so it reports its
true HEAD; a bare `go install` from a linked git worktree mis-stamps the
version from the *primary* worktree's commit, which silently defeats the
stale-daemon version-skew banner. `make build` does the same into `bin/gofer`.

## Hard rules

- **gofer consumes the SDK only through the typed Event/Op contract.** If a
  feature needs to reach past it, the contract is missing something — fix the
  contract in `agent-sdk-go` first.
- **SDK promotion test**: code moves down into the SDK only when a second
  application would need it unchanged. Supervision, roster, and TUI stay here.

## The environment is not a safe channel for a secret

**In gofer the model has a shell, so treat any process environment an agent's
tools can reach as readable by the model.** argv and the environment are *both*
unsafe channels for a secret. The safe hand-off is a `0600` file the recipient
reads once and deletes.

The tempting reasoning is "argv is world-readable via `ps`, so pass it in the
environment instead". That is correct about argv and wrong about the conclusion:
it defends against other local *users* while leaving the secret open to the
*agent*, which is the principal this codebase exists to run. Two mechanisms make
that concrete, and both are worth re-checking rather than taking on faith:

- The SDK's bash tool execs with `cmd.Env` left nil
  (`agent-sdk-go`'s `tool/bash.go` — the SDK module, not a path in this repo),
  and Go gives a child process with a nil `Env` its parent's environment.
  Anything in a gofer process's environment is therefore one `env` away from the
  model.
- `internal/sandbox` does not change that. The bwrap profile
  (`internal/sandbox/profile_bwrap.go`) binds the filesystem and unshares the
  network (`--ro-bind`, `--bind`, `--unshare-net`); neither backend passes
  `--clearenv`/`--unsetenv` or filters the environment at all. Containment
  covers the filesystem and the network, not the environment.

So when a gofer process must hand a credential to another process it spawns:

1. Write the secret to a `0600` file.
2. Pass the **path**, never the value — not in argv, not in the environment.
3. Have the recipient read it once and **delete** it.
4. Leave `cmd.Env` nil so the child inherits the parent's environment unchanged
   and gains nothing from the hand-off.
5. Gate the whole hand-off on the feature that needs it, so a deployment that
   never opted in carries no credential at all.
6. Sweep a leftover file with the recipient's other runtime artifacts, for the
   case where it dies before reading.

The same property bounds `config.SecretRef`: its `env:VAR` form resolves against
gofer's **own** environment at use time (`os.LookupEnv`), so any credential
supplied that way is readable by the model through the bash tool. That is not
one key — it holds for every `SecretRef` consumer, including each MCP server's
`env` and `headers` (`internal/mcpconn`'s `resolveEnv`/`resolveHeaders`), so an
`Authorization` bearer for an HTTP MCP server configured as `env:` is
model-readable on the same terms.

That is a reasonable trade for an operator-scoped, per-service credential and
the wrong one as the blast radius grows. `SecretRef` already supports
`file:/path` — prefer it whenever disclosure would cost more than the session.
Whether gofer should narrow the `env:` form further is open, and tracked in
gofer#354.

## Before you open a PR

- Make sure all CI checks pass locally first (the commands above, exactly as CI
  runs them).

## Branching & commits

- Branch off `main`; never commit directly to `main`.
- Use [Conventional Commits](https://www.conventionalcommits.org/) prefixes
  (`feat:`, `fix:`, `docs:`, `chore:`, `refactor:`, `test:`, …).
- Sign your commits where possible (`git commit -S`).
- Keep each PR focused; delete dead code rather than commenting it out.

## Known false positives from automated review

The automated reviewer raises these repeatedly. They have each been refuted with
evidence more than once. **Do not "fix" them** — changing correct code to quiet a
bot makes the code worse and the next reviewer will raise the same objection
about the workaround.

- **Loop-variable capture in goroutines.** Reports of the form "the closure
  captures `i`/`v` by reference, so all goroutines see the last value" are
  **wrong for this repo** *when the variable is the loop variable itself*. Go
  made loop variables **per-iteration in 1.22**, and `go.mod` declares
  `go 1.25.0`. Each iteration gets its own variable, so
  `for i, v := range xs { go func() { use(i, v) }() }` is correct as written and
  the pre-1.22 `i := i` shadow is redundant. (The codebase also uses
  range-over-int, which is itself 1.22+, so the language version is not in
  doubt.) Reply with the `go.mod` line and resolve.

  **This is not a blanket dismissal.** Go 1.22 changed the *loop variable*, and
  nothing else. A goroutine closing over a variable declared **outside** the
  loop, or over one the loop body reassigns, is still a genuine data race and
  the report may well be right. Check *which* variable is captured before
  reaching for this entry.

- **"Check `ctx.Done()` between loop iterations."** Reports that a paginating
  or retrying loop can outlive a cancelled context are wrong whenever every
  iteration already makes a context-honoring call. [`daemon.Client.Call`]
  selects on `ctx.Done()` and returns `ctx.Err()`, so the next iteration's
  request aborts immediately and the loop returns the error — an extra
  `if err := ctx.Err(); err != nil` between iterations adds a second, earlier
  place for the same outcome and nothing else. Reply with the `Call` select and
  a cancellation test, and resolve. (Genuinely unbounded work *between* calls —
  a long pure computation, a `time.Sleep` — is a different case and the report
  may be right.)

- **"`case` path patterns only match one directory level."** Reports that
  `case "$f" in internal/tui/*)` won't match `internal/tui/theme/foo.go` are
  wrong. `case` uses **pattern matching**, not pathname expansion: the rule
  that `*` stops at `/` belongs to globbing a filesystem, and POSIX applies it
  only there (`XCU 2.13.3`). In `case`, `*` matches any string including
  slashes, so `internal/tui/*` matches arbitrarily deep paths. The
  `.github/workflows/vhs-capture.yml` path matcher relies on this. Verify by
  running the matcher over nested paths in both `sh` and `bash` before
  changing it — adding `**` or a second `internal/tui/*/*` arm is noise, and
  `**` is not even special in `case` (it is just two `*`s). Reply with the run
  and resolve. (A pattern that genuinely needs to *stop* at a directory
  boundary is a different case — but then the fix is an explicit character
  class, not `**`.)

- **"Add a non-unix stub so `cmd/gofer` cross-compiles."** Reports that a new
  reference to a `//go:build unix` helper in `internal/daemon` (`ProcessAlive`,
  `LockWorker`, `SpawnDetached`, `Reap`) breaks the Windows/plan9 build, usually
  citing `cmd/gofer/service_other.go` as the pattern to follow, are **wrong about
  what that pattern is for**. `service_other.go` is `//go:build !darwin && !linux`
  — it keeps the **BSDs** green, and those are `unix`, so the `internal/daemon`
  unix files apply there normally. It has never been about non-unix.

  `cmd/gofer` does not build on Windows today and did not before any such PR: it
  depends transitively on `internal/router` and `internal/worker`, which already
  call four unix-only `internal/daemon` helpers. Adding `process_other.go` alone
  removes two of the five errors and leaves three. plan9 additionally fails
  inside `bubbletea/v2` and `grpc`, which no change here can fix. The release
  matrix (`.github/workflows/release.yml`) targets only linux and darwin.

  Verify before believing either side — the "before" must actually fail
  differently from the "after":

  ```bash
  GOOS=windows GOARCH=amd64 go build ./cmd/gofer/   # fails, on base and on HEAD alike
  GOOS=freebsd GOARCH=amd64 go build ./cmd/gofer/   # passes — this is what the stubs protect
  ```

  Making non-unix build is a real (unfiled) project spanning `internal/daemon`,
  `internal/router`, and `internal/worker` — not a one-file stub, and not the job
  of a PR that merely moves a call site. Reply with the two commands and resolve.

If you refute one of these, add it here rather than only in the PR thread — the
bot has no memory across pull requests, but this file does.

## Pull requests

- Open the PR against `main`.
- Every PR runs CI. Resolve **all** review threads before the PR is merged.
- An automated code review runs on each PR; address and resolve its threads
  like any other review.

## Capture new TUI states as VHS tapes, as you go

**A PR that adds or changes a visible TUI state should add or update a
`vhs/*.tape` in the same PR.** Not as a follow-up, and not "once it settles" —
a state nobody captured is a state nobody can review, and the tape is cheapest
to write while the state is fresh in your head.

The reason is a gap, not a ritual. The Ascii golden tests are authoritative for
*text*, but they render `termenv.Ascii`: they cannot see colour, and by
construction they miss ANSI-width bugs (the #61 colour-scatter regression
shipped past green goldens). Anything whose meaning lives in colour, styling, or
motion — a highlight that shifts to signal a pending confirm, a muted
in-progress indicator, a marker that changes hue — is invisible to them. That is
precisely what a tape is for.

Practical notes, learned the hard way:

- **Capture a state change as a before/after PAIR.** One frame of the new state
  shows a colour; it cannot show a *change*. The pair is also what makes the
  tape sensitive to your feature at all — if both frames would look identical
  without it, the tape is decoration. See `roster-delete-confirm.tape`.
- **Make the frame deterministic.** `vhs-baseline.yml` re-renders every tape on
  each push to `main` and commits the key-frames, so anything time-varying
  churns the baseline forever (gofer#297). If the state shows a live counter,
  capture it mid-bucket rather than on a boundary — `transcript-compacting.tape`
  works this way, and it is why the elapsed counter truncates instead of
  rounding.
- **A state that only exists mid-call needs the harness to hold that call.**
  Against a canned Supervisor that returns instantly, an in-flight indicator
  appears and vanishes inside one frame with nothing to photograph. See
  `vhsSupervisor.compactHold`.
- **Mock enough session history that the frame shows a real situation.** A
  widget captured over an empty transcript is technically correct and still
  misleading — it shows the thing while misrepresenting what produces it.
  `vhsSupervisor.seed` publishes a scripted conversation onto the broker's
  retained backlog, so it is on screen the moment the tape attaches; seed it
  that way rather than publishing on a timer, or the frame starts depending on
  whether the publish beat the subscribe.
- **Reuse an existing scenario when only the driving keys differ** — most tapes
  need no new harness scenario at all (`roster-peek`, `roster-delete-confirm`).
- **Look at the rendered frame before committing it.** A tape that runs
  successfully can still produce a blank, stale, or wrong-screen capture, and it
  will do so silently.

Tapes are advisory — never a merge gate. `vhs/README.md` has the full tape
inventory and `scripts/tui-vhs.sh` the render workflow.

## Documentation

Keep documentation current as part of the change, not as a follow-up — update
the README and `docs/` in the same PR.

## Releases

Releases are **opt-in per PR, via a label** — merging without one ships
nothing, silently. Before merging, label the PR with exactly one of:

| Label | Bump |
|---|---|
| `semver:major` | major |
| `semver:minor` | minor |
| `semver:patch` | patch |

On push to `main`, `.github/workflows/release.yml` reads the merged PR's
labels. With no `semver:*` label it logs *"No semver:\* label — skipping
release"* and stops; that is the intended default for docs/chore merges, so
omit the label deliberately rather than by accident. If several are present the
highest wins (major > minor > patch).

The git tag is the single source of truth for the version — `build` stamps it
into the binary via ldflags, so a release always reports its own tag. Version
and AI release notes come from the shared
`jedwards1230/release-workflows/ai-release.yml`.

To release without merging anything (or to preview), use the workflow's
`workflow_dispatch` entry: pick a `bump_type`, and set `dry_run` to compute the
version and notes without tagging or publishing.
