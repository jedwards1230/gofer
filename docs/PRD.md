# gofer — PRD & design

> Application half of the **gofer** platform. The framework half is
> [`jedwards1230/agent-sdk-go`](https://github.com/jedwards1230/agent-sdk-go),
> whose `docs/PRD.md` owns the contract, tenets, and SDK package design. This
> document covers what the app adds on top.

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
gofer                      # TUI (overview)
gofer demo                 # M0: offline faux-provider stream
gofer ps [--all]           # sessions incl. archived
gofer kill|archive <id>
gofer exec -p "..."        # headless (M3)
gofer import claude        # skills/commands import (M5)
```

## Session lifecycle

Event-sourced JSONL journals (SDK). `kill` = interrupt + terminate a running
session; `archive` = drop a finished one from the roster. **Journals are never
deleted** — both emit must-deliver events (`session.killed` / `.archived`).

## Milestones

| Stage | Ships | Proof |
|---|---|---|
| **M0 · scaffold** | repo + `gofer demo` streaming the SDK faux provider | typed events flow end-to-end offline |
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
- **Claude-subscription OAuth ships at M3** with API-key fallback from day
  one.
- **TUI is bubbletea v2**; plugin-contributed UI is a declarative widget
  vocabulary rendered by the host (plugins ship data + structure, never
  in-process code).
