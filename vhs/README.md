# TUI visual capture (VHS)

On-demand [charmbracelet VHS](https://github.com/charmbracelet/vhs) tapes that
render the gofer TUI to GIF/PNG, so real frames — colors, spacing, glyphs —
can be reviewed by eye. This **complements** the Ascii golden tests (which
stay the authoritative assertion); it is **not** a CI gate.

Tape/scenario names follow one slug schema, `<area>-<view>[-<state>]`
(kebab-case), grouped by area:

- `harness/` — a tiny `main` that drives the real `internal/tui` render path.
  The `transcript-*` scenes replay a scripted event stream into the attach
  transcript; the `roster-*` scene renders a static roster snapshot; the
  `panel-*` scenes build the real `tui.App` over a canned
  `tui.Supervisor`/`tui.CommandEnv` and let the tape drive it with real
  keystrokes.
- `transcript-tool-call.tape` — a clean turn with a bash tool call (real
  command in the header, block rhythm). Screenshots:
  `transcript-tool-call-running`, `transcript-tool-call`.
- `transcript-approval.tape` — a turn ending in the inline permission prompt,
  with a failed tool call's red error marker and dimmed body above it.
- `transcript-compacting.tape` — compaction in progress: the transient
  `⋯ compacting context… (Ns)` indicator at the attach transcript's tail while
  an explicit `/compact` runs. Needs a Supervisor whose `Compact` BLOCKS
  (`vhsSupervisor.compactHold`) — against the instant-returning Compact every
  other scene uses, the indicator would appear and vanish inside one frame with
  nothing to photograph. The screenshot lands MID-second on purpose: the counter
  truncates, so capturing ~2.5s in sits in the middle of the "2s" bucket rather
  than on its edge, which is what keeps the tracked baseline from churning
  (gofer#297). The transcript beneath it is a MOCKED conversation — two
  completed turns with a tool call in each (`compactableHistory`), seeded
  through the broker's retained backlog (`vhsSupervisor.seed`) so it is on
  screen the moment the tape attaches, with no publish-vs-subscribe race. The
  history is load-bearing: compaction replaces a long context, so an indicator
  over an empty transcript would show the widget while misrepresenting what
  produces it.
- `transcript-overflow-recovery.tape` — the failure-triggered compaction
  sequence (jedwards1230/gofer#279): a user prompt, then the non-fatal
  `context window exceeded — compacting…` notice, then the `session.compacted`
  block, then the retried turn's answer. Adds no rendering path — it captures
  three EXISTING blocks in a stack that did not exist before, and what the
  Ascii goldens cannot see is whether those three registers (muted error line,
  accent structural block, ordinary text) read as one coherent explanation.
  The deliberate GAP is the subject: an overflow rejection generates nothing,
  so the transcript really does jump from prompt to notice with no assistant
  output between — drop the notice and the frame visibly loses its
  explanation. Entirely static (`overflowRecoveryHistory`, seeded via
  `vhsSupervisor.seed`), so unlike the compaction tape above there is no
  counter to capture mid-bucket and nothing for the baseline to churn on.
- `roster-overview.tape` — the roster screen with mixed session states,
  capturing the ● status markers in color (yellow working / awaiting input
  incl. the ●2 pending count vs green finished).
- `roster-delete-confirm.tape` — the armed `ctrl+x` delete confirm, captured as
  a **before/after pair** (`roster-delete-confirm-before` /
  `roster-delete-confirm`). The thing under review is a colour change — the
  focused row's highlight shifts from the neutral `Highlight` to the warm
  `HighlightArmed` — and one frame of the armed state can only show a colour,
  not a change, so the pair is what makes "just enough to signal a state
  change" reviewable. The dispatch-bar hint swaps in the same two frames; the
  Ascii goldens pin that text but cannot see the highlight, which is the gap
  this covers. One press only: the first `ctrl+x` is pure client state and
  never reaches the Supervisor, so the scene deletes nothing. Reuses the
  `panel-status-overview` scenario.
- `roster-delete-lands.tape` — the frame a quarter second AFTER the confirming
  `ctrl+x`, as a **before/after pair** (`roster-delete-lands-before` /
  `roster-delete-lands`). The companion to the tape above, which stops at the
  arm. The subject is a timing change (gofer#322): a roster-mutating op used to
  leave its stale row on screen until the next 1s poll came round, and now
  refetches the roster the moment it lands. The after-frame is taken at a fixed
  250ms — after the refetch, inside the poll interval — which is what makes the
  change visible at all; the Ascii goldens assert the text of a state and never
  how long it took to arrive. The selected row is Working, so the op dispatched
  is `kill`: the row goes `Working` → `Finished · killed` (amber → green) and
  the header count follows, rather than vanishing, because a kill keeps the
  journal. Needs `vhsSupervisor.Kill` to really mutate the canned roster —
  against a frozen one both frames would be identical. Reuses the
  `panel-status-overview` scenario.
- `roster-quit-confirm.tape` — the armed `ctrl+c` double-tap quit confirm
  (gofer#314), captured as a **before/after pair**
  (`roster-quit-confirm-before` / `roster-quit-confirm`). Unlike
  `roster-delete-confirm` above, the signal here is TEXT — a.status switches
  from empty to "ctrl-c again to quit", rendered in the theme's warn style —
  so an Ascii golden can already see the words; the tape's job is showing that
  it renders in the same warn-yellow every other caution note uses (issue
  #161's severity styling), which the Ascii profile cannot, and documenting
  that the note is momentary rather than permanent chrome. This is the
  GLOBAL confirm, not roster-specific — the same `App.confirmQuit` also runs
  from the command panel and the pending-approval/decision overlays (see
  `docs/TUI.md`'s "ctrl-c quits gofer" section) — captured once here since the
  note renders identically on every screen. One press only: the first
  `ctrl+c` is pure client state and never reaches `tea.Quit`. A second,
  confirming `ctrl+c` follows the last screenshot only to end the recording
  promptly. Reuses the `panel-status-overview` scenario.
- `roster-select-all.tape` — `ctrl+a` select-all, captured as a **before/after
  pair** (`roster-select-all-before` / `roster-select-all`). The claim under
  review is that the reverse video reaches the rows OUTSIDE the roster body —
  the identity header at the top, the dispatch bar and hint row at the bottom
  (#307). Ascii goldens render without colour and cannot see a highlight at
  all, so this is the only check on it. Pressed with an empty dispatch bar,
  which is the gate: with text in the bar `ctrl+a` stays "move to line start"
  and nothing highlights. Read-only — select-all only installs a client-side
  selection and writes the clipboard. Reuses the `panel-status-overview`
  scenario.
- `transcript-select-all.tape` — the same `ctrl+a` select-all on the **attach**
  screen, as a before/after pair (`transcript-select-all-before` /
  `transcript-select-all`). Not a duplicate of the roster pair: the chrome under
  test is a different set. `App.transcriptRegion`'s doc lists what the old clamp
  excluded, and the attach entry adds the **input box and its framing rules** on
  top of the status/usage footer — neither of which exists on the overview, so
  the roster pair demonstrates nothing about them. A regression confined to
  attach would leave every other tracked frame correct. The rendered after-frame
  confirms the header, transcript, both input framing rules, the input row and
  the usage footer are all highlighted. Empty input box (same `ctrl+a` gate).
  Reuses the `transcript-compacting` scenario for its seeded two-turn history
  and populated usage footer; that scenario's `compactHold` is **inert** here,
  since this tape never dispatches `/compact`.
- `roster-peek.tape` — the peek card: the roster-only session summary opened
  with **space** on an empty dispatch bar (enter/→ *attach* instead). Peek does
  not subscribe to the session's event stream, so the card renders purely from
  the roster snapshot. Reuses the `panel-status-overview` scenario.
- `panel-status-overview.tape` — the command panel opened over the roster
  overview via `/status`, no session attached (Session rows read "—").
- `panel-status.tape` — the Status tab attached to a session, showing real
  session identity plus both provider auth kinds (Anthropic OAuth, OpenAI API
  key).
- `panel-config.tape` — the Config tab's settings-registry search list at
  gofer's own defaults.
- `panel-model.tape` — the Model tab's picker with authenticated providers: a
  populated model list and the ✓ active-model mark.
- `panel-model-empty.tape` — the Model tab with zero authenticated providers:
  the empty-list state and its "/login" hint.
- `panel-model-daemon-refresh.tape` — issue #162's before/after: a
  daemon-backed roster whose header adopts a new default model **mid-run**.
  Screenshots the header, types the `/model` change, screenshots it again —
  one continuous process, no restart — as `panel-model-daemon-refresh-before`
  / `-after`. The daemon is an in-process stub probe, so the scene performs no
  network IO.
- `panel-thinking.tape` — the Thinking tab: the reasoning-effort adjuster
  (`Runner.SetEffort`) with its level list and the active level marked.
- `panel-usage.tape` — the Usage tab opened from the overview (no session
  attached): the honest empty state.
- `panel-stats.tape` — the Stats tab: the roster rollup (session-state counts
  and totals) over the canned two-session roster.
- `panel-help.tape` — the Help tab: the keymap + slash-command reference
  rendered from the live command registry.
- `panel-resume.tape` — the Resume tab: the session picker with a fetched
  listing applied (an offline session plus a live one). Listing only, no
  resume.

Each of the five panel-thinking/usage/stats/help/resume tapes reproduces the
exact state its `internal/tui/testdata/app_panel_<tab>.golden` pins, so the
color frame and the Ascii golden agree.

Run: `scripts/tui-vhs.sh [slug...]` (no arg = all tapes, e.g.
`scripts/tui-vhs.sh panel-status panel-config`). It prebuilds
`vhs/.bin/harness`, then renders each tape to `vhs/out/`. Generated frames
(`vhs/out/`) and the built binary (`vhs/.bin/`) are gitignored.

`snapshots/` is the **tracked** PNG baseline (CI-authoritative) — pass
`--snapshot` to mirror the key-frames into it locally. CI commits it so TUI
changes show as native GitHub image diffs: `vhs-baseline.yml` keeps `main`
current, and `vhs-capture.yml` appends each PR's renders to a per-PR
`vhs-captures-pr-<n>` branch for a `main`-vs-feature diff. Advisory only — never
a merge gate.

Full workflow notes: [`docs/TUI.md`](../docs/TUI.md) → "Visual capture with VHS".
