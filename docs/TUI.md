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
An approval block shows the full pipeline trace (which rail matched, what
the sandbox said, what the reviewer decided):

```
 ✋ approval ─ Bash(kubectl delete replica pvc-9f21-r-x7ab -n longhorn-system)
    rails: no rule → sandbox: n/a (cluster write) → reviewer: RISKY (irreversible)
    [y] once  [r] remember rule…  [e] edit cmd  [n] deny  [tab] why?
```

**Remember-as-rule** — a grant never widens silently: the dialog offers
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
stale events (tagged with a since-left session id) are dropped. It has no cmd
wiring yet — a thin adapter bridges the concrete daemon supervisor to this
`Supervisor` interface once both land on the integration branch.

These patterns are adapted from Claude Code's agent-roster and collapsed
tool-block rendering — a status-count header, grouped sections, a one-line
session row, and a bottom dispatch bar with a hint line — reimplemented here
for gofer's Event/Op model.

**Tool blocks** in the attach/peek transcript render as a collapsed tree: a
header line `‹glyph› tool(args)` (streaming glyph while the call runs, ok glyph
once done), then the result tree-indented beneath — the first line on a `└`,
up to two more indented, and any remainder collapsed to `… +N lines`. A failed
call reuses the ok glyph until the SDK carries an error flag on
`ToolCallFinished`. Because a tool item now spans several lines, the transcript
renderers flatten every item to its lines before width-truncating and
height-windowing.

## Two trees, one renderer

The **fan-out tree** (subagents within a session — who is working) and the
**fork tree** (`/tree` — one conversation's branch history: forks,
compaction entries, HEAD) share a single row renderer. Fork/branch/compact
are first-class: the session is an append-only tree and context is
fold(root→head), so a "what if" fork costs nothing.

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
