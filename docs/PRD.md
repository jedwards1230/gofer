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

## CLI surface

```
gofer                       # TUI: health-probe daemon → auto-spawn if absent → overview
gofer attach <session>      # attach straight into one session
gofer demo                  # M0: offline faux-provider stream
gofer exec [-p prompt] [--agent name] [--json] [--output-schema file]
                            # headless one-shot: JSONL events on stdout (M3)
gofer serve [--host unix://…|tcp://…]   # run the daemon in the foreground
gofer acp serve             # ACP over stdio (editors, stdio→ws bridges)
gofer ps [--all]            # roster (--all includes archived; later: fleet)
gofer kill|archive <id>     # stop running / clear finished (journal kept)
gofer agents|skills|plugins # list what's composed; `plugins install <module>`
gofer import claude         # idempotent import of CC skills/commands (M5)
                            #   (settings.json permissions honored natively from M3)
gofer doctor                # providers, LSP servers on PATH, daemon, sandbox
gofer config get|set …      # global or project config
```

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
  ▼ not sandboxable                (denial text → model retries)
  ③ LLM reviewer   out-of-band call · strict JSON {decision, risk, rationale}
  │                30s timeout · 360-tok cap · fail-closed · injection-framed
  ├─ low-risk ∧ high-confidence ─▶ ✓ run (audit-logged)
  └─ anything else ──────────────▶ ✋ human (TUI · ACP · push)
```

Entering auto mode drops broad grants — `Bash(*)` can never bypass ③. Stages
①+② ship before ③ exists; each is independently useful. The reviewer is one
more SDK loop invocation with a different system prompt.

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

## Milestones

| Stage | Ships | Proof |
|---|---|---|
| **M0 · scaffold** ✅ shipped 2026-07-12 | repo + `gofer demo` streaming the SDK faux provider | typed events flow end-to-end offline |
| M1 · one good session | real provider + tools via SDK, minimal attach TUI | a real coding task, streaming, resumable after kill |
| M2 · the daemon | supervisor + roster + overview⇄peek⇄attach + native ACP | an ACP client on a phone drives a session on a laptop |
| M3 · guardrails | approvals UX + grants + sandbox + headless exec | Claude Code `settings.json` honored; approval from the phone |
| M4 · ecosystem | MCP + skills + plugins surfaced in the TUI | a third-party plugin adds a tool with one config line |
| M5 · auto + polish | auto mode (reviewer pipeline), CC-asset import, mDNS pairing | auto mode survives a week of real ops without a bad allow |

## Fleet & multi-machine (design-ahead)

Sessions and daemons on other machines — LAN mDNS discovery or a self-hosted
rendezvous registry (heartbeat · list · optional relay). Lives entirely in
gofer; the SDK stays fleet-unaware. The TUI overview merges local + peer
rosters; attach is transparent because the Event/Op stream is the same bytes
locally or remote. Remote transport carries identity (TLS fingerprint in the
beacon, rendezvous-issued tokens) from day one.

*Open question (decide at M2)*: rendezvous protocol — leaning native-contract
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
