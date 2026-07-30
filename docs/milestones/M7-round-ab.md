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
| 0 | Re-pin `agent-sdk-go` v0.19.0 → v0.21.0 | gofer | **merged** | [#267](https://github.com/jedwards1230/gofer/pull/267) |
| 0 | Track the SDK integration branch + carry `session.compacted` payload | gofer | **merged** | [#271](https://github.com/jedwards1230/gofer/pull/271) |
| 0 | Config schema: all six new sections, one writer | gofer | **merged** | [#270](https://github.com/jedwards1230/gofer/pull/270) |
| 0 | Index-first tool-registry contract | both | **designed + built** | [agent-sdk-go#114](https://github.com/jedwards1230/agent-sdk-go/pull/114) |
| 1 | `Runner.Compact` seam | SDK | **merged** | [agent-sdk-go#111](https://github.com/jedwards1230/agent-sdk-go/pull/111) |
| 1 | Auto-compact + `/compact` + `/context` | gofer | in flight | — |
| 2 | Prompt files via config; delete `defaultSystemPrompt` | gofer | verified, in review | [#272](https://github.com/jedwards1230/gofer/pull/272) |
| 3 | MCP: optional SDK `mcp/` package | SDK | verified, in review | [agent-sdk-go#116](https://github.com/jedwards1230/agent-sdk-go/pull/116) |
| 3 | MCP: gofer server config + connection manager | gofer | pending | — |
| 4 | Search providers (Brave + SearXNG) | SDK | **merged** | [agent-sdk-go#113](https://github.com/jedwards1230/agent-sdk-go/pull/113) |
| 4 | `web_search` tool + `tool_search` + preload toggle wiring | gofer | pending | — |
| 5 | Skills: `SKILL.md` loading, progressive disclosure | SDK | **merged** | [agent-sdk-go#115](https://github.com/jedwards1230/agent-sdk-go/pull/115) |
| 5 | Skills: config-driven dirs + **precedence fix** | gofer | pending | — |
| 6 | Wire SDK `lsp/`, verified live | gofer | **merged** | [#269](https://github.com/jedwards1230/gofer/pull/269) |
| 6 | LSP real config reads + drift test + `/config` row | gofer | **merged** | [#275](https://github.com/jedwards1230/gofer/pull/275) |
| — | CI: `lsp-live` job so the LSP proof is a gate | gofer | **merged** | [#273](https://github.com/jedwards1230/gofer/pull/273), [#274](https://github.com/jedwards1230/gofer/pull/274) |
| — | CI: `-race` on milestone PRs | SDK | **merged** | [agent-sdk-go#117](https://github.com/jedwards1230/agent-sdk-go/pull/117) |

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
  re-pin is to a real tag, not a pseudo-version. Note `git rev-parse v0.21.0`
  returns the *tag object*; dereference with `v0.21.0^{commit}` to compare against
  a branch. At the milestone boundary, cut a fresh SDK release tag and re-pin
  again — a squash-merge of the SDK integration PR deletes the branch and orphans
  a pseudo-version (M2 lesson, repeated at M3).
- **The re-pin was not test-only.** `event.NewSessionForked` changed signature in
  **v0.20.0** via `jedwards1230/agent-sdk-go#100` (checkpoint/fork/rewind), gaining
  `at`/`label`. gofer's only consumer was `internal/wirestream/reconstruct.go`,
  which had to carry both fields in its envelope to keep reconstruction lossless —
  the encoder needed no change, since `internal/daemon/event_relay.go` forwards the
  payload verbatim and `SessionForked.MarshalJSON` already emits both. Covered by
  `TestHandleNotificationReplaysGoferEventKinds`, which asserts field-for-field
  equality through the notification path; the `session.forked` case was
  strengthened to use non-empty values so it exercises the new fields instead of
  passing on zero values.
- **No SDK regression in the v0.19.0..v0.21.0 range.** The range was searched for
  the synthetic-delta / event-sequence changes that were expected to break
  assertions (incl. `a4c410b`, which touches `event/op.go`); nothing gofer is
  sensitive to. The one break was the `NewSessionForked` signature above.
- **Merge method for this round is squash**, so each piece lands as one commit on
  the integration branch and the eventual integration→`main` PR reads as one
  commit per workstream.

## Definition of done

Per piece: implementation + tests, CI green, review threads resolved, docs
updated in the same PR. Round-level: `defaultSystemPrompt` deleted, a long
session survives compaction, an MCP server's tools reach a session via config
alone, a search provider answers from both Brave and SearXNG, a skill loads on
demand, and LSP signal is demonstrated live.

Auto-compaction must be **visible** in the transcript — never silent.

## Process rules this round established

- **Gate the merge result, not the branch.** A branch cut before a dependency
  re-pin gates a combination that will never ship — `#269` was green on
  `agent-sdk-go v0.19.0` + LSP, a pairing that does not exist post-merge. Merge the
  integration branch into a detached worktree first, then run build/vet/test/
  `-race`/lint there. Standard per-piece step, not a judgment call.
- **A skipping test reads exactly like a passing one.** The LSP live tests skip
  without `gopls`, so four green checks said nothing about the deliverable. A CI
  job whose tests can skip is not a gate until it (a) fails when the dependency is
  missing, (b) fails on any `SKIP`, and (c) fails when zero target tests ran.
- **Never chain a check with the action it gates.** `gh-resolve-threads … && gh pr
  merge …` in one call merged `#273` over two unresolved threads — the listing
  exits 0 and its output arrives too late to read. Separate calls.
- **`gh` resolves a bare PR number against the current directory's repo** and
  returns a plausible wrong answer rather than an error. Always pass
  `--repo owner/name` in a multi-repo tree.
- **Use `--body-file` for PR comments.** An unescaped backtick in `--body "…"` is
  command-substituted and silently deletes a word.
- **Workers do not edit this file.** The owner is its single writer; per-piece PRs
  editing it serialized every merge behind a conflict.

## Found along the way

- [#268](https://github.com/jedwards1230/gofer/issues/268) —
  `TestResumeOfflineSpawnsFreshWorker` duplicates history under full-suite
  `-race`. Load-sensitive: fails only in a full `-race ./...` run, never
  package-isolated. Ruled out as caused by the SDK re-pin (base fails the same way
  under load; the bump touches neither resume nor the router's rebuild). Filed
  rather than tolerated because the assertion describes a correctness property,
  so it may be a real race in offline resume.

## Deferred

Not started this round: the permission/actor security cluster
(`jedwards1230/gofer#262`, `#263`, `#264`, `#216` — one design, actor identity on
the wire, not four patches), Tailscale integration depth, image/audio/resource
ACP content blocks, and checkpoint/rewind (`jedwards1230/gofer#183`).
