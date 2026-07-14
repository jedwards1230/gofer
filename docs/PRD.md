# gofer — PRD & design

> Application half of the **gofer** platform. The framework half is
> [`jedwards1230/agent-sdk-go`](https://github.com/jedwards1230/agent-sdk-go),
> whose `docs/PRD.md` owns the contract, tenets, and SDK package design. This
> document covers what the app adds on top.
> Companions: [`TUI.md`](TUI.md) (TUI design system, slash commands,
> plugin-contributed UI) and [`TESTING.md`](TESTING.md) (test strategy).

## Problem

Nothing self-hosted combines an owned, auditable agent loop with
Claude-Code-grade supervision UX: a roster of running agents, peek/attach
navigation, and approvals that reach your phone. gofer is that product, for an
operator running N agents across their own machines.

## Personas

- **The operator** (primary): runs N agents, supervises from one TUI, approves
  from a phone, trusts what's in context because the loop is theirs.
- **The ACP user**: drives sessions from an ACP client (editor, phone app)
  pointed at the daemon — with zero bespoke glue.

## What gofer owns (vs the SDK)

```
supervisor · roster · jobs          TUI (overview ⇄ peek ⇄ attach)
auto-mode policy wiring             config · packaging · deploy
```

Membership test: code moves down into the SDK only when a second application
would need it unchanged.

## Core UX

- **Overview**: every session — state, model, cost, elapsed, pending
  approvals. `enter` peek, `a` attach, `ctrl-x` kill (running, confirm;
  subtree interrupted) or archive (finished), `n` new.
- **Peek**: read-only live stream of one session without stealing input.
- **Attach**: full interactive session; detach returns to overview.
- **Approvals**: `permission.requested` renders wherever the client is — TUI
  dialog, phone push via ACP — and `permission.reply` may carry a
  remember-rule.
- **Cost everywhere**: per-session and per-model $ / tokens in roster rows and
  `/session`, from the SDK's usage accounting (P0, lands M1).

## Constraints & tenets

- **Daemon ACP surface stays spec-general.** Clients are many — phone app,
  editor, web later — so the daemon speaks generic ACP with no client-specific
  behavior, ever. A client identifies itself for display, never to unlock a
  code path.
- **Approvals are the thesis; containment complements them.** Supervision —
  approvals reaching your phone — is the product; the sandbox reduces how often
  a human is asked, it never replaces the human gate.
- **Context-cost discipline.** Prompt assembly stays small and auditable; tool
  and MCP schemas load index-first (name + one-liner), full schemas on demand —
  never the whole registry up front.
- **Open decision — session visibility (decide during M3 fan-out).** Does
  `session/list` return cwd-scoped sessions or the fleet-global set with a cwd
  label? It gates cross-client sync, so it lands with the fan-out registry.

## CLI surface

```
gofer                       # TUI: health-probe daemon → auto-spawn if absent → overview
gofer attach [<session>]    # daemon roster TUI; with <session>, attach straight into it
gofer agents [<session>]    # alias for `gofer attach` (M2)
gofer demo                  # M0: offline faux-provider stream
gofer exec [-p prompt] [--agent name] [--json] [--output-schema file] [-m model] [--root dir]
                            # headless one-shot, in-process (not daemon-routed): JSONL events
                            #   on stdout (M3)
gofer serve [--host unix://…|tcp://…]   # run the daemon in the foreground
gofer daemon install|uninstall|status   # launchd/systemd unit for the daemon (M3)
                            #   install [--listen addr] [--root dir] [--token tok]
gofer acp serve             # ACP over stdio (editors, stdio→ws bridges)
gofer ps [--all]            # roster (--all includes archived; later: fleet)
gofer kill|archive <id>     # stop running / clear finished (journal kept)
gofer skills|plugins        # list what's composed; `plugins install <module>` (M5)
gofer import claude         # idempotent import of CC skills/commands (M5)
                            #   (settings.json permissions via the vendor-format adapter, M4/M5)
gofer doctor                # providers, LSP servers on PATH, daemon, sandbox
gofer config get|set …      # global or project config
```

**Daemon discovery** (`ps`/`kill`/`archive`/`attach`/`agents`, and
`run`/`resume`/bare `gofer` when one is reachable): the daemon address and
bearer token are resolved in precedence order — an explicit `--daemon`/
`--token` flag, then `$GOFER_DAEMON`/`$GOFER_TOKEN`, then the endpoint a
running `gofer daemon` advertised at `<root>/daemon.json` (mode 0600,
written on startup and removed on clean shutdown), then the loopback
default `127.0.0.1:7333`. So on the same host, once a daemon is up, clients
need no flags at all — this closes the M2 gap where a daemon bound to a
non-loopback address (e.g. a tailnet IP) required every client invocation to
pass `--daemon`/`--token` by hand. `run`/`resume` read the endpoint file at
their own `--root` (defaulting to `~/.gofer`), so a daemon and a client given
the SAME `--root` discover each other automatically; `ps`/`kill`/`archive`/
`attach`/`agents`/bare `gofer` take no `--root` of their own and always use
the default `~/.gofer` — a daemon started with a different `--root` needs an
explicit `--daemon`/`$GOFER_DAEMON` on those clients.

Daemon-as-a-service: `gofer daemon install` writes a launchd user agent
(macOS) or systemd `--user` unit (Linux) so the daemon starts on login;
`uninstall`/`status` manage it. The unit defaults to the loopback bind
(`127.0.0.1:7333`, no token); a non-loopback `--listen` requires a token,
delivered out of band through a 0600 `<root>/daemon.env` file — never
templated into the (world-readable) unit or the daemon's argv. On a fresh,
fully interactive `gofer` (stdin+stdout TTYs, not CI, no service installed and
no daemon reachable) a one-line first-use prompt offers to install it; it is a
complete no-op in every other case.

Daemon lifecycle: the client auto-spawns the daemon on launch (health probe →
detached spawn); a version/build mismatch triggers graceful shutdown →
respawn, so upgrades "just work". Prompts sent to a busy session **queue**
(accept/dispatch state machine, inspectable and clearable by any client) —
real steering, not reject-if-busy. Worktree isolation per session is opt-in.

## Session lifecycle

Event-sourced JSONL journals (SDK). `kill` = interrupt + terminate a running
session; `archive` = drop a finished one from the roster. **Journals are never
deleted** — both emit must-deliver events (`session.killed` / `.archived`).

## Auto mode pipeline (M3 → M5)

Contain before you classify · fail closed · no local SLM.

```
tool call
  ① static rails ── deny rule ─▶ ✗ blocked (reason fed back to model)
  │                 allow rule ─▶ ✓ run
  ▼ no match
  ② sandbox ────── sandboxable ─▶ ✓ run contained (seatbelt / bwrap+seccomp)
  ▼ not sandboxable                (before ③ exists: escalate to ✋ human)
  ③ LLM reviewer   out-of-band call · strict JSON {decision, risk, rationale}
  │                30s timeout · 360-tok cap · fail-closed · injection-framed
  ├─ low-risk ∧ high-confidence ─▶ ✓ run (audit-logged)
  └─ anything else ──────────────▶ ✋ human (TUI · ACP · push)
```

Entering auto mode drops broad grants — `Bash(*)` can never bypass ③. Stages
①+② ship before ③ exists; each is independently useful. **①+② + the human
fallback shipped in M3** (`internal/sandbox` + the `RuleGuard`/`Gate` relay): an
allow-matched call runs contained when the host can contain it, and a call the
host cannot contain (no sandbox runtime, or a non-containable tool) escalates to
a human approval that reaches every attached client — never silently blocked,
never run uncontained (decided 2026-07-13). The ③ LLM reviewer is M4/M5. The reviewer is one
more SDK loop invocation with a different system prompt. Stage ① is a
format-agnostic rule engine over typed rules; vendor rule formats (Claude Code
`settings.json`, native YAML) are import adapters that land with the
vendor-format work (M4/M5).

## On-disk layout & config precedence

```
~/.gofer/                          project: <repo>/.gofer/
  config.yaml   global defaults      config.yaml   project overrides
  agents/*.yaml manifests            agents/*.yaml
  skills/       SKILL.md dirs        skills/
  grants.json   TTL'd grants         commands/     user slash commands
  sessions/<proj-slug>/<uuid>.jsonl  AGENTS.md     project context
  logs/         · daemon socket in $XDG_RUNTIME_DIR/gofer-<uid>.sock

precedence: flags > env > project .gofer/ > ~/.gofer/ > embedded defaults
(permissions: deny wins from any layer)
```

## Observability

gofer owns telemetry; the SDK stays dependency-light and exposes the seams
(context propagation, optional `*slog.Logger` injection, the Event/Op stream as
the instrumentation source — see agent-sdk-go `DESIGN.md`). All exporters point
at **generic OTLP endpoints**, configurable and **off by default** — no
phone-home.

- **M2 (now): leveled structured logging** via `log/slog` (stderr text handler),
  `--log-level debug|info|warn|error` (default `info`, env `GOFER_LOG_LEVEL`).
  Covers connection lifecycle, every inbound request (method, id, outcome,
  duration), session lifecycle (created/resumed/killed/archived), and unknown
  methods at WARN (the smoking gun for client-compat work). **Hard redaction
  rule**: never logs params, prompt text, message content, tool inputs/outputs,
  or the bearer token — identifiers, codes, and durations only.
- **M3 ✅ shipped: full OpenTelemetry**, entirely in a new `internal/telemetry/`
  package — the SDK still takes no otel dependency.
  - **Traces**: a span per turn, with a child span per provider-call proxy and
    per tool execution. The SDK's typed Event/Op stream is the span source —
    `*.started`/`*.finished` events open and close spans without the SDK
    knowing tracing exists.
  - **Metrics**: sessions (live), turns, tokens and cost, error rates.
  - **OTLP export**: traces + metrics to a generic OTLP endpoint, off by
    default (`telemetry.Config{}`) — no exporter, no network, no global otel
    state touched until a deployment opts in.
  - **Log correlation**: trace and span ids stamped into slog records so logs
    and traces join, for log calls whose ctx carries an active span.
  - **Two flagged gaps**, not worked around: (1) turn/tool events carry no
    turn id — span correlation relies on the supervisor's serial per-session
    pump (one turn in flight at a time), not an explicit identifier; (2)
    there is no dedicated provider-call event — the `message.*` pair is the
    closest proxy, and per-provider-call token usage isn't available
    (`provider.Usage` is a turn-aggregate on `turn.finished` only).

## Milestones

| Stage | Ships | Proof |
|---|---|---|
| **M0 · scaffold** ✅ shipped 2026-07-12 | repo + `gofer demo` streaming the SDK faux provider | typed events flow end-to-end offline |
| **M1 · one good session** ✅ shipped 2026-07-12 | real provider + tools via SDK, minimal attach TUI | a real coding task, streaming, resumable after kill |
| **M2 · the daemon** ✅ shipped 2026-07-13 | supervisor + roster + overview⇄peek⇄attach + native ACP | an ACP client on a phone drives a session on a laptop |
| M3 · guardrails | **① daemon session→peers fan-out registry** (every registered peer gets every `session/update`; echo/dedup so prompts don't double-render) → **② sandbox** (seatbelt / bwrap+seccomp) → **③ approvals relay + phone approval UX**; then headless exec, daemon-as-service ([#42](https://github.com/jedwards1230/gofer/issues/42), first-use install prompt), lossless attach (daemonbridge reconstruction → lossless path), OTel | a phone approves a laptop tool call; a TUI attached to the same session watches the turn stream live |
| M4 · ecosystem | MCP on by default (tool-search index-first) + subagents first-class (roster tree, peek/attach into children, linked journals) + skills + plugin UX | a third-party plugin adds a tool with one config line |
| M5 · auto + polish | auto mode (reviewer pipeline), CC-asset import, mDNS pairing | auto mode survives a week of real ops without a bad allow |

## Fleet & multi-machine (design-ahead)

Sessions and daemons on other machines — LAN mDNS discovery or a self-hosted
rendezvous registry (heartbeat · list · optional relay). Lives entirely in
gofer; the SDK stays fleet-unaware. The TUI overview merges local + peer
rosters; attach is transparent because the Event/Op stream is the same bytes
locally or remote. Remote transport carries identity (TLS fingerprint in the
beacon, rendezvous-issued tokens) from day one.

*Open question (decide at M5)*: rendezvous protocol — leaning native-contract
passthrough, terminating ACP at each daemon (ACP is a projection; tunneling it
would double-encode).

## Settled decisions

- **License: Apache-2.0** both repos (NOTICE-based attribution).
- **Supervisor stays in the app** — promoted to the SDK only if a second app
  needs it unchanged.
- **Claude-subscription OAuth shipped at M1** (`gofer login`, earlier than the
  original M3 target), with API-key fallback from day one.
- **TUI is bubbletea v2**; plugin-contributed UI is a declarative widget
  vocabulary rendered by the host (plugins ship data + structure, never
  in-process code). Full design: [`TUI.md`](TUI.md).

## Glossary

| Term | Meaning |
|---|---|
| agent | a named manifest identity (model + tools + permissions + prompt); many sessions can run one agent |
| session | one conversation: an append-only JSONL tree; the unit of attach, fork, resume, and ACP exposure |
| turn | one user-prompt → model/tool loop → final message cycle |
| daemon | the long-running gofer process owning sessions; clients attach over socket/network |
| roster | the daemon's registry of live sessions; fleet = merged rosters |
| grant | a persisted, TTL'd permission rule created from an approval |
| skill | SKILL.md unit: metadata always in prompt, body on demand |
| plugin | out-of-process extension (subprocess JSON-RPC, later WASM) from any repo |
| tool | a callable the model can invoke — builtin, MCP, or plugin — one flat registry per agent |
| rendezvous | optional self-hosted registry daemons report to ("account mode"); never a scheduler |
