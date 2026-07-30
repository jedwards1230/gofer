# M7 Round A+B — daily-driver build (gofer)

> **Living checklist.** Status is updated as pieces land. Integration branch
> `milestone/m7-round-ab`; every piece is its own PR **based on that branch**, not
> `main`. The integration→`main` PR is user-gated.
> Cross-repo source of record: `jedwards1230/home-orchestration`
> `docs/projects/gofer-m7-round-ab-plan.md`. SDK half:
> `jedwards1230/agent-sdk-go` `docs/milestones/M7-round-ab.md`.

## Why

The TUI is mature (M0–M6 shipped) but cannot carry a full working day. Two hard
stops — no compaction, a hardcoded system prompt — and three capability gaps
(MCP, search, skills) stand between it and daily use. This round closes all of
them. The permission/actor security cluster is a deliberate follow-up.

## Status

| # | Piece | Repo | State | PR |
|---|---|---|---|---|
| 0 | Re-pin `agent-sdk-go` v0.19.0 → v0.21.0 | gofer | in flight | — |
| 0 | Config schema: all new sections, one writer | gofer | pending | — |
| 0 | Index-first tool-registry contract | both | pending | — |
| 1 | `Runner.Compact` seam | SDK | in flight | — |
| 1 | Auto-compact + `/compact` + `/context` | gofer | blocked on SDK seam | — |
| 2 | Prompt files via config; delete `defaultSystemPrompt` | gofer | pending | — |
| 3 | MCP: optional SDK `mcp/` package + gofer wiring | both | pending | — |
| 4 | Search providers (Brave + SearXNG) | both | pending | — |
| 4 | `tool_search` + preload toggle | gofer | pending | — |
| 5 | Skills: `SKILL.md`, config dirs, progressive disclosure | both | pending | — |
| 6 | Wire SDK `lsp/`, verified live | gofer | PR open (verified live w/ real gopls) | [#269](https://github.com/jedwards1230/gofer/pull/269) |

## Sequencing

Two seams are shared and are serialized ahead of the work that depends on them:

- **Config schema is a single-writer bottleneck.** Pieces 2, 3, 4, and 5 all
  extend gofer's config. One config-schema PR adds every new section first; the
  feature PRs then fill their own section in parallel without fighting over
  `internal/config`.
- **Tool-registry indexing is the other shared seam.** Piece 4's `tool_search` +
  preload toggle and piece 3's MCP federation must agree on one index-first
  registry contract, designed once before either builds on it. Federating many
  MCP servers must never dump every schema into context.

Piece 1's SDK seam lands before gofer's auto-compact work. Pieces 2, 5, and 6 are
otherwise independent.

## Decisions

Recorded as they settle.

- **SDK pin**: v0.21.0 is an annotated tag on SDK `main` (`389a829`), so the
  re-pin is to a real tag, not a pseudo-version. At the milestone boundary, cut a
  fresh SDK release tag and re-pin again — a squash-merge of the SDK integration
  PR deletes the branch and orphans a pseudo-version (M2 lesson, repeated at M3).

## Definition of done

Per piece: implementation + tests, CI green, review threads resolved, docs
updated in the same PR. Round-level: `defaultSystemPrompt` deleted, a long
session survives compaction, an MCP server's tools reach a session via config
alone, a search provider answers from both Brave and SearXNG, a skill loads on
demand, and LSP signal is demonstrated live.

Auto-compaction must be **visible** in the transcript — never silent.

## Deferred

Not started this round: the permission/actor security cluster
(`jedwards1230/gofer#262`, `#263`, `#264`, `#216` — one design, actor identity on
the wire, not four patches), Tailscale integration depth, image/audio/resource
ACP content blocks, and checkpoint/rewind (`jedwards1230/gofer#183`).
