# gofer

Your errand-runner for agents. **gofer** is a daemon + TUI for running and
supervising many coding agents at once — a roster of live sessions, peek/attach
navigation, and phone-driven sessions over ACP — built in Go on
[`agent-sdk-go`](https://github.com/jedwards1230/agent-sdk-go). (Tool-call
approvals reach your phone over ACP; see the [roadmap](#roadmap).)

> **Status: M3 — guardrails.** On top of the M2 daemon (a session supervisor
> behind an ACP-over-WebSocket listener, so an ACP client — e.g. a phone over
> your tailnet — drives a session live in the laptop TUI roster), M3 adds the
> permission engine and approvals relay: a tool-call approval fans out to every
> attached client and can be answered from your phone, with sandboxed
> containment (seatbelt / bwrap+seccomp) narrowing what needs a human. `gofer
> exec` runs headless one-shots; `gofer daemon install` runs it as a service;
> OpenTelemetry export is off by default. `gofer run`/`resume` route through a
> daemon or fall back in-process; `gofer ps`/`kill`/`archive` manage the roster;
> `gofer demo` streams a faux-provider session with no network. See
> [`docs/M3-PLAN.md`](docs/M3-PLAN.md) and [`docs/PRD.md`](docs/PRD.md) (proofs:
> [M2](docs/M2-PROOF.md), [M1](docs/M1-PROOF.md)) and the [roadmap](#roadmap).

## What it will be

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
- **Structural permissions** — allow/ask/deny rules (Claude Code
  settings-compatible), approvals as protocol messages that render in the TUI
  or on your phone.
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
settled prefix survives; resume folds it back into context. See
[`docs/M1-PROOF.md`](docs/M1-PROOF.md) for the full walkthrough.

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
| M4 · ecosystem | MCP servers, SKILL.md skills, out-of-process plugins |
| M5 · auto + polish | auto mode with reviewer pipeline, multi-machine discovery |

## License

[Apache-2.0](LICENSE). See [NOTICE](NOTICE) for attribution requirements.
