# M5 — ACP v1 Featureset Expansion

Integration branch: `milestone/m5-acp-featureset`. Slice PRs target **this branch** (not `main`); the branch merges to `main` when M5 lands. Tracking PR is the draft opened from this branch.

Cross-repo vertical: **agent-sdk-go** models/projects → **gofer** emits → **Agmente** decodes. Tracked against an internal ACP v1 conformance matrix (spec ↔ SDK ↔ gofer ↔ Agmente).

**Policies** (decided): *promote-if-stable* — a capability goes on the standard ACP surface when a stable `schema/v1` variant exists; stays gofer-native only when the spec surface is unstable/absent (`set_model`, `gofer/event` stay native). Model discovery via a *gofer-native list-models* endpoint until `providers/*` stabilizes.

> Milestone numbers skew by repo: this is agent-sdk-go's **M4**, gofer's **M5**. Each repo's `docs/PRD.md` is authoritative for its own numbering.

## Slices

- [x] **Slice 1 — `usage_update`** — SHIPPED. usage/cost on the ACP surface + `gofer run` renderer (agent-sdk-go v0.6.0; gofer #97/#99).
- [ ] **Slice 2 — Rich content & tool-call blocks** — SDK emits `diff`/`image`/`resource` from tools (producers), gofer passes through (free), Agmente renders. *SDK-first → needs an SDK release; sequenced, not in this branch's parallel batch.*
- [ ] **Slice 3 — Session methods** — `session/list` dispatch + `set_config_option` (SDK must model `set_config_option` first; `session/list` types + `cwd`/`title` already modeled), resume, Agmente client. *Partly SDK-first.*
- [ ] **Slice 4 — Model discovery** — **gofer-native list-models endpoint** over the provider registry (*this branch*); Agmente picker (Agmente repo). `set_model` stays gofer-native.
- [ ] **Slice 5 — Capability stretch (net-new)** — `session_info_update` (needs session titles), `plan` (needs a plan concept), `available_commands_update`/`current_mode_update`/`config_option_update` (need registries). Decide subset.
- [ ] **TUI — transcript word-wrap** — chat/message body wraps to width instead of truncating with `…` (`internal/tui`), while list/status/roster rows stay clamped. Golden-file assertion (`termenv.Ascii`).

## In this branch now (parallel)

1. **TUI transcript word-wrap** — the visible bug from the roster/attach view.
2. **gofer-native list-models endpoint** — Slice 4's gofer half.

Cross-repo slices (2, 3, 5) are tracked here but land via their own SDK PR → release → gofer integration sequence, not as direct PRs into this branch.
