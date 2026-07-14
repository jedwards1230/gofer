# M3 — guardrails (app half): tracking plan

Living checklist for the M3 milestone in this repo. The spec is
[`PRD.md`](PRD.md) (milestone table, auto-mode pipeline, constraints); this doc
tracks progress and sequencing. Cross-repo plan of record lives in the umbrella
orchestration repo (`docs/projects/gofer-m3-plan-and-docs-refresh.md`).

## Sequencing (order matters)

1. [x] **Daemon session→peers fan-out registry.** ✅ shipped (#55). Peers register interest at
       `session/load`; every registered peer then receives every `session/update`
       for that session, regardless of which client drove the turn. Includes an
       **echo/dedup rule** — suppress the user-prompt echo to the originating peer
       (or key client rendering by message id) so a prompt can't double-render
       once all clients see all events. This is the one missing primitive behind
       the CLI⇄ACP live-sync gap, and it shares plumbing with the approvals relay
       (③). SDK broker already fans out to N subscribers — this is gofer-side.
2. [x] **Session-visibility model — DECIDED (2026-07-13): fleet-global, cwd as a
       label.** ✅ shipped with #55 (session/list drops the cwd-hiding filter, keeps cwd as metadata). `session/list` returns every session; the working dir is a
       filterable tag, not a wall — so all clients see one roster and any client
       can sync any session. (cwd-scoped isolation may return later as opt-in
       config, but the default is global.)
3. [x] **Sandbox (stage ②).** ✅ shipped. seatbelt (macOS `sandbox-exec` + a
       generated deny-by-default SBPL profile) / bwrap+seccomp (Linux, network
       unshared) containment, in `internal/sandbox` — the SDK owns only the
       `loop.Container` interface, backends live here. Runtime-detected with a
       no-op fallback (`CanContain=false`) so an uncontainable call **falls back
       to a human approval**, never silently blocked or run uncontained (decided
       2026-07-13). The RuleGuard's Container gates the decision (contain-or-ask);
       a sandbox-wrapping tool registry (bash wrapped in the generated profile,
       injected via `runner.Options.Tools`) runs an allowed+containable call
       contained. Profile generation is a pure function of the workdir — no env
       secrets can leak into it (asserted in tests).
4. [x] **Approvals relay + phone approval UX.** ✅ shipped. `permission.requested`
       (a must-deliver SDK event) fans out to **every attached peer** via the
       wave-① registry as a `gofer/permission_requested` notification; a
       `permission.reply` op from ANY peer — routed by call id → the session's
       reference `loop.Gate` — gates execution, then the loop proceeds or denies.
       TUI approval dialog (allow / deny / toggle-remember) plus a roster ✋N
       pending badge so a supervisor sees a waiting session without attaching.
       Because the surface is spec-general, a phone approving a laptop-driven
       session works with zero client-specific code. **Depended on an SDK seam**
       (`runner.Options.Guard/Approver`) added in agent-sdk-go #41.
5. [x] **Headless exec** (`gofer exec`). ✅ shipped. In-process, one-shot —
       never daemon-routed, unlike run/resume/bare gofer. Streams the SDK's
       `exec.Run` JSONL event contract directly to stdout (no banner, no
       summary — the only thing on stdout is the event stream); `--agent`
       fails clean until an agent-manifest registry exists;
       `--output-schema` validates the final result, reporting a
       `*exec.SchemaError` as a normal command error.
6. [x] **Daemon-as-a-service** ([#42](https://github.com/jedwards1230/gofer/issues/42)):
       launchd/systemd install + first-use install prompt. ✅ shipped (#42).
       `gofer daemon install|uninstall|status` writes a launchd user agent /
       systemd `--user` unit; loopback default is token-free, a non-loopback
       bind carries its token only via a 0600 `<root>/daemon.env` file (never
       the unit or argv). First-use prompt fires only on a fully interactive
       terminal (pure `shouldPromptInstall` gate), a complete no-op otherwise.
7. [x] **Lossless attach.** ✅ shipped. A `gofer/event` notification carries each
       source `event.Event`'s own MarshalJSON envelope, fanned uniformly to every
       attached peer alongside `session/update`; the bridge ignores
       `session/update` and replays `gofer/event` verbatim via `event.New*`
       (incl. `tool.call.delta` and `tool.call.finished`'s Diagnostics/Spill*,
       both entirely dropped by ACP's projection). History replay on
       `session/load` gets the same treatment (`historyEvents`).
8. [x] **OTel.** ✅ shipped, in a new `internal/telemetry/` package: spans per
       turn / provider-call proxy / tool execution off the Event/Op stream;
       session/turn/token/cost/error metrics; OTLP export, off by default;
       trace-ids stamped into slog records. gofer owns the otel dependency +
       exporters.

## Constraints

- **The daemon ACP surface stays spec-general** — no client-specific behavior,
  ever (many clients: phone, editor, web later). Client-specific anything lives
  in the client.
- Approvals-reaching-your-phone is the product thesis; **containment complements
  approvals, it never replaces them.**

## Exit gate

A **live multi-client test pass** is required before the milestone closes — two
clients on one session (one of them a phone), watching a turn stream live and
gating a tool call via approval. Automated PR review caught zero of M2's
cross-connection bugs; live client testing caught all of them.
