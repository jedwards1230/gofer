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

## Design discipline

- **Opinions are config.** Before hardcoding a behavior, ask: config default,
  plugin, or genuinely core? A value a user might reasonably change becomes a
  default, never a literal.
- **Visible artifacts over hidden state.** Prefer on-disk, greppable artifacts
  (journals, per-call output files) to in-memory state a client can't inspect.
- **Context-cost discipline.** Keep prompt assembly small and auditable; load
  tool/MCP schemas index-first, full schemas on demand.
- **Code style.** Inline single-call-site helpers; never hardcode a config
  value — add a default; ask before removing intentional code.

## Commands

```bash
go build ./... && go vet ./... && go test ./...   # the CI gate
go vet -tags workerbench ./...                     # also on the PR lane
golangci-lint run                                  # lint, zero tolerance
go test -race ./...                                # PR lane + push/tags
scripts/bench.sh --check                           # allocation-regression gate (PR lane)
go run ./cmd/gofer demo                            # offline faux-provider stream
make install                                       # local install, truthfully version-stamped (never bare `go install`)
```

## Layout

- `cmd/gofer/` — CLI entrypoint: bare `gofer` on an interactive terminal opens
  the roster overview TUI, preferring a reachable daemon's live roster
  (`internal/daemonbridge`) and falling back to the local in-process
  supervisor only when none is reachable; `gofer attach [<session>]` is the
  same TUI but requires a daemon. Piped/non-interactive stdin keeps the M1
  one-prompt behavior. `run`/`resume` (routing through a reachable daemon as
  an ACP client, else the in-process path), `ps`/`kill`/`archive` (always
  daemon-only), `demo`, `login`/`logout`/`auth`, `daemon`/`serve` round out
  the surface.
- `internal/supervisor/` — session registry over the shared store + runner
  seams; see its package doc for the full contract.
- `internal/daemon/` — ACP-over-WebSocket listener hosting the supervisor
  (`gofer daemon`); see its package doc.
- `internal/tui/` (bubbletea) — the attach/peek/overview frontend, plus the
  slash-command dispatcher and command panel (`/status`, `/config`, `/model`,
  `/thinking`, `/usage`, `/stats`, `/resume`, `/help`), plus the
  session-lifecycle commands `/new` and `/quit`, the `/yolo` guardrail toggle,
  and the declarative keymap `/help` renders from.
- `internal/tuibridge/` — adapts the daemon supervisor to the TUI's narrow
  `Supervisor` interface (the single seam importing both).
- `internal/render/` — turns a session's typed event stream into terminal
  output (the `gofer demo`/line renderer); dependency-light and stateless.
- `internal/config/` — gofer's native on-disk config (JSON at
  `<root>/config.json`, written via `config.Save` — indented, mode 0600,
  atomic); sections are the permissions ruleset (M3), `Session`/`TUI` (M4),
  `Telemetry`, `Daemon` (incl. `drain_timeout_ms`), and M7's `Prompt`/
  `Tools`/`MCP`/`Search`/`Skills`/`LSP` sections, plus the shared `SecretRef`
  (`env:`/`file:`, resolved at use time, never inlined). `Prompt` is read by
  `internal/prompt`; `LSP`/`MCP`/`Tools`/`Search`/`Skills` by
  `internal/supervisor`'s session wiring (`internal/websearch` and the SDK's
  `toolindex` consume `Search`/`Tools` respectively; `internal/skillset`
  consumes `Skills`; `internal/mcpconn` consumes `MCP`). See its package doc.
- `internal/prompt/` — composes a session's system prompt from
  `config.Prompt.Files` (the replacement for cmd/gofer's old
  `defaultSystemPrompt` string constant): `builtin:`/absolute/`~/`/cwd-then-
  root file resolution, blank-line-joined composition with first-wins dedup,
  and the shipped `builtin:system.md` default, `go:embed`-compiled in. A leaf
  over `internal/config`; `cmd/gofer`'s run/resume/exec are its only callers,
  and `internal/supervisor.RecordPrompt` journals the result beside a
  session's journal.
- `internal/permrationale/` — the gating rationale behind a permission
  request (matched rule, policy, source, trace) that `ctrl+e` surfaces and
  `session/explain_permission` answers.
- `internal/sandbox/` — OS containment backends (seatbelt / bwrap+seccomp)
  behind the SDK's permission guard.
