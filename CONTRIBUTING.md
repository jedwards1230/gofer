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

## Pull requests

- Open the PR against `main`.
- Every PR runs CI. Resolve **all** review threads before the PR is merged.
- An automated code review runs on each PR; address and resolve its threads
  like any other review.

## Documentation

Keep documentation current as part of the change, not as a follow-up — update
the README and `docs/` in the same PR.
