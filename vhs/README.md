# TUI visual capture (VHS)

On-demand [charmbracelet VHS](https://github.com/charmbracelet/vhs) tapes that
render the gofer attach TUI to GIF/PNG, so real frames — colors, spacing,
glyphs — can be reviewed by eye. This **complements** the Ascii golden tests
(which stay the authoritative assertion); it is **not** a CI gate.

- `harness/` — a tiny `main` that drives the real `internal/tui` render path
  (`-scenario tool-call | approval | overview | panel | status | config`): the
  attach scenes replay a scripted event stream, the overview scene renders a
  static roster snapshot, and the command-panel scenes build the real
  `tui.App` over a canned `tui.Supervisor`/`tui.CommandEnv` and let the tape
  drive it with real keystrokes.
- `tool-call.tape` — a clean turn with a bash tool call (real command in the
  header, block rhythm).
- `approval.tape` — a turn ending in the inline permission prompt, with a
  failed tool call's red error marker and dimmed body above it.
- `overview.tape` — the roster screen with mixed session states, capturing the
  ● status markers in color (yellow working / awaiting input incl. the ●2
  pending count vs green finished).
- `panel.tape` — the command panel opened over the roster overview via
  `/status`, no session attached (Session rows read "—").
- `status.tape` — the Status tab attached to a session, showing real session
  identity plus both provider auth kinds (Anthropic OAuth, OpenAI API key).
- `config.tape` — the Config tab's settings-registry search list at gofer's
  own defaults.

Run: `scripts/tui-vhs.sh [tool-call|approval|overview|panel|status|config]`
(no arg = all). It prebuilds `vhs/.bin/harness`, then renders each tape to
`vhs/out/`. Generated frames (`vhs/out/`) and the built binary (`vhs/.bin/`)
are gitignored.

Full workflow notes: [`docs/TUI.md`](../docs/TUI.md) → "Visual capture with VHS".
