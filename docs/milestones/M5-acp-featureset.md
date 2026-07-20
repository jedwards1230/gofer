# M5 — ACP v1 Featureset Expansion

Integration branch: `milestone/m5-acp-featureset`. Slice PRs target **this branch** (not `main`); the branch merges to `main` when M5 lands. Tracking PR is the draft opened from this branch.

Cross-repo vertical: **agent-sdk-go** models/projects → **gofer** emits → **Agmente** decodes. Tracked against an internal ACP v1 conformance matrix (spec ↔ SDK ↔ gofer ↔ Agmente).

**Policies** (decided): *promote-if-stable* — a capability goes on the standard ACP surface when a stable `schema/v1` variant exists; stays gofer-native only when the spec surface is unstable/absent (`set_model`, `gofer/event` stay native). Model discovery via a *gofer-native list-models* endpoint until `providers/*` stabilizes.

> Milestone numbers skew by repo: this is agent-sdk-go's **M4**, gofer's **M5**. Each repo's `docs/PRD.md` is authoritative for its own numbering.

## Slices

- [x] **Slice 1 — `usage_update`** — SHIPPED. usage/cost on the ACP surface + `gofer run` renderer (agent-sdk-go v0.6.0; gofer #97/#99).
- [ ] **Slice 2 — Rich content & tool-call blocks** — SDK emits `diff`/`image`/`resource` from tools (producers), gofer passes through (free), Agmente renders. **gofer `diff` pass-through DONE** (agent-sdk-go v0.7.0; the `diff` block rides `acp.ToSessionUpdate` unchanged — daemon-path test proves it, `gofer run` renderer shows a compact `edited <path>` summary). Remaining: `image`/`resource` blocks + Agmente rendering.
- [ ] **Slice 3 — Session methods** — **gofer `session/set_config_option` dispatch + `session/list` confirmation DONE** (agent-sdk-go v0.7.0): `set_config_option` `configId=model` maps to the gofer-native `set_model` path and replies a `model` select option (registry catalog + current value); unknown `configId` → rpc error. `session/list` was already wired and still holds on the v0.7.0 types. Remaining: resume, Agmente client.
- [ ] **Slice 4 — Model discovery** — **gofer-native list-models endpoint** over the provider registry (*this branch*); Agmente picker (Agmente repo). `set_model` stays gofer-native.
- [ ] **Slice 5 — Capability stretch (net-new)** — `session_info_update` (needs session titles), `plan` (needs a plan concept), `available_commands_update`/`current_mode_update`/`config_option_update` (need registries). Decide subset. **`plan` pass-through DONE (5b gofer half)** (agent-sdk-go v0.9.0): the SDK's `update_plan` builtin rides `tool.Builtins` (already wired by the runner), and its `plan` event rides `acp.ToSessionUpdate` unchanged — daemon-path test proves the entries reach a peer, `gofer run` renderer shows a compact `N steps (M done)` summary. No gofer projection code beyond the re-pin + renderer case. `update_plan` is mutation-free, so it is added to `sandbox.containableTools` — the daemon auto-allows it (no `session/request_permission` per plan revision), exactly like the read/ls file tools. **`config_option_update` emit DONE (5c gofer half)** (agent-sdk-go v0.10.0): both model-swap routes (ACP `session/set_config_option` and gofer-native `gofer/set_model`) now emit `event.ConfigOptionsUpdated` on an actual model change — the daemon builds the neutral `event.ConfigOption` model snapshot from the same provider-registry catalog the `set_config_option` response uses (`modelConfigOptionEvent`, twin of `modelConfigOption`), publishes it onto the session stream via the runner Emit seam (`Supervisor.EmitConfigOptions`), and fans it out live to every attached peer, where `acp.ToSessionUpdate` projects it to `config_option_update` (pass-through, no gofer-side ACP synthesis). Emit-on-change guarded (a no-op re-select fans nothing); direct fan-out because there is no continuous broker drain outside a `session/prompt`. Daemon-path tests prove both routes surface the snapshot with the new `currentValue` to a second attached peer.
- [x] **TUI — transcript word-wrap** — SHIPPED. Chat/message body wraps to width instead of truncating with `…`: `transcriptLines` runs each rendered line through `wrap` (`internal/tui/model.go:467-469`, `wrap` at `:814` over `ansi.Wrap`). List/status/roster rows stay clamped on `truncate`, as intended.

## In this branch now (parallel)

1. **gofer-native list-models endpoint** — Slice 4's gofer half.

Cross-repo slices (2, 3, 5) are tracked here but land via their own SDK PR → release → gofer integration sequence, not as direct PRs into this branch.
