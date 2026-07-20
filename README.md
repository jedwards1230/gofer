# gofer

Your errand-runner for agents. **gofer** is a daemon + TUI for running and
supervising many coding agents at once — a roster of live sessions, peek/attach
navigation, and phone-driven sessions over ACP — built in Go on
[`agent-sdk-go`](https://github.com/jedwards1230/agent-sdk-go). (Tool-call
approvals reach your phone over ACP; see the [roadmap](#roadmap).)

> **Status: M6 — process isolation — shipped; M5 in flight.** M6 runs each
> session in its own detached `gofer session-worker` process behind a thin
> router daemon, so the daemon and CLI can be upgraded in place while live
> turns finish on the binary that started them. It is opt-in and **off by
> default** — enable it with `gofer daemon --workers`. M5 (ACP v1 featureset
> expansion) is in progress alongside it: `usage_update` and the `diff`/`plan`
> pass-throughs are on the ACP surface; rich content blocks, resume, and model
> discovery are still landing. Earlier milestones stand: M3's permission engine
> + approvals relay and M4's slash dispatcher and command panel — `/status`
> (per-provider auth), `/config` (a settings registry backed by `config.Save`),
> and `/model` (a picker that hot-swaps a live session's model), with
> autocomplete and a chat-style redesign. `gofer exec` runs headless one-shots;
> `gofer daemon install` runs it as a service; OpenTelemetry export is off by
> default. `gofer run`/`resume` route through a daemon or fall back
> in-process; `gofer ps`/`kill`/`archive` manage the roster; `gofer demo`
> streams a faux-provider session with no network. See
> [`docs/PRD.md`](docs/PRD.md) and [`docs/TUI.md`](docs/TUI.md) and the
> [roadmap](#roadmap).

## What it is

```
┌ overview ────────────────────────────────────────────┐
│ ● fix-ci        running   linux-build   $0.42  2m11s │
│ ● refactor-api  waiting   approval ⚠    $1.03  8m40s │
│ ○ docs-pass     done      —             $0.11  1h02m │
│                                                      │
│ [enter] peek · [a] attach · [ctrl-x] kill · [n] new  │
└──────────────────────────────────────────────────────┘
```

- **One roster, many agents** — every running session, its state, cost, and
  pending approvals in one screen; `overview ⇄ peek ⇄ attach` navigation.
- **Everything is a client** — the TUI, ACP clients (phone/editor), and
  headless exec all consume the same typed Event/Op stream. Attach from
  anywhere; the bytes are identical.
- **Structural permissions** — allow/ask/deny rules; approvals are protocol
  messages that render in the TUI or on your phone (Claude Code
  settings-format import lands later).
- **Slash commands** — a dispatcher with autocomplete opens a command panel:
  `/status` (per-provider auth), `/config` (live settings), `/model` (pick and
  hot-swap a session's model).
- **Session lifecycle you can trust** — event-sourced JSONL journals; kill or
  archive from the roster, resume after a crash, fork at any point. Journals
  are never deleted.

## Try it

```bash
go run ./cmd/gofer demo
```

Streams a scripted faux-provider session through the real event pipeline — no
API key, no network.

## Auth (M1)

```bash
gofer login anthropic          # subscription OAuth (paste the code back)
gofer login openai             # subscription OAuth (local browser redirect)
gofer login anthropic --api-key   # reads a key from stdin, never argv
gofer auth                     # show configured providers and credential status
gofer logout anthropic
```

Credentials persist in `~/.gofer/auth.json` (mode 0600). `gofer auth` never
prints token material.

> **Subscription-OAuth self-description caveat.** Logging in with subscription
> OAuth (`gofer login <provider>`, no `--api-key`) authenticates over the
> vendor's coding-assistant credential path (Anthropic's "Claude Code",
> OpenAI's "Codex"), which carries a fixed assistant identity in the system
> context. That identity can bleed into how the model describes *itself* in a
> session — so an agent may call itself "Claude Code" regardless of gofer's own
> system prompt. This is inherent to subscription auth, not a gofer bug. Use
> `--api-key` (or the provider's API-key env var) if you need the model's
> self-description to reflect only gofer's system prompt.

## Run a session (M1)

```bash
export ANTHROPIC_API_KEY=sk-...   # or `gofer login anthropic`
gofer run "create hello.txt containing hi using your tools, then summarize"
# Ctrl-C mid-run, then:
gofer resume <id> "continue"      # id was printed to stderr on start
gofer resume <id>                 # no prompt: print the transcript and exit
```

A real provider streams through the builtin tools (`bash`, `read`, `edit`,
`write`, `grep`, `glob`, `ls`) into a durable JSONL journal — kill it and the
settled prefix survives; resume folds it back into context.

Run interactively (a prompt given as an argument, in a real terminal, no
`--json`) and the stream renders through gofer's minimal attach TUI instead
of the plain transcript — esc or Ctrl-C interrupts the run, same as Ctrl-C on
the line renderer. Anything non-interactive — `--json`, a piped/redirected
stdout, or a prompt piped in on stdin — always renders as the line-oriented
stream, so scripts and CI never hit the TUI.

## Roadmap

| Stage | Ships |
|---|---|
| **M0 · scaffold** ✅ | repo + `gofer demo` streaming the SDK's faux provider |
| **M1 · one good session** ✅ | real provider, builtin tools, resumable sessions, cost accounting |
| **M2 · the daemon** ✅ | supervisor, roster, overview⇄peek⇄attach TUI, native ACP over WebSocket, bearer auth |
| **M3 · guardrails** ✅ | permission engine + approvals UX, sandboxed exec, headless mode |
| **M4 · command views** ✅ | slash dispatcher, `/status`/`/config`/`/model` panels, autocomplete, TUI redesign |
| **M5 · ACP v1 featureset expansion** 🚧 in flight | cross-repo ACP conformance push — `usage_update` on `session/update`, `diff` and `plan` pass-through, `session/set_config_option` + `session/list` (shipped); rich content blocks, resume, model discovery + `set_model`, capability stretch (titles, commands/mode) still landing |
| **M6 · process isolation** ✅ | detached per-session `gofer session-worker` processes behind a thin router daemon; upgrade the binary mid-turn without interrupting live sessions. Opt-in, off by default (`gofer daemon --workers`) |
| M7 · ecosystem | MCP servers, SKILL.md skills, out-of-process plugins, subagents first-class |
| M8 · auto + polish | auto mode with reviewer pipeline, CC-asset import, multi-machine discovery |

## License

[Apache-2.0](LICENSE). See [NOTICE](NOTICE) for attribution requirements.
