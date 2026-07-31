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
```

`go test -race ./...` runs on every PR and on push to `main` / release tags.
Still run it locally before anything touching concurrency so you catch a race
before CI does — but a data race now blocks the PR, not just the release.

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
