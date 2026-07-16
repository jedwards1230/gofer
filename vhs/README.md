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
- `roster-overview.tape` — the roster screen with mixed session states,
  capturing the ● status markers in color (yellow working / awaiting input
  incl. the ●2 pending count vs green finished).
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

Run: `scripts/tui-vhs.sh [slug...]` (no arg = all tapes, e.g.
`scripts/tui-vhs.sh panel-status panel-config`). It prebuilds
`vhs/.bin/harness`, then renders each tape to `vhs/out/`. Generated frames
(`vhs/out/`) and the built binary (`vhs/.bin/`) are gitignored.

Full workflow notes: [`docs/TUI.md`](../docs/TUI.md) → "Visual capture with VHS".
