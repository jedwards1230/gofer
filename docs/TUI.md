# gofer TUI — design

The TUI is a projection of the Event/Op contract like every other client.
Navigation is three altitudes — **overview ⇄ peek ⇄ attach** — modeled as a
screen stack. Golden-file tests come first: the `testkit` harness pins fixed
sizes, forces `termenv.Ascii`, and uses a test theme (see
[`TESTING.md`](TESTING.md)).

**Status**: `internal/tui` holds the M2 TUI — the attach `Model` (transcript +
input, driven by `Model.Ingest`), the `Overview` roster screen, the `Peek`
split, collapsed tool-block rendering, and the `App` screen-stack root that
composes them under the navigation contract (see [Roster &
navigation](#roster--navigation-m2) below). Still ahead: the dialog stack and
central keymap registry, then slash commands and plugin UI (M4+).

## The three altitudes

**Overview** — one row per session (a session = a task, titled by the work).
A row may be a whole fan-out hierarchy; it collapses to aggregate state,
agent count, and total pending approvals (`✋ N`), and expands inline to the
subagent tree. `space` peek · `enter` attach · `a` approve · `n` new ·
`ctrl-x` kill (running; confirm; subtree interrupted) or archive (finished).
Journals are never deleted — `gofer ps --all` lists archived sessions.

**Peek** — read-only tail of one session without stealing input. For a
fan-out it defaults to the child most needing attention (a waiting
approval); `J/K` cycles agents within the session, `j/k` cycles sessions.
Approvals are actionable without attaching.

**Attach** — full transcript + input; `esc` detaches back to overview.

A pending permission request is **not** a centered modal — it renders inline in
the conversation's bottom UI. The transcript keeps a permanent `✋ <tool>` badge
in the flow, while the live prompt **commandeers the input line**: it takes the spot the text
input normally occupies and stays anchored there until answered, then the input
returns. It reads as a confirm prompt — the tool + args, the question, the
action row, and a dim footer — keyed `a`/`d`/`r` (`r` toggles remember), `esc`
dismisses without answering (the request stays pending; a re-attach re-surfaces
it):

```
 ✋ bash · cmd=rm -rf /tmp/session-fixtures
 Allow this tool call?
   [a] allow   [d] deny   [r] remember: off
 esc cancel · session 0192a1b2-…
```

Resolution is deliberately quiet. A routine **allow** adds *no* transcript
line — the `✋` badge already recorded that the call was gated, and a
`✓ permission allow (config)` line on every approved call (printed *after* the
result, reading as if config auto-allowed it) was pure noise. A **deny** keeps
a `✗ permission denied` line, because a blocked call changed what happened. The
old rule-source parenthetical is dropped either way.

The fuller pipeline trace (which rail matched, what the sandbox said, what the
reviewer decided) and the richer action set (`edit cmd`, `why?`) land later; M3
ships the inline allow/deny/remember prompt above.

**Remember-as-rule** — a grant never widens silently: the prompt offers
exact / prefix / broad patterns, but dangerous commands are force-downgraded
to exact-match regardless, scoped (agent/global) and TTL'd.

## Roster & navigation (M2)

The `Overview` screen is the concrete M2 roster. Like `Model`, it is a pure
value — every method returns an updated copy, so a fixed input sequence renders
identically in every golden test — and it consumes the daemon through the
consumer-side `Supervisor` interface (`supervisor.go`), never a privileged
path.

Layout, top to bottom:

- **Header** — app name + version, then `model · cwd`, then a status-count line
  `N awaiting input · M working · K completed`. The counts are the roster
  tallied by status; the wording mirrors the group labels.
- **Roster body** — one line per session:
  `‹caret› ‹status glyph› ‹title› ‹one-line summary› ‹cost · age›`. The caret
  (`▸`) marks selection so it reads without color (golden tests force
  `termenv.Ascii`). The status glyph promotes to `✋` when approvals are
  pending. Cost comes from the SDK's usage accounting (PRD: cost in every
  roster row); age is a compact relative string (`now`/`5m`/`3h`/`2d`) computed
  against an injected reference time so tests stay deterministic. The body
  windows to keep the selected row visible.
- **Dispatch bar** — a rule, an input line (a placeholder until the user
  types), and a one-line shortcut hint. Typing anywhere in the roster edits the
  bar; `enter` on a non-empty bar creates a new session from that text and
  attaches into it (`Supervisor.Create`).

**Two roster views**, toggled by `tab`: flat (every session, most-recently-active
first) and grouped (Working / Needs input / Finished sections, each
recency-sorted). Selection is tracked by session id, not row index, so it
survives the reorder a toggle causes. (`tab` rather than a letter key so the
dispatch bar stays freely typeable — a plain `v` is text, not a shortcut.)

**Peek** is the read-only split: the roster rail (the overview's header + body,
no dispatch bar) alongside a live tail of the selected session's transcript. It
steals no input — `j`/`k` move the rail selection and the app root swaps the
tail to the newly selected session. The panes stack vertically (roster above
tail) by default and split side-by-side once the terminal reaches
`layout.PeekHorizontalMinWidth` (120 cols) — below that a horizontal split
leaves each pane too narrow for a roster row. The `layout` package owns the
geometry (orientation, pane-size division, column zipping) as pure int/string
math so both arrangements stay golden-testable.

**Navigation contract** — enforced by the app root (`App` in `app.go`, the
bubbletea root that composes overview/peek/attach): `enter` peeks the selected
session (with dispatch-bar text, it instead creates a session from that text
and attaches into it); `→` attaches the selected session; `esc`
interrupts/acts on the *active* session (never "go back"); `←` in an **empty**
input backs out to the overview (with text, it edits); `ctrl-x` kills a running
session or archives a finished one; `ctrl-c` quits. In peek, `j`/`k` switch the
peeked session and `←` returns to the overview.

The app root is a **client** like any other (repo invariant): it reads the
roster by polling `Supervisor.Roster` on a timer (the supervisor's roster is
pull-based) and drives one live `event.Subscription` at a time for the
peeked/attached session, issuing the same create/send/interrupt/kill/archive
Ops an ACP client would. Switching sessions closes the old subscription and
stale events (tagged with a since-left session id) are dropped. A thin adapter
now bridges the concrete daemon supervisor to this `Supervisor` interface.

These patterns are adapted from Claude Code's agent-roster and collapsed
tool-block rendering — a status-count header, grouped sections, a one-line
session row, and a bottom dispatch bar with a hint line — reimplemented here
for gofer's Event/Op model.

**Tool blocks** in the attach/peek transcript render as a collapsed tree: a
header line `‹glyph› tool(command)`, then the result tree-indented beneath — the
first line on a `└`, up to two more indented, and any remainder collapsed to
`… +N lines`. The header command is the **authoritative** input from
`ToolCallFinished.Input`, not `ToolCallStarted.Input` (which is only the
start-of-block seed — an empty `{}` when a provider streams the arguments as
`input_json_delta` fragments, so building the header from it rendered every call
as `bash({})`). A command-shaped input is summarized to its own text
(`bash(find . -type f | wc -l)` rather than `bash({"command":"…"})`); unknown
tool shapes fall back to compact JSON. While a call is still running its input is
usually just the empty seed, so the header shows the **bare tool name** (`◐ bash`)
until the real command lands on finish. `ToolCallDelta` is ignored — it carries
input fragments, not result text (it used to be mis-appended to the result).

A **failed** call (`ToolCallFinished.IsError`) is styled distinctly: the ✗ glyph
and header render in the warn accent — deliberately softer than the red a fatal
`SessionError` uses — and the result body is dimmed, so an internal/transient
error (e.g. `sandbox: … command is required`) reads as a de-emphasized
diagnostic rather than prominent, genuine-looking output. A clean call keeps the
ok glyph and an unstyled body.

Transcript blocks are separated by a blank line (`transcriptGap`) for vertical
rhythm — user turn, assistant reply, and tool blocks each get breathing room.
Because a tool item spans several lines and the gaps are ordinary lines, the
three transcript renderers (`View`, `TailView`, `FullTranscript`) share one
`transcriptLines` helper that flattens every item to its lines — with the gaps
between — before width-truncating and height-windowing.

## Two trees, one renderer

The **fan-out tree** (subagents within a session — who is working) and the
**fork tree** (`/tree` — one conversation's branch history: forks,
compaction entries, HEAD) share a single row renderer. Fork/branch/compact
are first-class: the session is an append-only tree and context is
fold(root→head), so a "what if" fork costs nothing.

## Subagent sessions (M4)

A subagent is **not a black box within a turn** — it is a real child session
with its own journal, cost, and transcript, linked to its parent
(`session.spawned` event + `parent_id`; depth ≤ 5). The overview renders the
parent with its children indented beneath it, each child row carrying its own
description, run duration, and cumulative token/cost tally — the same
one-line-per-session shape as a top-level row:

```
● main
  ○ tui-inline-perm-owner   Own the M3 TUI change…      5m 9s · ↓ 214.7k tokens
  ○ sandbox-shell-fix-owner Own the M3 sandbox fix…      5m 30s · ↓ 185.3k tokens
  ○ go-developer            Editing model.go doc comment 6m 47s · ↓ 128.0k tokens
  ↑/↓ to select · enter to view
```

`↑`/`↓` selects a child; `enter` navigates *into* that child's full session —
its complete transcript, tool blocks, and approvals — exactly as if it were a
top-level session (`esc`/`←` returns to the parent). So a supervisor watching
one task drills into any subagent's whole history without losing the parent
context, and an approval waiting deep in the tree still surfaces as `✋ N` on
the ancestor row. This is the fan-out tree above made navigable: the tree shows
*who is working*; entering a node shows *what they did*. It reuses the shared
row renderer and the id-tracked selection/windowing the M2 roster already
established — a child session is just a session, so no new navigation model is
needed, only the parent→child link and the indent.

## Responsive layout

The root layout picks **compact stack** (< ~90 cols: one screen at a time)
or **split** (≥ ~90: persistent roster rail + detail pane) by breakpoint,
config-pinnable via `tui.layout: auto|compact|split`. Components only
implement `View(w, h)` and reflow — they never know which layout they're in.
In split mode, rail selection drives the detail pane (read along without
attaching); `f` promotes the pane to fullscreen; focus moves between panes.

## Package layout & contracts

```
tui/
  app.go        root tea.Model: screen stack + dialog stack + global keys
  screens/      overview · peek · attach     (navigation = stack depth)
  components/   roster · transcript · toolblock · approval · sessiontree
                palette · editor · statusbar · toast
  theme/        Theme struct (~20 tokens) + Capabilities (colorprofile)
  keymap/       central registry w/ user overrides + scoped tables + help
  layout/       rect/center/size-diff helpers
  testkit/      golden harness: fixed sizes · forced Ascii · theme.Test()
```

- **Typed component contract**: `Component[T]{Init; Update(msg) (T, Cmd);
  View(w, h)}` — generics keep children concretely typed below the root. A
  polymorphic `tea.Model` root degenerates into a god-object of concrete
  pointer casts; this is the failure mode the contract exists to prevent.
- **Capability interfaces** opted into per component: `Focusable`, `Helper`,
  `Sizeable` (later `MouseClickable`).
- **Theme**: ~20 semantic tokens (bg/panel/ink×3/accent/ok/warn/danger/info
  + state glyphs `○⚙◐✋✓✗` + spacing). Area styles are *functions of tokens*,
  not pre-baked struct fields. Detect the color profile once and let
  lipgloss downsample — no hand-kept per-profile palettes.
- **Keymap**: one central registry with user overrides + conflict detection;
  scoped binding tables instead of imperative mode branching; status-bar
  help composed from the focused component's `ShortHelp()`.
- **Dispatch precedence as a rule**: dialog stack > active pane/screen >
  global keys.

## Load-bearing patterns (design in from day one)

- **Virtualized transcript with a frozen-item cache**: items expose
  `Version()` + `Finished()`; finished items render from cache verbatim,
  only the streaming tail re-renders.
- **Streaming-markdown stable-prefix cache**: render the settled prefix
  once; re-render only past the last safe markdown boundary.
- **Dialog grace-period absorption**: async-opened dialogs swallow in-flight
  keystrokes (200ms-quiet / 1500ms-max window) — the approval-pops-mid-
  keystroke race.
- **Editor internals**: flat cursor model, grapheme-aware word-wrap
  (CJK-correct), kill-ring, snapshot undo. Autocomplete renders in-flow
  below the editor, not as an absolute overlay.
- **Per-tool-kind `ToolRenderer`** interface + width-aware diff view (split
  ≥ 140 cols, else unified).
- Deferred: mouse, animations beyond one shared spinner, a second theme,
  raw-ANSI subprocess remapping.

## How the TUI is tested

Three layers, each catching what the one below can't:

1. **Ascii goldens = structure.** `testkit` renders a `Model` at a fixed size
   through `theme.Test()` (forced `termenv.Ascii`, so lipgloss emits no color
   codes) and diffs byte-for-byte against a checked-in `testdata/*.golden`.
   This locks the *layout* — line breaks, glyphs, spacing, truncation — free of
   any per-machine color nondeterminism. Regenerate:
   `go test ./internal/tui/... -run TestGolden -update`, then **review the
   diff** (a golden is a committed assertion, not a cache). A transcript golden
   also lives in `internal/daemonbridge` (history-replay render) — regenerate
   it the same way with its own `-update`.
2. **`ansi.Strip(colored) == plain` = ANSI-width.** An Ascii golden can't see a
   color code, so it can't catch a styling bug that changes *display width*
   (the #61 color-scatter: a styled pane measured wider than its cells and tore
   the layout). The color tests (`color_layout_test.go`, `dialog_color_test.go`)
   render the same component twice — once plain, once through a real color
   profile (`colorTheme()`) — and assert that stripping ANSI from the colored
   render reproduces the plain one exactly, and that no line exceeds its width.
   Every render change here ships with both a golden and a colored width test.
3. **VHS = visual/pixel.** Goldens and width tests both run on plain text; they
   can't tell you whether the amber actually reads as caution or the spacing
   looks right. VHS renders real frames to GIF/PNG for a human eye (below).

The pyramid: goldens catch structure regressions cheaply on every run; the
colored tests catch the ANSI-width class the goldens are blind to; VHS is the
on-demand visual check for the pixels neither can assert.

## Visual capture with VHS

The Ascii golden tests are the authoritative assertion, but they render
`termenv.Ascii` — they can't show color, and by construction miss ANSI-width
bugs (the #61 color-scatter regression shipped past green goldens). For a
human-eye check of real rendered frames, `vhs/` holds on-demand
[charmbracelet VHS](https://github.com/charmbracelet/vhs) tooling:

- `vhs/harness/` — a tiny `main` that drives the **real** `internal/tui` render
  path (`theme.Default`, live `tui.Program`, `Program.Send`) through a fixed,
  scripted event stream, exactly as `cmd/gofer`'s `driveTUI` forwards a
  session. Pick a scene with `-scenario tool-call | approval`.
- `vhs/tool-call.tape` — a clean turn with a bash tool call (real command in the
  header, block rhythm). `vhs/approval.tape` — a turn ending in the inline
  permission prompt, with a failed call's softened error styling above it.

Run `scripts/tui-vhs.sh [tool-call|approval]` (no arg = all). It prebuilds
`vhs/.bin/harness`, then renders each tape to `vhs/out/` (GIF of the whole turn
+ PNG of the key frame); both are gitignored. If VHS isn't installed the script
prints an install hint and exits. This is **not** a CI gate — VHS complements,
never replaces, the golden tests.

## Slash commands

`/` is reserved for commands; file mentions are `@` (fuzzy path completion).
ONE registry powers the palette, slash parsing, and keybindings. Collision
order: extension commands > markdown templates > builtins.

- **P0**: user markdown commands (`~/.gofer/commands` + project
  `.gofer/commands`, with `$1`, `$ARGUMENTS`, `${1:-def}`, `${@:N}`
  substitution + frontmatter description/argument-hint) · `/model [id]` ·
  `/new` · `/quit` · `/resume` (picker) · `/compact [instructions]`
  (block-if-busy) · `/yolo` permission-mode toggle (dual-bound command +
  key; ships before autonomous tool use) · `/help` rendered from the live
  keymap · `!` / `!!` shell escape (`!!` runs but excludes output from model
  context) · `@`-file mention.
- **P1**: `/init` (first-run project context) · `/fork` · `/tree` ·
  `/export html|jsonl` · `/login` · `/thinking` (toggle vs effort-picker by
  model capability) · runtime `registerCommand` from plugins ·
  `/skill:name` · `/name` · `/session` (id, path, per-model tokens/cost).
- **P2**: model-cycling key · `/mcp` management · `/debug` (hidden commands
  share the dispatcher, skip autocomplete).

## Plugin-contributed UI

Plugins run out of process; they can't ship Go components. v1 is a **small
declarative widget vocabulary**: the plugin sends a serialized view tree over
its existing JSON-RPC channel and the host walks it into a bubbletea
sub-model — data + structure, never code in our process.

- **MVP widgets** (single digits): `text · list · table · key_value · form ·
  tabs` + `tool_result_renderer` (the highest-value slot: a plugin claims
  rendering for a tool's structured output).
- **Fixed slots, not a free canvas**: `tool_result_renderer` (per
  tool/content-type) · `sidebar_panel` (one tab in a dedicated area, never
  displaces core) · `status_bar_segment` (append-only) · `slash_command`.
  Core chat/input/roster are never plugin-touchable.
- **Local echo, never per-keystroke RPC**: the generic renderer owns
  navigation; only committed semantic actions (item activated, form
  submitted) round-trip, one message per commit.
- **Conflict & lifecycle**: host-enforced namespacing
  (`<plugin-id>.<local>` at registration); slot conflicts → first-registered
  wins with a visible "renderer conflict" placeholder (never silent), plus
  an opt-in priority field; version negotiation degrades UI capability only
  (tools still load); **render budget** ~150ms per render RPC → "plugin
  unresponsive" placeholder, N consecutive timeouts disable the surface;
  capability-gated per surface (manifest declares, user approves once,
  cached, grantable per-surface).
- **Growth path (v2, on demand only)**: a WASM render surface (wazero,
  memory-sandboxed, structured `render(w,h)→buffer` — never raw
  ANSI-over-pipe). Rejected outright: in-process Go components and
  native-subprocess raw-pty capture.
