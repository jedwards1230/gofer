# gofer TUI — design

The TUI is a projection of the Event/Op contract like every other client.
Navigation is three altitudes — **overview ⇄ peek ⇄ attach** — modeled as a
screen stack. Golden-file tests come first: the `testkit` harness pins fixed
sizes, forces `termenv.Ascii`, and uses a test theme (see
[`TESTING.md`](TESTING.md)).

**Status**: `internal/tui` holds the M2+M3 TUI — the attach `Model` (transcript +
input, driven by `Model.Ingest`), the `Overview` roster screen, the `Peek`
split, collapsed tool-block rendering, the inline permission prompt (M3), and
the `App` screen-stack root that composes them under the navigation contract
(see [Roster & navigation](#roster--navigation-m2) below). M4 step 1 added the
slash dispatcher + command panel host (`command.go`, `panel.go`); M4 step 2
added the `CommandEnv` data seam (`env.go`) and the real `/status` view
(`status.go`); M4 step 3 added `config.Save`, the settings registry
(`settings.go`), and the real `/config` view (`config_view.go`); M4 step 4
added the real `/model` picker view (`modelpicker.go`) and its coupled
Enter/select action (`App.handleModelSelect`, panel.go); a follow-up added
the slash-command autocomplete popup (`command_menu.go`) — see [Slash
commands](#slash-commands) below. **M4 is done.** A later redesign pass put a
global identity header on every screen, reformatted the approval prompt,
hid the roster dispatch bar while a command panel is open, unified the
header into the same scrollable region as the attach transcript, and added
mouse-wheel/PgUp-PgDn scrolling — see [Bottom-anchored
layout](#roster--navigation-m2) and the approval-prompt example below. A
later fix pass closed a real streaming-attach bug (multi-line items breaking
the tail-follow height accounting, wheel scroll along with it) and added the
`tui.autoscroll` setting — see [Multi-line items and the height-accounting
invariant](#roster--navigation-m2). A follow-up pass replaced the append-only
input buffers with a cursor-aware one and native editing keymap, tuned wheel
scroll, and added app-owned click-drag text selection with OSC 52 copy plus
the `tui.mouse` escape hatch — see [Input editing](#input-editing) and
[Mouse: scroll + selection](#mouse-scroll--selection) below. Still ahead: a
general reusable dialog abstraction, the central keymap registry, and plugin
UI.

## The three altitudes

**Overview** — one row per session (a session = a task, titled by the work).
A row may be a whole fan-out hierarchy; it collapses to aggregate state,
agent count, and whether approvals are pending, and expands inline to the
subagent tree. `↑`/`↓` move the selection · `tab` switches view · `enter`
peek · `→` attach · `ctrl-x` kill (running; subtree interrupted) or archive
(finished), acting immediately on the selected row. `enter`, `→` and
`ctrl-x` take these meanings only while the dispatch bar is empty; every
other key types into it, and `enter` on non-empty text starts a new session
— or dispatches a `/` command. Journals are never deleted — `gofer ps --all`
lists archived sessions.

**Peek** — a summary card for one session: its title, a one-line
waiting/status line, and a `❯ reply` input. `up`/`down` move the roster
selection (the card follows); `enter` opens (attaches) or, with reply text,
sends the reply; `space` closes back to the overview; `ctrl+x` kills a
running session or archives a finished one, as on the overview. Peek
carries no transcript tail — it is a roster-only projection.

**Attach** — full transcript + input. `esc` interrupts the in-flight turn;
`←` on an empty input backs out to the overview (with text, it moves the
cursor).

Every screen — overview, attach's transcript, its approval prompts, and its
command-menu/panel overlays — opens with the same two-line identity header:
`gofer v<version>` then `<model> · <cwd>` (`identityHeaderLines`,
overview_render.go; `attachHeaderLines`, app.go). The overview's own header
adds a third status-count line beneath it; the attach screen's copy leaves
that row blank instead (a global roster tally means nothing once attached to
one session).

A pending permission request is **not** a centered modal — it renders inline in
the conversation's bottom UI. The transcript records a permanent `● <tool>`
badge the moment the request arrives, but while it's unresolved the live
prompt **commandeers the whole footer** (status line, input box, and its
framing rules) and the badge is suppressed from the transcript so it isn't
shown twice; once answered, the footer returns and the badge becomes visible
again. It reads as a confirm prompt — a rule, an attributed `<tool> command`
header, the call's own description and body, a plain-English rationale, the
question, and the action row, keyed `a`/`d`/`r` (`r` toggles remember, `1`/`2`
alias allow/deny), `esc` dismisses without answering (the request stays
pending; a re-attach re-surfaces it):

```
 ────────────────────────────────────────────────
 bash command · from the `researcher` agent
   Run the test suite with race detection

   go test -race ./... \
     -run TestApproval
   timeout=120

 Why you're being asked

   No permission rule matched this call, so gofer is asking before it runs.
   It also cannot be sandboxed on this host, so an allow rule alone will not
   let it run unattended.

   Policy: unmatched · containable: false (no container configured)

   Press `r` before allowing to remember this exact call for the rest of the
   session. Add a rule to the `permissions` array in `config.json` — e.g.
   `{"verdict": "allow", "tool": "bash", "specifier": "go *"}` — to stop
   being asked.

 Do you want to proceed?
   1. [a] Yes   2. [d] No   ·   [r] remember: off

 esc cancel · session 0192a1b2-…
```

The header's attribution clause is omitted entirely for an un-attributed call
(no subagent, or a stream that never carried one) — never a placeholder. The
body is the call's own `command`/`cmd`/`script`/`file_path`/`path` value,
rendered over as many rows as it needs with every other spec key demoted to a
sorted `k=v` list beneath it; the whole body is capped at
`tui.approval_body_lines` rows (default 12) with the remainder collapsed into
`… +N more lines`, so a pasted script can never push the question off the
frame.

Resolution is deliberately quiet. A routine **allow** adds *no* transcript
line — the `●` badge already recorded that the call was gated, and a
`permission allow` line on every approved call (printed *after* the result,
reading as if config auto-allowed it) was pure noise. A **deny** keeps a red
`permission deny` line, because a blocked call changed what happened. The old
rule-source parenthetical is dropped either way.

**Richer provenance — what ships.** The prompt now carries the call's
**attribution** ("from the `<agent>` agent", correlated from the tool call's
`event.Agent` — `event.PermissionRequested.ID` *is* the tool call id), its
**multi-line body** (the real command text, not a one-line `cmd=…` summary),
and a **rationale derived from the guard's decision trace**
(`event.PermissionRequested.Trace`): why it was gated in plain English, the
matched policy with every raw trace entry preserved, and the two escape
hatches that actually exist — `r` to remember the call for the session, or a
rule in `config.json`'s `permissions` array (the example specifier is built
from the call's own first token, and is omitted rather than guessed at when
there is no command body).

**Richer provenance — what remains backlog.** The **gating hook** that raised
the request (e.g. `PreToolUse:Bash`) and a copy-paste **override hint**
carrying its `[plugin:x]` provenance both need fields the permission request
doesn't carry yet. Two affordances ride the action row — `Tab` to amend the
call before allowing, `ctrl+e` to explain why it was gated — but both need new
SDK permission-outcome variants (an amended-input reply and an explain
request) before the TUI can offer them; see the agent-sdk-go design backlog.
Neither key is advertised on the prompt until its implementation lands. Once
`session/explain_permission` exists it supersedes the locally-derived
rationale above as the source for the "why".

**Remember-as-rule** — a grant never widens silently: the prompt offers
exact / prefix / broad patterns, but dangerous commands are force-downgraded
to exact-match regardless, scoped (agent/global) and TTL'd.

## Structured question / decision tool (M7, not yet built)

Design intent only. An agent that needs a **decision** — not a tool approval,
but "which of these should I do?" — deserves a first-class prompt distinct from
the permission dialog above. Like an approval it renders inline in the footer,
commandeering it while unresolved; unlike one it carries the agent's own
question and options rather than a tool call.

**Single question** — a title chip, the bold question, numbered options each
with a dim rationale sub-line, a free-text row to answer off-menu, and an escape
row that hands the turn back to the conversation:

```
 ────────────────────────────────────────────────
 decision   Pick a migration strategy

 Which approach should I take?

   1  In-place ALTER
        fastest, but locks the table for the duration
   2  Shadow table + backfill
        online, but doubles disk until cutover

   › Type something.
   ↳ Chat about this

 Enter to select · ↑/↓ to navigate · Esc to cancel
```

**Multi question** — a tabbed stepper strips across the top; `Tab` switches
between questions, each with its own option list, and a right-side reference box
shows the focused option's detail. `n` opens a notes field on that option:

```
 ────────────────────────────────────────────────
 ←   □ Q1    □ Q2    ✔ Submit   →

 Q1   Which database?

   1  Postgres           ┌─ reference ───────────────┐
   2  SQLite             │ Postgres: the focused      │
                         │ option's detail renders    │
   › Type something.     │ here.                      │
   ↳ Chat about this     └────────────────────────────┘
                                press n to add notes

 Enter to select · ↑/↓ to navigate · n to add notes ·
 Tab to switch questions · Esc to cancel
```

Both forms need a **structured-question ACP message type** the daemon can relay
and the client can render — a decision request distinct from
`permission.requested`, carrying the question(s), their options, and per-option
rationale/reference — so this stays deferred until that lands; see the
agent-sdk-go design backlog.

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
  `‹caret› ‹title› ‹status word · one-line summary› ‹age›`. The caret (`▸`)
  marks selection so it reads without color (golden tests force
  `termenv.Ascii`). There is no status glyph — state rides the **color of the
  status word**: yellow while working or awaiting input, green once finished. A
  pending approval simply reclassifies the row to `Needs input` (no count — one
  or many pending reads the same). The status word (`Working` / `Needs input` /
  `Finished`) prefixes the summary in the flat view, where no status section
  states it; the grouped view omits it from the row and colors the section
  header instead.
  Age is a compact relative string (`now`/`5m`/`3h`/`2d`) computed against an
  injected reference time so tests stay deterministic, right-aligned as the
  sole right-column metadata. The body windows to keep the selected row
  visible.
- **Dispatch bar** — a rule, an input line (a placeholder until the user
  types), and a one-line shortcut hint. Typing anywhere in the roster edits the
  bar; `enter` on a non-empty bar creates a new session from that text and
  attaches into it (`Supervisor.Create`).

**Two roster views**, toggled by `tab`: flat (every session, most-recently-active
first, grouped under a **cwd header** per working directory) and grouped
(Working / Needs input / Finished sections, each recency-sorted). The cwd
header makes the fleet-global working directory visible — one header per
distinct cwd, sessions beneath. Selection is tracked by session id, not row
index, so it survives the reorder a toggle causes. (`tab` rather than a letter
key so the dispatch bar stays freely typeable — a plain `v` is text, not a
shortcut.)

**Peek** is the roster rail (the overview's header + body, no dispatch bar)
above a **summary card** for the selected session: a rule, the session title, a
`‹verb› ‹duration›` waiting line (`waiting`/`working`/`finished` since last
activity), a `❯ reply` input, and a footer hint. Peek subscribes to no event
stream — the card is a pure projection of the roster snapshot plus the reply
buffer, so moving the selection never re-subscribes. (This replaces the earlier
read-along transcript tail and its side-by-side split; the `layout` package now
holds only frame padding.)

**Navigation contract** — enforced by the app root (`App` in `app.go`, the
bubbletea root that composes overview/peek/attach): `enter` peeks the selected
session (with dispatch-bar text, it instead creates a session from that text
and attaches into it); `→` in an **empty** dispatch bar attaches the selected
session (with text, it edits); `esc`
interrupts/acts on the *active* session (never "go back"); `←` in an **empty**
input backs out to the overview (with text, it edits); `ctrl-x` kills a running
session or archives a finished one; `ctrl-c` quits. In peek, `up`/`down` move
the selection, `enter` opens the session (or sends the reply when the `❯` input
has text), `space` closes to the overview, and `ctrl+x` deletes.

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

**Bottom-anchored layout, scroll-away header** (chat-style, like Claude
Code): on overview and attach, the input block — the autocomplete menu when
open, the input's framing rules, the `>`/`❯` line, and the status/usage
footer — is pinned to the terminal's last rows; everything above it is one
scrollable region. On attach that region is the identity header **plus** the
transcript (`Model.view` joins `attachHeaderLines` to `transcriptLines`
before windowing) — a short conversation leaves the header pinned at the top
with blank filler below it, exactly as before, but a transcript long enough
to overflow the viewport scrolls the header off the top along with the
oldest messages, tailing to the latest by default. `Overview.render` pads
its own header (unaffected — the overview's header stays fixed; only its
roster rows scroll) plus roster rows with blank filler up to the height it's
handed before appending the pinned dispatch block, so a short roster leaves
blank rows *above* the input instead of trailing directly beneath it.

**Scroll**: a mouse wheel (`tea.MouseWheelMsg`, enabled via `View().MouseMode
= tea.MouseModeCellMotion` — bubbletea v2 moved mouse mode off
`tea.NewProgram` and onto the View) or `PgUp`/`PgDn` moves `App.scroll` — 0 is
the default, tail-to-latest; wheel-up/`PgUp` scroll back into history,
wheel-down/`PgDn` scroll toward the tail, floored at 0. On overview it
overrides the roster's selection-anchored windowing while active; on attach
it scrolls the header+transcript region described above. Both go through the
shared `scrollTail` primitive, which clamps the offset to the content's
actual length so an oversized offset (or a zero/negative viewport — the #87
class of underflow) can never slice out of range. `App.scroll` resets to 0
whenever the screen or the attached/peeked session changes, so navigating
away and back always lands back at the tail.

**Multi-line items and the height-accounting invariant**: `Model.view`'s
avail/scrollTail/pad math assumes `transcriptLines`' returned slice LENGTH
equals the transcript's actual terminal row count — one slice entry, one
row. A streamed assistant reply (or a pasted multi-line user prompt, or a
multi-line tool command) is virtually always more than one physical line —
paragraphs, lists, code blocks — so `renderItemLines` splits each item's text
on embedded `"\n"` into one display-line entry per physical line
(`styledMarkerLines`, model.go), continuation lines indented to align under
the marker glyph rather than repeating it. Leaving a raw `"\n"` inside a
single slice entry instead (the pre-fix shape) undercounts the item's real
height: avail/scrollTail never clip it, so it silently overflows past the
bottom of the frame while the header/oldest messages stay wrongly pinned in
view — the streaming top-anchor bug (`internal/tui/streaming_test.go`
reproduces it end to end via incremental `MessageStarted`/`MessageDelta`
events, the same shape a live daemon attach streams, before asserting the
fix).

**`tui.autoscroll`** (settings.go, default true/unset) controls whether new
streaming events pull the attach view down toward the tail: enabled (the
default) behaves exactly as scroll always has — offset 0 always renders the
current tail, so growing content keeps the latest message in view. Disabled,
`App.ingestAttach` bumps `App.scroll` by however many transcript lines the
just-ingested event added, keeping the *absolute* window of visible content
fixed (same start/end line indices) rather than sliding toward the tail —
"manual", the operator moves it themselves with the wheel/PgUp/PgDn. Read
live off `CommandEnv.Config()` on every streamed event, not cached, the same
"always current" contract every other `CommandEnv` read follows.

## Mouse: scroll + selection

Cell-motion mouse reporting (1002) plus SGR extended coordinates (1006) is
the minimal enable pair bubbletea v2.0.8 offers — there is no wheel-only
mode, only `MouseModeNone`/`MouseModeCellMotion`/`MouseModeAllMotion` — so
turning on wheel scroll also captures every click/drag/release the terminal
would otherwise hand to its own native selection. Rather than accept that as
a tradeoff, the app **owns** selection instead: `mouse.go` tracks a
`selectionState` (a screen-cell region, absolute terminal row/column
coordinates — the same space `App.render`'s own output uses) from
`tea.MouseClickMsg` (left button only) through `tea.MouseMotionMsg` (motion
while the left button stays held — cell-motion mode never reports it
otherwise) to `tea.MouseReleaseMsg`, on whichever of overview/attach is
showing (the same gate `handleWheel` uses; peek has no selectable content of
its own, and a command panel/menu/approval overlay composes *over* the
screen without stopping selection on it either, matching wheel scroll).

`App.render` overlays the selection's span as reverse video after every
other overlay, cutting each covered line via `ansi.Cut` into its
unselected-before/selected/unselected-after runs. The unselected runs keep
their original styling untouched; the selected run is stripped of whatever
ANSI it already carries (`ansi.Strip`) before the reverse-video style wraps
it, so the highlight is a solid, uniform block immune to a reset embedded
inside the run — a transcript row built from more than one styled
sub-render (a marker glyph's own color, reset right before the text that
follows it) would otherwise nest that reset inside the reverse wrap and
have it terminate the reverse video partway through the row instead of at
its end. Losing inner styling within the selection (a marker's glyph color)
in exchange for full-width, embedded-reset-proof reverse video is the
tradeoff. On release,
`App.selectedText` extracts the plain (ANSI-stripped) text the span covers
straight out of `App.render`'s own output — the *same* fully composed frame
the terminal shows, so the scroll offset and the identity header are already
baked in with no separate coordinate space to translate between — and copies
it to the system clipboard via bubbletea's built-in OSC 52 support
(`tea.SetClipboard`, an `"\x1b]52;c;<base64>\x07"` sequence written straight
to the program's output; no external clipboard dependency). A multi-row span
takes the clicked line from its start column to the end, every full line in
between whole, and the released line from its own start through the released
column — the standard terminal click-drag shape. The selection stays
shown/copyable after release until the **next click** (which always installs
a fresh `selectionState`, clearing any previous one outright) or **any key
press** (`App.Update`'s `tea.KeyPressMsg` case drops `a.sel`); it does *not*
clear on scroll, so wheel/PgUp-PgDn during or after a selection is fine.

Both the highlight and the copy are clamped to `App.transcriptRegion` —
the active screen's own scrollable content, computed via the same
`frameLayout` row-budget arithmetic `render` uses (so the two can't drift
apart): the attach transcript (plus whatever of its identity header is
still scrolled into view) or the overview roster body. A drag that runs off
the transcript into the input box and its framing rules, off the bottom
into the usage/status footer, past the top into the identity header, or
over a command panel/menu never paints or copies those rows — a row the
clamped range still covers is painted/copied in full, not bounded by a
click/release column that itself landed outside the region.

**`tui.mouse`** (settings.go, default true/unset) is the escape hatch for a
terminal where OSC 52 or SGR mouse reporting misbehaves: off sets
`View().MouseMode = tea.MouseModeNone` instead of `tea.MouseModeCellMotion`,
handing mouse reporting back to the terminal entirely — its native
click-to-select and scrollback return — and every mouse-message case in
`Update` is also defensively gated on the same setting, so a message a
misbehaving terminal sends anyway (or one a non-terminal client synthesizes)
is a no-op too, not just uncaptured at the protocol level. Not every
terminal honors mouse reporting at all — macOS's stock Terminal.app in
particular sends no mouse events to the foreground program regardless of
what a TUI enables — so a wheel/selection that does nothing there is a
terminal limitation, not a gofer bug; a tmux/Zellij session also needs its
own `mouse on` setting to pass mouse events through to the program it
hosts.

## Input editing

The overview dispatch bar and the attach input (the two text-entry surfaces
the slash-command grammar covers — see [Slash commands](#slash-commands))
share `inputBuffer` (`inputbuf.go`): text plus a cursor index (a rune
offset), copy-on-write like every other TUI value. Before this it was
append-only (`TypeRune` appended, `Backspace` dropped the last rune, no
cursor at all); now every op — insertion, movement, deletion — applies at
the cursor, and the `▏` glyph renders at its real mid-text position
(`inputBuffer.Render`) instead of always at the end.

The keymap (`input_keymap.go`'s `applyInputKey`, shared by both surfaces) is
the standard readline/macOS set, bound to what bubbletea v2.0.8 actually
delivers: Option/Alt reaches the app as `tea.ModAlt` on terminals that
forward it (Ghostty does); Cmd/Super doesn't reliably reach a terminal
program at all, so Home/End and their Ctrl-A/Ctrl-E equivalents are the
dependable line-start/end bindings, not a Cmd pairing.

| Action | Keys |
|---|---|
| Move one char | `←`/`→` |
| Move one word | `Alt+←`/`Alt+→` |
| Move to line start/end | `Home`/`Ctrl-A`, `End`/`Ctrl-E` |
| Delete char before/at cursor | `Backspace`, `Delete`/`Ctrl-D` |
| Delete word before cursor | `Alt+Backspace`/`Ctrl-W` |
| Delete to line start/end | `Ctrl-U`, `Ctrl-K` |

Word movement/deletion only treats whitespace as a boundary — `foo.bar` is
one word, matching bash/zsh/readline's own Ctrl-W convention rather than an
editor's finer-grained punctuation-splitting. Each screen's one nav-contract
arrow is **conditional on its input being empty**, and only when unmodified
(`handleOverviewKey`/`handleAttachKey`'s own `key.Mod == 0` guards keep them
from colliding with the keymap's word-move bindings): a bare `→` on the
overview attaches the selected session from an empty dispatch bar and moves
the cursor right when it has text; a bare `←` on attach backs out to the
overview from an empty input and moves the cursor left when it has text. So
neither arrow is ever swallowed mid-edit — the cursor moves both ways on
both surfaces.

**Bracketed paste** (`paste.go`) arrives as a single `tea.PasteMsg` carrying
the whole clipboard payload — bubbletea enables bracketed paste by default —
and is inserted at the focused surface's cursor **outside the key handlers**.
That is the point: replayed as key presses, a pasted newline would submit
mid-paste and a pasted leading space would close peek. All three text-entry
surfaces take it (dispatch bar, attach input, and peek's `❯` reply, which is
a plain string and so appends); an open command panel or a pending approval
prompt owns the keyboard, so a paste there is a no-op exactly as a typed rune
is. CR/CRLF line endings normalize to `\n`, and the buffer keeps its real
newlines — the paste submits as pasted. Control characters are substituted
only at **render** time, with their one-cell Unicode Control Pictures glyph
(`␊`, `␉`, `␛`), because a literal newline inside a one-row input line breaks
the frame out of its height budget.

**`tui.max_paste_bytes`** (default 128 KiB, `0` = unlimited) caps one paste.
The input line is re-derived from the buffer string on every frame, so a
stray multi-megabyte paste makes each redraw allocate megabytes — and it is
unreadable in a one-line input anyway. An over-cap paste is clipped on a rune
boundary and reported on the status line, never dropped silently. It is a
`config.json` knob rather than a `/config` registry row: the registry's
string editor has no numeric validation affordance today.

`App.render` composes the autocomplete menu into the pinned input block
rather than budgeting for it separately — `Overview`/`Model`'s `*WithMenu`
variants already carve its rows out of their own height budget. The command
panel takes its own slice out of the bottom when open (unaffected — panel
and menu are mutually exclusive) and, on the overview, blanks the dispatch
bar's three rows in its place (`Overview.dispatch`'s `hide` parameter) — the
panel then owns the bottom of the screen, so the roster's own (un-typeable)
dispatch chrome doesn't render redundantly beneath it. `layout.TopPadding`
is unrelated: a fixed one-row workaround for a terminal that clips the
frame's first row, applied once in `App.render` on top of the bottom-anchored
frame.

**Tool blocks** in the attach transcript render as a collapsed tree: a
header line `‹marker› tool(command)`, then the result tree-indented beneath — the
first line on a `└`, up to two more indented, and any remainder collapsed to
`… +N lines`. The header command is the **authoritative** input from
`ToolCallFinished.Input`, not `ToolCallStarted.Input` (which is only the
start-of-block seed — an empty `{}` when a provider streams the arguments as
`input_json_delta` fragments, so building the header from it rendered every call
as `bash({})`). A command-shaped input is summarized to its own text
(`bash(find . -type f | wc -l)` rather than `bash({"command":"…"})`); unknown
tool shapes fall back to compact JSON. While a call is still running its input is
usually just the empty seed, so the header shows the **bare tool name** (yellow
`● bash`) until the real command lands on finish. `ToolCallDelta` is ignored —
it carries input fragments, not result text (it used to be mis-appended to the
result).

The marker carries the whole state: yellow while running, green once done,
**red** on a failed call (`ToolCallFinished.IsError`) — same red as a fatal
`SessionError`, since a failed tool call *is* an error, just a scoped one. Only
the marker is colored; the header text keeps its own styling. What sets a
failed call apart from a real `SessionError` is the **body**, not the header:
its result lines are dimmed, so an internal/transient error (e.g.
`sandbox: … command is required`) reads as a de-emphasized diagnostic rather
than prominent, genuine-looking output. A clean call's body is unstyled.

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

## Checkpoint / rewind + versioned changes (open design question)

Exploratory — not committed. gofer sessions are **already event-sourced
JSONL**, which makes two directions cheap to reach for and worth designing
together:

- **Checkpoint / rewind.** Beyond trivial named scrollback anchors, a real
  checkpoint model — mark a point, keep working, then rewind the session and
  its context back to it — folded straight out of the append-only journal, in
  the spirit of Claude Code's "Rewind code (checkpoints)". The fork tree above
  already makes a "what if" branch free; a checkpoint is that same machinery
  pointed at *undo* rather than *explore*.
- **Versioned working-tree changes (jj-style).** A Jujutsu-style substrate
  where each change an agent makes to the working tree is a first-class,
  addressable diff — so an individual change within a session can be reverted or
  cherry-picked without unwinding everything after it, and a rewind of the
  conversation and a rewind of the code stay in step.

This **subsumes** the reference's lightweight "timeline label chips", which are
only named anchors: reversible checkpoints plus versioned changes are the
direction with the leverage, and named anchors fall out of them for free. Left
open — whether the change substrate is gofer-native atop the JSONL journal or
leans on a task/checkpoint seam from the SDK; see the agent-sdk-go design
backlog.

## Subagent sessions (M7, not yet built)

Design intent only — lands with M6's subagents-first-class work. A subagent
is **not a black box within a turn** — it is a real child session
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
context, and an approval waiting deep in the tree still surfaces as a
`Needs input` state on the ancestor row. This is the fan-out tree above made navigable: the tree shows
*who is working*; entering a node shows *what they did*. It reuses the shared
row renderer and the id-tracked selection/windowing the M2 roster already
established — a child session is just a session, so no new navigation model is
needed, only the parent→child link and the indent.

**Tool-call attribution (SDK-gated).** When a tool event carries an
originating-agent id, its transcript block should name the source —
`ToolName(args) · from the <agent> agent` — alongside the existing human
caption, so a transcript that interleaves a parent's and its subagents' calls
reads unambiguously. It falls back to the current un-attributed rendering when
the event carries no agent id, so it is purely additive; surfacing that id on
the tool event is an SDK change — see the agent-sdk-go design backlog.

## Monitor / background tasks (M8 — goal)

A first-class **goal**, not a non-goal: a long-running background task the
daemon can spawn and *persist* — keyed by a task id, surviving attach/detach
(and daemon restart) rather than dying with the turn or the client that started
it. In the transcript it reads as its own block —

```
● Monitor(deploy/rollout) → task 0192a1b2 · persistent
```

— and the live task surfaces in the roster/fleet view alongside sessions, so an
operator sees what is still running without attaching. It fits the "visible
artifacts over hidden state" tenet: the task is an on-disk, greppable thing, not
in-memory client state.

**Open — the persistence substrate.** Whether the task-id/persistence machinery
is a **task-handle seam from the SDK** or is built **gofer-native** atop
resumable sessions + the JSONL journal is deliberately left open (a monitor may
well *be* a session under the hood). Decide the SDK-vs-gofer boundary when the
work is scheduled — see the agent-sdk-go design backlog.

## Responsive layout

Not yet built — design intent only. Once it lands, the root layout picks
**compact stack** (< ~90 cols: one screen at a time) or **split** (≥ ~90:
persistent roster rail + detail pane) by breakpoint, config-pinnable via a
future `tui.layout: auto|compact|split` setting (deliberately **not** in the
M4 step 3 settings registry — no layout modes exist yet, so the knob would be
a no-op; see `settings.go`). Components only implement `View(w, h)` and
reflow — they never know which layout they're in. In split mode, rail
selection drives the detail pane (read along without attaching); `f` promotes
the pane to fullscreen; focus moves between panes.

## Status line & context bar (backlog)

**Post-turn activity summary.** The attach footer's status line today shows only
`usage=<in>/<out>` and cost (`Model.statusLine`, model.go) — the one thing that
surfaces nowhere else. It should *also* render a one-line human digest of what
the turn did — "Read 4 files, ran 2 shell commands, recalled 1 memory" —
aggregated app-side by tallying tool-call events off the same stream the
transcript already consumes. No new contract is needed: the substrate (the
per-turn tool-call events) already exists; this is a rendering that counts it.

**Configurable context bar (statusline-style).** A user-customizable bottom bar
composed of named **segments** — model, context-remaining (`Ctx: 359.2k`), git
branch, working-tree diff-stat (`(+6,-1)`), session state, token/cost — with the
segment **set, order, and format** all configurable, explicitly in the spirit of
Claude Code's `statusLine` setting: the user supplies a command or template and
the shell renders it, rather than gofer baking in a fixed bar. It wires into the
existing settings registry under `tui.*` (`settings.go`) like every other knob,
and **degrades to the current muted `model · cwd` line** when unconfigured, so
the default view is unchanged. Prefer this configurable model over a fixed
powerline-style bar — the point is that the operator decides what the bar says.

## Package layout & contracts

> Target structure, partially built. `layout/`, `theme/` and `testkit/` exist
> as packages today; `screens/`, `components/` and `keymap/` do not — those
> concerns currently live in flat files directly under `internal/tui/`
> (`app.go`, `command.go`, `config_view.go`, …). The contracts below describe
> the intended decomposition, not the present tree.

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
  + state markers `○●` + spacing). Area styles are *functions of tokens*,
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
- **Editor internals**: flat cursor model shipped (`inputBuffer`,
  inputbuf.go — see [Input editing](#input-editing)); grapheme-aware
  word-wrap (CJK-correct), a kill-ring, and snapshot undo are still ahead.
  Autocomplete renders in-flow below the editor, not as an absolute overlay.
- **Per-tool-kind `ToolRenderer`** interface + width-aware diff view (split
  ≥ 140 cols, else unified).
- Mouse-wheel scroll and app-owned click/drag text selection with OSC 52
  copy both shipped (see [Mouse: scroll + selection](#mouse-scroll--selection)).
- Deferred: transcript virtualization with a frozen-item cache (see the
  first bullet above — `Model.transcriptLines` is still O(items) per
  render, negligible at realistic transcript sizes but the thing to revisit
  if a genuinely massive transcript ever makes wheel scroll feel slow),
  animations beyond one shared spinner, a second theme, raw-ANSI subprocess
  remapping.
- **Non-goal (for now)**: voice input ("hold space to speak") — no analogue in
  gofer's model and no demand; revisit only if it resurfaces.

## How the TUI is tested

Four layers, each catching what the one below can't:

1. **Ascii goldens = structure.** `testkit` renders a `Model` at a fixed size
   through `theme.Test()` (forced `termenv.Ascii`, so lipgloss emits no color
   codes) and diffs byte-for-byte against a checked-in `testdata/*.golden`.
   This locks the *layout* — line breaks, markers, spacing, truncation — free
   of any per-machine color nondeterminism. Regenerate:
   `go test ./internal/tui/... -run TestGolden -update`, then **review the
   diff** (a golden is a committed assertion, not a cache). A transcript golden
   also lives in `internal/daemonbridge` (history-replay render) — regenerate
   it the same way with its own `-update`.
2. **Styled goldens = color state.** The marker vocabulary carries state only
   through color (running/done/failed are all the same `●`), so an Ascii
   golden can't tell them apart. `testkit.AssertGoldenStyled` renders the same
   component through `testkit.ColorTheme()` (a real color profile), translates
   the ANSI it emits into stable `<yellow>●</yellow>`-style tags keyed to the
   theme's semantic styles, and diffs that against a checked-in
   `testdata/*.styled.golden` — an unrecognized escape fails loudly rather than
   silently passing. Same `-update` flag as the Ascii goldens.
3. **`ansi.Strip(colored) == plain` = ANSI-width.** Neither golden layer above
   catches a styling bug that changes *display width* (the #61 color-scatter: a
   styled pane measured wider than its cells and tore the layout). The color
   tests (`color_layout_test.go`, `dialog_color_test.go`) render the same
   component twice — once plain, once through `testkit.ColorTheme()` — and
   assert that stripping ANSI from the colored render reproduces the plain one
   exactly, and that no line exceeds its width. Every render change here ships
   with both a golden and a colored width test.
4. **VHS = visual/pixel.** Goldens and width tests both run on plain text; they
   can't tell you whether the color actually reads as intended or the spacing
   looks right. VHS renders real frames to GIF/PNG for a human eye (below).

The pyramid: Ascii goldens catch structure regressions cheaply on every run;
styled goldens catch the color-state class Ascii is blind to; the colored
width tests catch the ANSI-width class neither golden layer asserts; VHS is
the on-demand visual check for the pixels none of them can.

## Visual capture with VHS

The Ascii golden tests are the authoritative assertion, but they render
`termenv.Ascii` — they can't show color, and by construction miss ANSI-width
bugs (the #61 color-scatter regression shipped past green goldens). For a
human-eye check of real rendered frames, `vhs/` holds on-demand
[charmbracelet VHS](https://github.com/charmbracelet/vhs) tooling:

- `vhs/harness/` — a tiny `main` that drives the **real** `internal/tui` render
  path, exactly as `cmd/gofer`'s `driveTUI` forwards a session (the
  `transcript-*` scenes) or as a real terminal's keystrokes drive the command
  panel (the `panel-*` scenes). Pick a scene with `-scenario <slug>`; every
  slug follows `<area>-<view>[-<state>]`, kebab-case.
- `transcript-tool-call` — a clean turn with a bash tool call (real command in
  the header, block rhythm). `transcript-approval` — a turn ending in the
  inline permission prompt, with a failed call's red error marker and dimmed
  body above it.
- `roster-overview` — the roster with mixed states, showing the status words
  in color (yellow working/awaiting vs green finished) — the state that now
  lives only in color.
- `panel-model-daemon-refresh` — the #162 before/after: a daemon-backed
  roster whose header adopts a new default model mid-run, captured as two
  screenshots of one continuous process (`-before` / `-after`).
- `panel-status-overview` — the command panel opened via `/status` with no
  session attached (Session rows read "—"). `panel-status` — the same tab
  attached to a session, showing real session identity and both provider auth
  kinds. `panel-config` — the Config tab's settings-registry list at gofer's
  own defaults. `panel-model` / `panel-model-empty` — the Model tab's picker
  with authenticated providers (populated list, ✓ active mark) vs zero
  providers (empty state, "/login" hint).

Run `scripts/tui-vhs.sh [slug...]` (no arg = all tapes). It prebuilds
`vhs/.bin/harness`, then renders each tape to `vhs/out/` (GIF of the whole turn
+ PNG of the key frame); both are gitignored. Pass `--snapshot` to also mirror
the PNG key-frames into the tracked `vhs/snapshots/` baseline (what CI commits;
see below). If VHS isn't installed the script prints an install hint and exits.
This is **not** a CI gate — VHS complements, never replaces, the golden tests.

### Committed baseline + per-PR image diffs

So TUI changes are reviewable as a native GitHub image diff without pulling the
branch, the PNG key-frames are **committed** — CI is the sole author, so every
frame comes from the same ubuntu-latest render environment:

- **Baseline on `main`** — `vhs/snapshots/*.png`, kept current by
  `.github/workflows/vhs-baseline.yml` (renders on any push to `main` that
  touches the TUI, a tape, the harness, or the renderer; lands via a bot PR
  merged on the spot, since the main ruleset requires changes through a PR).
  Seed it once with `gh workflow run vhs-baseline.yml`.
- **Per-PR captures** — `.github/workflows/vhs-capture.yml` renders on PRs and
  appends the frames to an append-only `vhs-captures-pr-<n>` branch, branched
  from `main` so `main...vhs-captures-pr-<n>` is a clean image-only diff. A
  single sticky PR comment indexes the renders (a "latest" diff link, a
  per-commit table with frame-change counts, and a collapsed preview of the
  latest frames served from the capture branch) — the PR branch itself stays
  free of image blobs. The branch is deleted when the PR closes
  (`vhs-capture-cleanup.yml`); fork PRs degrade to a `vhs-frames` artifact.

The shared render step (install VHS → render → sync `vhs/snapshots/`) lives in
the `.github/actions/render-vhs` composite action. A bit of pixel jitter
between renders is expected and tolerated — this lane is an advisory helper,
never a merge gate.

## Slash commands

`/` is reserved for commands; file mentions are `@` (fuzzy path completion).
ONE registry powers the palette, slash parsing, and keybindings. Collision
order: extension commands > markdown templates > builtins.

**Built (autocomplete)**: `command_menu.go` is the palette — a popup listing
`Registry.List()` (Name-sorted, `Hidden` excluded), composed above the
dispatch bar/attach input's rule in `App.render`. It opens whenever
`commandToken(buf, cursor)` finds an active command token at the cursor: a
`/` at buffer start or immediately preceded by whitespace, with no
whitespace between it and the cursor, prefix-matched (case-insensitive)
against every command's Name and Aliases. A `/` preceded by any other
character (`` `/x ``, `foo/bar`) is literal text — no popup. Rows scroll past
`commandMenuMaxRows` (6) with a muted "↑/↓ N more" affordance. While open,
`↓`/`↑` move the highlight ahead of the per-screen handlers (dispatch
precedence: panel > approval > menu > active screen > global); `Tab`
completes the highlighted Name into the buffer, appending a trailing space
when the command has an `ArgHint` (ready for an argument) or none otherwise
(ready to submit); `Enter` runs the highlighted command directly; `Esc`
closes the popup but keeps the typed text. Any other key (ordinary typing,
Backspace) falls through to the buffer as usual, and `App.syncMenu`
recomputes the popup from the edited buffer on the way back out.

**Built (M4 step 1)**: `command.go` holds `Command{Name, Aliases, Summary,
ArgHint, Hidden, Run}` and `Registry` (name/alias → `Command`). Both submit
paths — the overview dispatch bar and the attach input — parse a leading `/`
at Enter time (`/name arg…`, whitespace-split) and dispatch through the
registry instead of creating/sending a prompt; an unmatched name sets the
transient status line.

**The transient status line carries a severity, not just text** (#161). Every
write goes through `App.setStatus(sev, note)`, which sets the text and its
`statusSeverity` together so the two cannot drift, and `App.render` styles the
footer from it:

| Severity | Style | Means | Examples |
|---|---|---|---|
| `sevOK` | `OKStyle` (green) | unqualified success | *"Default model set to X."*, *"…the daemon adopted it."* |
| `sevWarn` | `WarnStyle` (yellow) | it worked, with a caveat | cross-provider pick, a pinned daemon, a clipped paste |
| `sevDanger` | `DangerStyle` (red) | the operation failed | `opDoneMsg`'s error path, config read/write errors, unknown command |

`opDoneMsg`'s error path is the **only** route to danger for an operation
result. The zero value is `sevDanger` on purpose — it is the pre-#161
behavior, so a write that forgets a severity degrades to the old rendering
rather than silently claiming success — and `App.clearStatus` resets it so a
stale color can never outlive the note it described.

Before this, *every* note rendered in `DangerStyle`, so a successful `/model`
change was painted the same red as an HTTP 400 and a user reasonably read the
success as a failure. The severities are pinned by **styled** goldens
(`testkit.AssertGoldenStyled` / `testkit.TagANSI` under
`testkit.ColorTheme`), never plain Ascii ones: `theme.Test` forces
`termenv.Ascii`, which emits no escapes at all, so an Ascii golden is blind to
color by construction and would assert nothing here.

`panel.go` holds the command panel: a bottom overlay
(`App.panel`, nil = closed) composed over whichever screen `App` is showing,
routed with the same precedence as the approval overlay — `panel > approval >
active screen > global` — and closed by Esc, sized to whatever the active
tab's body actually renders (`commandPanel.Height`) rather than always a
worst-case max. Five builtins register now and open the panel on their tab —
the M4 trio (`/status`, `/config`, `/model`) plus the M5 read-only pair
(`/usage`, `/stats`); each opened on a one-line placeholder body until its own
step landed the real view (`/status` in step 2, `/config` in step 3, `/model`
in step 4, `/usage` + `/stats` in the M5 usage-panels step — see below). `@`
and `!` are not implemented — the intercept only switches on a leading `/` so
they can slot in later.

**Built (M4 step 2)**: `env.go` adds `CommandEnv` — the panel's read-only
data seam: `Version`/`Cwd`/`Root` plus `Auth`/`Config` closures wrapping the
SDK auth store and gofer's config loader. `cmd/gofer` builds one per process
(`buildCommandEnv`, `cmd/gofer/tui_app.go`) from the resolved store root and
passes it to `tui.NewApp`; `App` hands it to the panel at open time
(`command.go`'s `openPanel`), and every read happens lazily on render — never
a cached snapshot — so a `/login` elsewhere or an edited `config.json` shows
up the next time the panel opens. `status.go` is the real `/status` body: a
pure `statusView{env, sess}` rendering version/cwd, session identity (from
whichever session is peeked/attached, `App.currentSessionInfo`), one row per
authenticated provider (never a singular login/org/email block — gofer is
multi-provider; "not signed in" when none), the resolved model, and which
config layers exist on disk — omitting any row it can't answer honestly
rather than blank-filling it. Opens cleanly with zero providers
authenticated and never resolves a credential (auth-independence).

**Built (M4 step 3)**: `internal/config` adds `Session`/`TUI` config sections
(`session.model`, `session.permission_mode`, `tui.roster_view`,
`tui.autoscroll`, alongside the existing `telemetry.*`) and `Save(path,
Config)` — indented JSON, mode 0600,
atomic (temp file + rename). `settings.go` adds the setting registry: a
`[]Setting{Key, Label, Kind, Options, Get(Config), Set(Config, val) Config}`
table parallel to the command registry, namespaced (`session.*`, `tui.*`,
`telemetry.*`, and — once plugin loading lands in M7 — `plugin.<name>.*`
without a schema change) so adding a setting is one row; `Kind` picks the edit
affordance (bool/enum/string). `config_view.go` is the real `/config` body: a
search list (`Search settings…` filter box, `Label … value` rows) where ↓/Enter
select a row and edit it in place by kind — a bool toggles, an enum cycles, a
string opens an inline edit line — and a commit calls `env.SaveConfig`
immediately, no separate save step. Esc is two-stage: it cancels an
in-progress edit or clears the filter before a second Esc closes the panel.
Pure local: reads/writes `config.json` only, no auth path at all.

**Built (M4 step 4)**: `modelpicker.go` is the real `/model` body: the SDK's
static catalog (`provider.Models()`/`provider.Lookup`) filtered to the
providers `CommandEnv.Auth()` reports authenticated (the same seam
`status.go` reads — no new credential path), grouped by provider, ✓-marking
the active model (the attached session's override, else the persisted
`session.model` config default, else the resolved roster default) with a
one-line context-window/pricing description through a small gofer-side
display-name table (`modelDisplayNames`) that falls back to the raw id. Zero
providers authenticated renders an empty list plus a `/login` warning line
instead of blocking the picker from opening (§4c/auth-independence). ↓/↑
move the row highlight.

The list is compiled in, so it is only ever as new as the binary. Since the
SDK stopped treating its registry as an admission gate — `provider.Resolve`
runs an unregistered id by inferring its backend from the id's shape — the
picker carries a **free-text entry line** as the escape hatch: type any model
id and Enter commits it, listed or not, with no network call and no cache.
Typing drops the row highlight (the typed id is what Enter commits; ↓ back
onto a row hands the commit back to the row), Backspace edits, and Esc is
two-stage like the Config tab's — the first clears a half-typed id, a second
closes the panel. Typed ids are not added to the list: the list stays "what
this binary knows about", the entry is "what you can also ask for". The one
id the entry refuses is one no provider family matches — there is no adapter
to run it — and it renders the SDK's own reason in place of the candidate
line rather than failing silently on Enter.

An unregistered model has **no trustworthy metadata**: `ModelInfo`'s zero
`ContextWindow` and zero `Pricing` mean "unknown", explicitly not "no
context" and not "free". So each description segment renders from a known
value or as `context unknown` / `pricing unknown`, guarded on both the
`Unregistered` flag and a zero field value, so neither an inferred record nor
an incomplete registry row can put a fabricated price or limit on screen as
fact.

**`/model <id>` skips the picker entirely** (`runModel`, command.go — issue
#165). `/model` declares `ArgHint: "[id]"`, which the autocomplete popup
renders and appends a trailing space for, so the UI actively invites the
argument; before this it was wired to the args-discarding `openPanel` and
threw the id away without a word. With an argument the command now applies
that id directly and never opens the panel, routing through the *same*
`App.applyModelSelection` the picker's Enter uses — one config write, one
header refresh, one daemon probe, one set of status notes, shared by both
paths rather than reimplemented per path. Bare `/model` still opens the
picker, unchanged.

Admission for the string form is `provider.Resolve` **alone** — deliberately
the same rule as the free-text entry line above, so the two ways of typing an
id cannot disagree. An id Resolve routes but the compiled-in catalog does not
list applies: the catalog is a vendor listing that goes stale, comes back
empty, or is unreachable offline, and gating on it would break the string
override in precisely the situations it exists for. What Resolve *rejects*
gets a `sevDanger` note naming the id, never a silently-opened picker. Since
`parseSlash` splits on whitespace and no model id contains a space, `/model a
b` is rejected by argument count rather than silently applying `a`.

The declared-`ArgHint`-but-discarded-args defect is guarded **generally**, not
for `/model` specifically: `TestArgHintCommandsConsumeArgs`
(command_args_test.go) iterates `newBuiltinRegistry().List()` and fails any
command that advertises an argument yet behaves identically with and without
one, so a future command that repeats the mistake fails without anyone
extending the test.

**Enter couples the select** (`App.handleModelSelect`,
panel.go — the pure `modelPickerView` has no IO seam, so App intercepts Enter
one level up, ahead of `commandPanel.handleKey`, whenever the Model tab is
active). The selected id is always persisted as the `session.model` config
default via `env.SaveConfig` — the only side effect possible with zero
providers authenticated, keeping Enter auth-independent (§5). That persisted
default is now honored by model resolution itself (`resolveRunModel`), where
it outranks the credential-derived guess and is the supported way to settle
which of several logged-in providers gofer uses (see PRD, "Model
resolution"). When a session
is attached/peeked, App also decides — client-side, through `provider.Resolve`
(so a typed id the registry doesn't carry still resolves to its provider),
before ever calling the daemon — whether to hot-
swap it: same provider calls `Supervisor.SetModel` (the swap applies on the
session's next turn, not the one in flight); a cross-provider pick leaves the
running session on its model (a session's provider is fixed at creation) and
sets a status note instead: *"Live model swap needs the same provider —
default set for new sessions; this session keeps its model."* Either way,
Enter is a committing action: it closes the panel, leaving the outcome in the
transient status line.

**On a daemon-backed roster the outcome is confirmed, not guessed** (#162).
The header shows the DAEMON's default model, which this process cannot
recompute, so after a committed write App dispatches a `gofer/hello` re-probe
in a `tea.Cmd` (`App.probeDaemonDefaultCmd`, over
`CommandEnv.DaemonDefaultModel` — a network call, so never inline on the
Update loop) and folds the answer back in `App.applyDaemonDefault`:

| Daemon reports | Header | Note |
|---|---|---|
| the model just written | moves to it | *"…the attached daemon adopted it."* (ok) |
| a different model (started with `--model`, so pinned) | moves to the **pinned** model | *"…the attached daemon is pinned to another model."* (warn) |
| nothing — probe failed, or a daemon predating `gofer/hello` | unchanged | the hedged *"adopts it unless pinned"* wording stands |

The header previously updated only on the local backend and was otherwise a
startup snapshot, so a daemon-attached user saw no evidence at all that a
`/model` change had worked. It now updates **in the running process, with no
restart**. Every note above is a standalone string that fits the 80-column
floor and names no model id: the status line is width-truncated, and a caveat
truncated off the right edge leaves exactly the unqualified overclaim behind.

The same run also fixed the roster row: `session/new`'s response carries the
model the daemon ASSIGNED in ACP's reserved `_meta`, under the
gofer-namespaced key `gofer/model`, so `daemonbridge.Create` reports what the
session actually runs instead of echoing the (normally empty) requested model.

Effort-adjust (←/→) stays deferred (no SDK backing) —
and has no room on the Model tab regardless, since ←/→ are already claimed by
the panel host for tab switching. The concrete dependency is a runtime
`Runner.SetEffort` paralleling the `Runner.SetModel` that `Supervisor.SetModel`
already rides on; once the SDK grows it (see the agent-sdk-go design backlog)
the control becomes actionable — a persisted `session.effort` default plus a
same-session hot-swap on the same terms as the model — needing only a spot on
the tab that ←/→ don't already own.

**Built (M5 usage panels)**: `/usage` (usage.go) and `/stats` (stats.go) are two
more read-only tabs cut from the same cloth as `/status` — pure, stateless
views that omit any row the current data can't answer rather than blank-filling
it, needing no `handleKey`/`handleEscape` of their own (Esc just closes). They
split the token/money story in two: **`/usage` is where THIS session's tokens
and money went** — the accumulated `SessionInfo.Usage` (input / output /
cache-read / cache-write tokens) and the `SessionInfo.Cost` breakdown (USD total
plus the per-bucket USD when non-zero), both already flowing off the daemon's
`session/update`. It collapses to one honest line when there's no active session
or no turn has finished yet (all-zero usage), and shows `Cost: —` rather than
`$0.0000` for an unpriced (unregistered-model) session. **`/stats` is session
lifecycle plus portfolio-wide counts** — the current session's age
(`Created`→now), last-active (`Updated`→now), status, and model, above a roster
rollup: how many sessions the fleet holds and the summed tokens (every
normalized bucket) + summed cost across them. Both capture their inputs at open
time like every other tab — `sess` off `App.currentSessionInfo`, and `/stats`
additionally the overview's reference `Now()` (so elapsed output stays
deterministic in goldens) and `Roster()` (the snapshot it sums).

Deferred (issue #175): true per-message / per-tool-call token attribution
(needs SDK per-item usage granularity absent from v0.14.2, which reports usage
only at the turn and session level — rendering a synthesized per-message
estimate as fact is what the issue forbids), and the per-turn activity roll-up
line ("read N files, ran M commands") the issue flags as M8 polish (needs
per-tool-call tallying off the event stream this roster-snapshot projection
doesn't consume).

- **P0**: user markdown commands (`~/.gofer/commands` + project
  `.gofer/commands`, with `$1`, `$ARGUMENTS`, `${1:-def}`, `${@:N}`
  substitution + frontmatter description/argument-hint) ·
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
