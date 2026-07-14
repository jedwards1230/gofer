# TUI visual capture (VHS)

On-demand [charmbracelet VHS](https://github.com/charmbracelet/vhs) tapes that
render the gofer attach TUI to GIF/PNG, so real frames — colors, spacing,
glyphs — can be reviewed by eye. This **complements** the Ascii golden tests
(which stay the authoritative assertion); it is **not** a CI gate.

- `harness/` — a tiny `main` that drives the real `internal/tui` render path
  through a fixed, scripted event stream (`-scenario tool-call | approval`).
- `tool-call.tape` — a clean turn with a bash tool call (real command in the
  header, block rhythm).
- `approval.tape` — a turn ending in the inline permission prompt, with a
  failed tool call's softened error styling above it.

Run: `scripts/tui-vhs.sh [tool-call|approval]` (no arg = all). It prebuilds
`vhs/.bin/harness`, then renders each tape to `vhs/out/`. Generated frames
(`vhs/out/`) and the built binary (`vhs/.bin/`) are gitignored.

Full workflow notes: [`docs/TUI.md`](../docs/TUI.md) → "Visual capture with VHS".
