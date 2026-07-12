# CLAUDE.md

@CONTRIBUTING.md

Guidance for Claude Code when working in this repository.

## What this is

**gofer** — the application half of the gofer agent platform: a daemon +
supervisor + TUI for running many coding agents with roster/peek/attach
navigation and protocol-message approvals. The framework half is
[`jedwards1230/agent-sdk-go`](https://github.com/jedwards1230/agent-sdk-go);
gofer consumes it exclusively through the typed Event/Op contract.

Full product requirements + design: [`docs/PRD.md`](docs/PRD.md). Read it
before structural changes. [`docs/TUI.md`](docs/TUI.md) holds the TUI design
system (component contracts, slash commands, plugin UI);
[`docs/TESTING.md`](docs/TESTING.md) the test strategy.

## Architecture invariants (violations are bugs)

1. **Contract-only consumption**: gofer never reaches into SDK internals. If
   the TUI needs data the contract doesn't carry, the contract gains a typed
   event/op in `agent-sdk-go` first.
2. **Everything is a client**: the TUI is a projection of the same Event/Op
   stream ACP clients and headless exec consume — it gets no privileged path.
3. **SDK promotion test**: code moves to the SDK only when a second app would
   need it unchanged. Supervisor, roster, jobs, auto-mode policy, packaging
   stay here.
4. **Journals are never deleted**: `session.kill` interrupts + terminates,
   `session.archive` drops from the roster — both keep the JSONL journal.

## Commands

```bash
go build ./... && go vet ./... && go test ./...   # the CI gate
golangci-lint run                                  # lint, zero tolerance
go run ./cmd/gofer demo                            # offline faux-provider stream
```

## Layout

- `cmd/gofer/` — CLI entrypoint (`run`, `resume`, `demo`, `login`/`logout`/
  `auth`, `daemon`/`serve` today; `ps`, `kill`, `archive`, `attach` land
  M2+).
- `internal/supervisor/` — session registry over the shared store + runner
  seams; see its package doc for the full contract.
- `internal/daemon/` — ACP-over-WebSocket listener hosting the supervisor
  (`gofer daemon`); see its package doc and `docs/M2-PROOF.md`.
- `internal/tui/` (bubbletea) — the attach/peek/overview frontend.
- Planned: `internal/config/`.
