# gofer

Your errand-runner for agents. **gofer** is a daemon + TUI for running and
supervising many coding agents at once — a roster of live sessions, peek/attach
navigation, approvals that reach your phone — built in Go on
[`agent-sdk-go`](https://github.com/jedwards1230/agent-sdk-go).

> **Status: M1 in progress.** `gofer run`/`gofer resume` drive a real
> provider and the builtin tool set through a durable, kill-resumable
> session journal (see [`docs/M1-PROOF.md`](docs/M1-PROOF.md)); `gofer demo`
> still streams a faux-provider session with no network. The daemon,
> supervisor, and TUI land at M2 (see the [roadmap](#roadmap)).

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

## Roadmap

| Stage | Ships |
|---|---|
| **M0 · scaffold** ✅ | repo + `gofer demo` streaming the SDK's faux provider |
| **M1 · one good session** (in progress) | real provider, builtin tools, resumable sessions, cost accounting |
| M2 · the daemon | supervisor, roster, overview⇄peek⇄attach TUI, native ACP |
| M3 · guardrails | permission engine + approvals UX, sandboxed exec, headless mode |
| M4 · ecosystem | MCP servers, SKILL.md skills, out-of-process plugins |
| M5 · auto + polish | auto mode with reviewer pipeline, multi-machine discovery |

## License

[Apache-2.0](LICENSE). See [NOTICE](NOTICE) for attribution requirements.