- `internal/lspdiag/` — wraps the session tool registry (the same seam
  `internal/sandbox.WrapRegistry` uses) so a successful edit/write is
  followed by a bounded, best-effort round trip through a real language
  server (the SDK's `agent-sdk-go/lsp`), appending diagnostics to both the
  tool's model-facing content and its client-facing metadata. Advisory only —
  every failure mode degrades silently to the unmodified tool result.
- `internal/mcpconn/` — the MCP connection manager the SDK's optional
  `agent-sdk-go/mcp` package leaves to the embedder: one process-lifetime
  `Manager` (per session under `--workers`) that connects every configured
  server asynchronously with capped-backoff reconnect, and projects each
  server's tools into the same base `tool.Registry` builtins and `ask_user`
  use. A session's tool set is captured ONCE at create (a bounded wait +
  `Snapshot()`) and never mutated afterward — a late-connecting server joins
  the next session, a dead one keeps its tools registered (degrading to
  `IsError`, working again after reconnect with no re-registration). See its
  package doc.
- `internal/skillset/` — wires the SDK's `agent-sdk-go/skill` package
  (`SKILL.md` discovery with progressive disclosure) into a session:
  resolves `config.Skills` into a `skill.Load` call, applies
  `skills.disabled` (which the SDK's `skill.Set` has no notion of), and
  exposes the single `skill` tool `internal/supervisor.sessionGuard`
  registers into the base tool registry, omitted when nothing survives
  disabling. Also the seam that fixes the project-vs-global skill precedence
  bug (`config.Skills.Directories` lists the project directory first, since
  the SDK's `skill.Load` is first-directory-wins).
- `internal/decision/` — gofer's structured-decision round trip: the
  `ask_user` tool (gofer's first tool of its own) plus the per-session `Gate`
  it blocks on, carrying `acp` decision types over a gofer-native transport
  because the SDK's Event union has no decision kind. A leaf over the SDK.
- `internal/websearch/` — the `web_search` tool (M7 workstream 4): projects
  the SDK's `search` package (`Provider`, the Brave/SearXNG backends, the
  name-keyed registry) into a model-facing tool, registered only when
  `config.Search.Selected()` is not none. Blank-imports both backends
  unconditionally; resolves its `config.SecretRef` credential at Run time,
  never at construction. Wired into a session's registry (alongside
  `tools.schema_mode`'s preload/index toggle, via the SDK's `toolindex`) at
  `internal/supervisor`'s `sessionGuard` — see its package doc.
- `internal/telemetry/` — OpenTelemetry (traces/metrics/log-correlation) off
  the Event/Op stream; the only otel importer.
- `internal/router/` — the M6 thin router daemon: roster aggregation, client
  fan-out, discovery, and the ACP surface, spawning one worker per session.
  See its package doc.
- `internal/worker/` — the per-session `gofer session-worker`: owns the
  runner, pump, gate, journal, and broker for exactly one session. It builds
  an `internal/daemon` with `MaxSessions: 1` — a worker IS a single-session
  daemon.
- `internal/wirestream/` — reconstructs a remote session's typed event stream
  from lossless `gofer/event` envelopes; `Subscribe`/`SubscribeLive` plus the
  `WithEventSink` push seam the router fans out through.
- `internal/daemonbridge/` — adapts a reachable daemon to the supervisor
  interface the TUI and `run`/`resume` consume, so a client path is identical
  local or remote.
- `internal/modelcatalog/` — answers "which models can THIS credential reach,
  and what should gofer default to?". Needed because OpenAI routes by
  credential *kind* (an API key and a ChatGPT-OAuth token serve different
  model families), which the SDK's provider-id-only view gets wrong. A static
  floor plus a live discovery layer above it, with a fallback rule when the
  live listing is unavailable. A leaf over the SDK.
- `internal/modelmeta/` — display naming for model ids (`DisplayName`, e.g.
  `claude-sonnet-5` → `Sonnet 5`).
- `internal/usercmd/` — user-authored markdown slash commands: discovery under
  `<root>/commands` + `<cwd>/.gofer/commands`, the two-key frontmatter reader,
  and the `$1`/`$ARGUMENTS`/`${1:-def}`/`${@:N}` substitution engine. A leaf
  (stdlib only) so the rules are table-tested without a terminal; the TUI only
  adapts a loaded command into a dispatcher entry.
- `internal/versionskew/` — classifies how a daemon's build relates to the
  client's (older / different / silent), the one comparison the CLI's stderr
  version-skew warning (`cmd/gofer`) and the roster's stale-daemon banner
  (`internal/tui`) share so the two surfaces never disagree. A leaf over
  `x/mod/semver`.
