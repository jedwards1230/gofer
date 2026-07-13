# M3 — guardrails (app half): tracking plan

Living checklist for the M3 milestone in this repo. The spec is
[`PRD.md`](PRD.md) (milestone table, auto-mode pipeline, constraints); this doc
tracks progress and sequencing. Cross-repo plan of record lives in the umbrella
orchestration repo (`docs/projects/gofer-m3-plan-and-docs-refresh.md`).

## Sequencing (order matters)

1. [ ] **Daemon session→peers fan-out registry.** Peers register interest at
       `session/load`; every registered peer then receives every `session/update`
       for that session, regardless of which client drove the turn. Includes an
       **echo/dedup rule** — suppress the user-prompt echo to the originating peer
       (or key client rendering by message id) so a prompt can't double-render
       once all clients see all events. This is the one missing primitive behind
       the CLI⇄ACP live-sync gap, and it shares plumbing with the approvals relay
       (③). SDK broker already fans out to N subscribers — this is gofer-side.
2. [ ] **Session-visibility model — DECIDED (2026-07-13): fleet-global, cwd as a
       label.** `session/list` returns every session; the working dir is a
       filterable tag, not a wall — so all clients see one roster and any client
       can sync any session. (cwd-scoped isolation may return later as opt-in
       config, but the default is global.)
3. [ ] **Sandbox (stage ②).** seatbelt (macOS) / bwrap+seccomp (Linux) containment
       for tool execution. Binary policy: contain if possible; if a call **can't
       be contained on this host** (no sandbox available, or an inherently
       un-sandboxable tool), **fall back to a human approval** — never silently
       block, never silently run uncontained (decided 2026-07-13).
4. [ ] **Approvals relay + phone approval UX.** Route `permission.requested` to
       every attached client (built on ①); `permission.reply` gates execution;
       TUI approval dialog. Agmente already ships the `session/request_permission`
       UI, so a spec-general ACP surface lights it up with zero client work.
5. [ ] **Headless exec** (`gofer exec`).
6. [ ] **Daemon-as-a-service** ([#42](https://github.com/jedwards1230/gofer/issues/42)):
       launchd/systemd install + first-use install prompt.
7. [ ] **Lossless attach.** Promote the daemonbridge's client-side reconstruction
       to a lossless/byte-exact path.
8. [ ] **OTel.** Spans per turn / provider call / tool execution off the Event/Op
       stream; session/turn/token/cost/error metrics; OTLP export; trace-ids
       stamped into slog records. gofer owns the otel dependency + exporters.

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
