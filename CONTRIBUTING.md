# Contributing to gofer

Daemon + TUI for supervising coding agents, built on
[`agent-sdk-go`](https://github.com/jedwards1230/agent-sdk-go). All changes go
through the workflow below.

## Prerequisites

Go ≥ 1.25 and `golangci-lint`.

## Build, test & lint

```bash
go build ./...
go vet ./...
go test ./...
golangci-lint run
```

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

If you refute one of these, add it here rather than only in the PR thread — the
bot has no memory across pull requests, but this file does.

## Pull requests

- Open the PR against `main`.
- Every PR runs CI. Resolve **all** review threads before the PR is merged.
- An automated code review runs on each PR; address and resolve its threads
  like any other review.

## Documentation

Keep documentation current as part of the change, not as a follow-up — update
the README and `docs/` in the same PR.
