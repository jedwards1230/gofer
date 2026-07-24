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
open (attach the full session) · `space` peek · `ctrl-x` kill (running; subtree
interrupted) or archive (finished) — a **two-press confirm**: the first `ctrl-x`
arms the action and shows a status line naming the verb for that row's state
(`kill` vs `archive`), the second `ctrl-x` (same session still selected) runs it;
any other key cancels · `ctrl-t` stop every subagent **below** the
selected row, acting immediately on the selected row. `enter`, `space`, `ctrl-x`
and `ctrl-t` take these meanings only while the dispatch bar is empty; every
other key types into it — so `space` on non-empty text is an ordinary space —
and `enter` on non-empty text starts a new session, or dispatches a `/` command.
Journals are never deleted — `gofer ps --all` lists archived sessions.

**Peek** — a summary card for one session: its title, a one-line
waiting/status line, and a `❯ reply` input. `up`/`down` move the roster
selection (the card follows); `enter` opens (attaches) or, with reply text,
sends the reply; `esc` closes back to the overview, and so does `space` with an
empty reply (`space` is the toggle partner of the overview's `space`-to-peek);
`ctrl+x` kills a running session or archives a finished one, as on the overview —
the same two-press confirm (first press arms, second press acts). Peek carries no
transcript tail — it is a roster-only projection.

**Attach** — full transcript + input. `esc` interrupts the in-flight turn;
`←` on an empty input backs out — to the **parent session** when the attached
session is a subagent, otherwise to the overview (with text, it moves the
cursor); `↓` on an empty input goes the other way, to the overview with this
session's **first spawned child** selected (a no-op when it has none). See
[Subagent sessions](#subagent-sessions-m7--ecosystem) for the drill-in/drill-out
pair.

Every screen — overview, attach's transcript, its approval prompts, and its
command-menu/panel overlays — opens with the same two-line identity header:
`gofer v<version>` then `<model> · <cwd>` (`identityHeaderLines`,
overview_render.go; `attachHeaderLines`, app.go). The overview's own header
adds a third status-count line beneath it; the attach screen's copy leaves
that row blank instead (a global roster tally means nothing once attached to
one session).

The overview header's fourth line is normally the blank separator, but when the
roster is served by a daemon whose build is **older than (or a different build
from) the running CLI** it carries a persistent warn-styled banner —
`⚠ daemon is stale (<daemon> < <cli>) — run: gofer daemon restart`
(`Overview.skewSeparator`, overview_render.go). It is the TUI-visible twin of
the CLI's stderr version-skew warning (`warnVersionSkew`, daemonclient.go),
which the alt-screen swallows on the roster path; both share one classifier
(`internal/versionskew`), so an unknown, matching, or *newer* daemon stays
silent on both. Reusing the separator slot keeps the header a fixed
`headerLines` tall, so the banner costs no body rows and a non-skewed roster
renders byte-identically.

A pending permission request is **not** a centered modal — it renders inline in
the conversation's bottom UI. The gated call's **own tool block** is the
transcript record: the SDK emits `ToolCallStarted` while the provider streams
the `tool_use` (before the loop gates the call), so a `● <tool>` block already
sits in the transcript when the request arrives. While the request is
unresolved the live prompt **commandeers the whole footer** (status line, input
box, and its framing rules) and that block is suppressed from the transcript so
it isn't shown twice; once answered, the footer returns, the block reappears,
and `ToolCallFinished` fills it in with the args/output/exit — **one call, one
item**. (Appending a separate `● <tool>` badge alongside the tool block instead
— the pre-fix shape — left both alive past an allow, rendering the call twice:
the real `● bash(args)` block beside an empty `● bash` bullet. A standalone
`itemApproval` badge now survives only as the fallback for a request that has
no matching tool block.) It reads as a confirm prompt — a rule, an attributed `<tool> command`
header, the call's own description and body, a plain-English rationale, the
question, and the action row, keyed `a`/`d`/`r` (`r` toggles remember, `1`/`2`
alias allow/deny), `tab` amends the call before allowing and `ctrl+e` explains
why it was gated (read-only — both below), `esc` dismisses without answering
(the request stays pending; a re-attach re-surfaces it):

```
 ────────────────────────────────────────────────
 bash command · from the `researcher` agent
   Run the test suite with race detection

   go test -race ./... \
     -run TestApproval
   timeout=120

 Why you're being asked

   No permission rule matched this `bash` call, so gofer is asking before it
   runs. It also cannot be sandboxed on this host, so an allow rule alone
   will not let it run unattended.

   Policy: unmatched · containable: false (no container configured)

   Press `r` before allowing to remember this exact call for the rest of the
   session. Add a rule to the `permissions` array in `config.json` — e.g.
   `{"verdict": "allow", "tool": "bash", "specifier": "go *"}` — to stop
   being asked.

 Do you want to proceed?
   1. [a] Yes   2. [d] No   ·   [r] remember: off   ·   [tab] amend

 esc cancel · ctrl+e explain · session 0192a1b2-…
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
line — the call's own `● <tool>` block already records it (and fills in on
`ToolCallFinished`), and a `permission allow` line on every approved call
(printed *after* the result, reading as if config auto-allowed it) was pure
noise. A **deny** keeps a red `permission deny` line, because a blocked call
changed what happened. The old rule-source parenthetical is dropped either way.

**Richer provenance — what ships.** The prompt now carries the call's
**attribution** ("from the `<agent>` agent", correlated from the tool call's
`event.Agent` — `event.PermissionRequested.ID` *is* the tool call id), its
**multi-line body** (the real command text, not a one-line `cmd=…` summary),
and a **rationale**: why it was gated in plain English, the matched policy
with every raw trace entry preserved, and the two escape hatches that actually
exist — `r` to remember the call for the session, or a rule in `config.json`'s
`permissions` array (the example specifier is built from the call's own first
token, and is omitted rather than guessed at when there is no command body).

**Local vs authoritative rationale.** The rationale on screen starts out
**locally derived** from the guard's decision trace
(`event.PermissionRequested.Trace`). **`ctrl+e`** asks the agent side for the
**authoritative** one over ACP `session/explain_permission`, and replaces it;
the header then reads `Why you're being asked · the agent's answer` (and
`· explaining…` while the call is in flight, during which a second `ctrl+e` is
a no-op). Both are produced by the same grammar — `internal/permrationale`,
shared by the TUI's local render and the daemon's handler — so the two are
comparable rather than differently-worded restatements. A failed explain says
so on the status line and leaves the local rationale standing.

`ctrl+e` is **read-only, and that is a contract, not an implementation
detail**: an explain never resolves the request. The prompt stays open, the
action row stays armed, and the human still answers — asking why must never
cost you the ability to decide. The daemon answers from the request it already
retains, so a client can ask as many times as it likes (`internal/daemon`'s
`handleExplainPermission`; `internal/supervisor`'s `ExplainPermission` is the
daemonless path). An unknown or already-resolved call id is an **error**, not
an empty rationale — "no longer pending" and "gated for no stated reason" are
different answers.

**Height-aware collapse.** The full block runs ~22 rows, which on an 80×24
terminal would leave a two-line transcript and scroll the identity header out
— losing the conversation that led to the gated call exactly when it is needed
to decide. So the prompt adapts: when the full block would leave fewer than
`tui.approval_min_transcript_rows` (default 8) transcript rows, the rationale
collapses to its opening paragraph plus a muted `… ctrl+e to explain`. The
header, the call's body, the question, the action row, and the hint line are
**never** collapsed — nor is an open amend editor, including its cursor line
and its warning (see below). Set the key to `0` to never collapse at all.

**`Tab` — amend the call before allowing — what ships.** `Tab` opens an inline
editor prefilled with the gated call's command body (the same
`command`/`cmd`/`script`/`file_path`/`path` value the prompt displays), in
place of the decision row; the header, attribution, body, and rationale stay
above it. `ctrl+s` approves the EDITED call, `esc` returns to the prompt with
the request still pending and the call untouched, `enter` inserts a line
break, and every other key edits — `a`/`d`/`r`/`1`/`2` type characters rather
than answering, which is the only sane behavior for a text field. Within a
line the app's ordinary text keymap applies (word motion, `ctrl+a`/`ctrl+e`,
`ctrl+w`/`ctrl+u`/`ctrl+k`); `←`/`→` cross line boundaries and `↑`/`↓` move
between lines. The visible editor is capped by the same
`tui.approval_body_lines` budget as the body, but it SCROLLS to keep the
cursor line in view rather than truncating it away. A call whose spec carries
no command-ish key (a structured edit payload, a search query object) has
nothing sensible to edit, so `Tab` there is a no-op with a status note saying
so.

**`ctrl+e` inside the editor is jump-to-end-of-line, not explain.** That is a
deliberate split of the one key both features want: in a text field `ctrl+a`/
`ctrl+e` are the readline bindings every other input in this app gives them,
and an explain fired mid-edit would repaint the rationale under a live cursor
and resize the block while the user types into it. Nothing is lost — `esc`
leaves the editor with the request still pending and the command untouched,
and `ctrl+e` explains from there; while amending, the collapsed rationale's
pointer says `… esc, then ctrl+e to explain` rather than advertising a key
that would do something else. An explain already in flight when `Tab` was
pressed still lands normally: it swaps the rationale block above and leaves
the editor's text, cursor, and warning alone.

Neither the editor nor its warning participates in the height-aware collapse
above. The collapse only ever shortens the rationale, so on any frame size the
line being typed on and the "not re-run through the permission rules" warning
are both still there — the two rows in this block it would be worst to lose.

Two properties of an amend are load-bearing, and the editor states both on
screen rather than burying them here:

- **An amended call is NOT re-run through the permission rules.** The SDK
  substitutes the replacement input into the call *after* the guard already
  evaluated the model's original arguments (`loop.awaitApproval`) — approving
  an edit is a deliberate human override, and the prompt says exactly that in
  the warn style, never anything implying the edit was examined.
- **A remembered amend pins the EDITED call.** The SDK substitutes the input
  *before* calling `Grant`, so `r` + `Tab` + `ctrl+s` makes the command you
  typed the standing grant, not the one the model proposed. The warning adds
  that sentence whenever remember is on.

The reply carries the call's **full original input** with only the edited key
replaced (`tui.Model.AmendedInput`) — `event.PermissionReply.Input` is
substituted wholesale, so a reply carrying just the command would erase every
other argument (a `timeout`, a working directory). The same replacement input
crosses every path a plain verdict does: in-process (`tuibridge`), over the
daemon wire (`permission.reply`'s optional `input` member — `omitempty`, so a
plain allow is byte-identical to before amend existed), through the router's
worker hop, and from a pure ACP client answering with
`{"outcome":"amended","optionId":…,"rawInput":…}`.

**Richer provenance — what remains backlog.** The **gating hook** that raised
the request (e.g. `PreToolUse:Bash`) and a copy-paste **override hint**
carrying its `[plugin:x]` provenance both need fields the permission request
doesn't carry yet — they are the last two provenance items still missing, and
neither key is advertised on the prompt until it lands. Both action-row
affordances now ship: `Tab` amends (SDK `PermissionOutcomeAmended`) and
`ctrl+e` explains (SDK `session/explain_permission`).

**Remember-as-rule** — a grant never widens silently: the prompt offers
exact / prefix / broad patterns, but dangerous commands are force-downgraded
to exact-match regardless, scoped (agent/global) and TTL'd.

## Structured question / decision prompt

An agent that needs a **decision** — not a tool approval, but "which of these
should I do?" — gets a first-class prompt distinct from the permission dialog
above. Like an approval it renders inline in the footer, commandeering it while
unresolved; unlike one it carries the agent's own question and options rather
than a tool call. Dispatch precedence is `panel > approval > decision > menu >
active screen`: a permission gate blocks a tool call already in flight, so it
outranks a decision if both are somehow pending.

The request originates in gofer, from the `ask_user` tool
(`internal/decision`), and travels its own stream — `Supervisor.Decisions` /
`Supervisor.AnswerDecision`, not the Event/Op stream — because the SDK's
`event.Event` union carries no decision kind. See `internal/decision`'s package
doc for the full rationale.

### The `ask_user` tool

Input schema (snake_case, matching the SDK builtins):

```
{ "questions": [ {
    "title":           string   // short chip label, e.g. "Fix rebuilds"
    "question":        string   // required
    "context":         string   // optional supporting context (multi-question side panel)
    "options": [ {
        "label":       string   // required, short — the choice itself
        "rationale":   string   // the indented reasoning/risk body
        "reference":   string   // optional locator for supporting material
        "recommended": bool     // renders "(Recommended)"
    } ]
    "allow_free_text": bool     // DEFAULT TRUE when omitted
    "allow_chat":      bool     // DEFAULT TRUE when omitted
} ] }
```

**Ids are gofer's, never the model's**: `q1, q2, …` for questions and
`q1o1, q1o2, …` for options, assigned by position (`decision.AssignIDs`). An
answer therefore can't reference an id the model hallucinated, and the id space
is stable across runs — which is what makes the tool's result text and this
package's goldens deterministic.

**The escape hatches are opt-out, not opt-in.** `allow_free_text` and
`allow_chat` default to **true**; the model must set them explicitly to `false`
to remove a row. A forced-choice prompt with no way out is a trap, so the agent
has to ask for that trap deliberately.

**Four answer shapes** come back, one per question, in question order:
`selected` (an option id), `text` (the free-text answer), `chat` (the user
wants to talk it through instead of choosing), and `cancelled` (left
unanswered — the gate fills these in). Only a genuinely malformed call is an
error; `chat` and `cancelled` are legitimate outcomes.

### Single question (shipped)

A title chip, the question, numbered options each with a dim rationale sub-line,
a free-text row to answer off-menu, and an escape row that hands the turn back
to the conversation:

```
 ────────────────────────────────────────────────
 decision   Pick a migration strategy

 Which approach should I take?

   1  In-place ALTER
        fastest, but locks the table for the duration
   2  Shadow table + backfill  (Recommended)
        online, but doubles disk until cutover

   › Type something.
   ↳ Chat about this

 Enter to select · ↑/↓ to navigate · Esc to cancel
```

The focused row carries the same `▸` caret every other list in this TUI uses,
in the gutter, so focusing a row never shifts the columns beneath it. Only the
state-bearing tokens are colored (marker-only styling): the `decision` chip
takes the approval header's warn style, the caret and `(Recommended)` take the
accent, the rationale and the key hint are muted. A rationale-less option
renders no sub-line at all, and a row the question opted out of is simply
absent.

**Keys** — `↑`/`↓` move the focused row (clamped, never wrapping: wrapping from
the last row onto option 1 is how a stray press sends the wrong answer);
`1`-`9` answer with that option directly; `Enter` resolves the focused row (an
option answers with it, `Type something.` opens its editor and a **second**
`Enter` submits, `Chat about this` answers with the chat hatch); `Esc` leaves
typing mode, or — when not typing — **cancels the request**, answering every
question in it with `cancelled`; `ctrl+c` quits. While typing, the hint reads
`Enter to submit · Esc to cancel` and every unclaimed key goes to the shared
input keymap, so digits type digits. `j`/`k` are deliberately unbound — every
list here is arrow-only, and vi keys would fight the free-text row.

`Esc` cancels rather than merely closing the prompt because there is nothing to
come back to: unlike a permission request — which leaves a transcript badge and
replays off the event stream on re-attach — a decision has neither, so a prompt
closed without resolving would leave the agent's turn blocked forever with
nothing on screen pointing at it. `cancelled` is a first-class outcome the model
is told about and can act on, not an error.

Resolving (an answer or a cancel) is an optimistic local dismiss, exactly like
an approval: the matching `UpdateResolved` arriving a moment later finds nothing
pending. A request another peer answers, or one an interrupted turn drops,
clears the prompt the same way with no answer sent from here — as does the
session ending, which closes its gate (and with it every decision subscription).

### Multi question (shipped)

A request carrying **several** questions grows a tab strip, a side panel, and a
notes editor — and stops answering on every keystroke. Each answer accumulates
as a **draft**; the final `Submit` tab commits the whole set in **one**
`AnswerDecision` call, which is the whole point: an agent that needs four
sign-offs asks once instead of stalling four times.

The multi-question affordances engage only when the request carries more than
one question. A single question renders exactly as above — a tab strip with one
tab, or a Submit tab for one answer, would be noise on the common case.

```
 ────────────────────────────────────────────────────────────────────────────────
 decision   2 questions
 ←   ▸ □ M4 slicing     ✔ Views v1 scope     □ Submit   →

 How should the M4 milestone be sliced?

 ▸ 1  Renderer first, wiring after                   │ context
        goldens land early, so the frame is          │ M4 is the TUI polish
        reviewable before any plumbing moves         │ milestone; whichever half
   2  Wiring first, renderer after  (Recommended)    │ lands first is what M5 has
        unblocks the daemon path sooner, at the cost │ to build on.
        of an unreviewable frame                     │
                                                     │ reference
   › Type something.                                 │ docs/TUI.md#rendering
   ↳ Chat about this                                 │
                                                     │ notes
                                                     │ leaning renderer-first,
                                                     │ ask again after the spike

 Enter to select · ↑/↓ to navigate · n to add notes · Tab to switch questions ·
 Esc to cancel
```

**The tab strip** carries one tab per question plus the final `Submit` tab,
between two end-affordance arrows. Each tab is `caret · checkbox · label`: the
label is the question's own `title`, falling back to `Q1`/`Q2`/… when the agent
gave it none; the checkbox is `✔` once that question has an answer drafted and
`□` until then, and `Submit`'s own checkbox reports whether *every* question is
answered. State rides two channels on purpose — the caret and the glyph read in
a plain Ascii render (this TUI's "selection reads without color" rule), the
accent on an answered `✔` and on the focused label is the color-only layer the
styled golden pins. When the full strip doesn't fit it degrades to the focused
tab plus an `(i/n)` counter rather than truncating, since a truncated strip can
hide the very tab you're on.

**The side panel** carries the focused question's `context`, the focused
*option's* `reference`, and the note attached to this question's answer — the
split the tool's schema defines: context belongs to the decision, a reference
belongs to one choice within it. It takes about a third of the frame, and it
degrades in two stages: with nothing to show there is no panel at all, and with
something to show but no room beside the options it **stacks beneath them**
instead of squeezing the labels into a two-cell column. A narrow terminal loses
the geometry, never the text. (It is a divider rule rather than the drawn box
the original sketch had: this TUI has no other boxes, and a box's borders have
to be stretched to whichever column is taller, which reads worse than the rule
as options wrap.)

**Options wrap** onto continuation lines indented under their own label rather
than being truncated — the normal case once the panel takes its third of the
frame, and a choice you can't read in full is a choice you can't make.

**Keys** — everything the single-question prompt binds, plus: `Tab` moves to the
next question (and onto `Submit`), `shift+tab` moves back, `←`/`→` do the same
(the strip draws those arrows, and a rendered affordance that does nothing when
pressed would be a lie), and `n` opens a **notes** editor on the focused
question's answer, landing in `DecisionAnswer.Notes`. Enter saves the note, Esc
discards it, and an empty save clears the note — the only way to take one back
off. Tab movement **wraps** where the row cursor clamps: switching tabs commits
nothing, so the stray-keypress surprise the clamp protects against doesn't exist
here. Switching tabs leaves both editors (a half-typed answer belongs to the
question that opened it) and lands the cursor on the answer that question
already has, so returning to it *shows* your pick rather than inviting you to
make it twice. The hint line reads
`Enter to select · ↑/↓ to navigate · n to add notes · Tab to switch questions · Esc to cancel`,
wrapped to the frame.

**Submit vs `Esc`, spelled out** (the issue's open question). The `Submit` tab
reviews what is about to be sent — one line per question, with the unanswered
ones labelled `not answered — cancelled on submit` — and `Enter` on its row
sends **one** `AnswerDecision` carrying every drafted answer. Questions with no
answer are simply **omitted**; the gate fills each of them in as `cancelled`
(`decision.Gate.Answer`'s normalize), which is tested behavior there and is
deliberately not re-implemented in the TUI. So **submitting two of four commits
those two and cancels the other two, and `Esc` cancels all four** — `Esc`
discards the drafts and their notes rather than quietly committing the half that
happened to be filled in, matching the single-question contract above. The one
draft a submit can't leave to the gate is a question that was *annotated but
never answered*: omitting it would drop text the user typed, so it goes out as
an explicit `cancelled` answer carrying its note.

**Not built with it**: a single question's `context` and its options'
`reference` have nowhere to render — the side panel is part of the
multi-question widget, and growing the shipped single-question layout a second
column is its own change with its own golden.

### Across the daemon wire

A decision crosses a daemon connection the way a permission does, with one
structural difference: a permission is an `event.Event`, so the daemon's
`session/prompt` handler observes every one inline while draining the turn's
event stream. A decision is not — the SDK's Event union is closed and carries no
decision kind — so the supervisor runs a **standing per-session watcher** over
the session's gate and hands each update to a `DecisionRelay` the daemon
implements (`internal/supervisor/decision_relay.go`, `internal/daemon/decision_relay.go`).
That watcher is the daemon's only observation point; nothing else can see one.

Four methods carry it:

| Method | Direction | Params |
|---|---|---|
| `gofer/decision_requested` | daemon → every attached peer | `{sessionId, id, questions}` |
| `gofer/decision_resolved` | daemon → every attached peer | `{sessionId, id}` |
| `decision.answer` | client → daemon (notification) | `{sessionId, id, answers}` |
| `session/request_decision` | daemon → each **ACP** peer (request) | ACP `RequestDecisionRequest`, answered with `RequestDecisionResponse` |

`decision.answer` carries a `sessionId` that `permission.reply` does not need:
a decision request id is minted **per session** (`dec-1`, `dec-2`, …), so unlike
a permission call id it does not name a request on its own. The daemon keys its
route/retain/cancel registries on the pair for the same reason.

**First answer wins, from either surface.** A gofer client answers with
`decision.answer`; a pure ACP client answers the `session/request_decision`
request. Whichever reaches the gate first resolves it; the loser is a harmless
no-op (the gate rejects an id that is no longer open) and its outstanding request
is retracted, so no daemon-side waiter dangles. Every peer then receives
`gofer/decision_resolved` and clears its prompt.

**Replay on attach is unconditional.** A peer that attaches while a question is
outstanding is sent it on `session/load`. Unlike a permission — which is an event
and so rides the replay backlog and leaves a transcript badge — a decision is in
no backlog and no journal, so this is the *only* way a late client learns a turn
is blocked on one. It is not behind a config flag.

**With zero peers attached, a decision simply stays open.** The supervisor's own
watcher counts as a subscriber, so `ErrNoClient` (the "continue in prose" fast
path on the daemonless attach) never fires under a daemon. The question waits
until a peer attaches and is replayed it, the turn is interrupted, or the session
ends — exactly what a permission asked with zero peers already does. There is no
decision-side timeout; inventing one would silently answer a question the user
never saw.

**Still deferred: the `--workers` router hop.** Under `gofer daemon --workers`
each session runs in a worker process, and the decision relay does not yet cross
the router↔worker leg — the router's supervisor implements no
`daemon.DecisionAnswerer`, so a decision there is refused with a clear error
rather than silently swallowed. Closing it needs the same treatment permissions
got: a relay + `Notify` across the leg, plus skew-subset handling in
`internal/router/skew.go`.

**And deferred with it**: decisions from **background sessions**. The App
subscribes decisions only for the session it is *attached* to (`app.go`,
alongside the event subscription), so an unattached session's question is not
rendered even though it now reaches the client — the daemon fans it out to every
peer, but the TUI subscribes per attached session. Surfacing "needs a decision"
on the roster the way a pending approval is surfaced is the remaining UI work.

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
bubbletea root that composes overview/peek/attach): `enter` in an **empty**
dispatch bar opens the selected session — attaches into the full, subscribed
transcript (with dispatch-bar text, it instead creates a session from that text
and attaches into it); `space` in an **empty** dispatch bar peeks the selected
session — the roster-only card that does not subscribe (with text, it is an
ordinary space); the arrows carry no open/peek verb on the overview — a bare `→`
is a plain cursor-move; `esc`
interrupts/acts on the *active* session (never "go back"); `←` in an **empty**
input backs out to the attached session's parent, or to the overview when it has
none (with text, it edits); `↓` in an **empty** attach input returns to the
overview with the attached session's first spawned child selected, and does
nothing when it has no children (with text, the key belongs to the input keymap,
not to navigation); `ctrl-x` kills a running
session or archives a finished one (a two-press confirm — press twice);
`ctrl-t` stops the selected row's subagents;
`ctrl-c` quits. In peek, `up`/`down` move
the selection, `enter` opens the session (or sends the reply when the `❯` input
has text), `esc` closes to the overview (and `space` does too with an empty
reply), and `ctrl+x` deletes (press twice to confirm).

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
oldest messages, tailing to the latest by default. A single blank **spacer row**
is pinned at the top of the attach input block (counted in the footer, above the
menu/rules/input), so an overflowing transcript tailing flush to the frame keeps
one row of breathing space between the newest message and the input rule instead
of butting against it; in a short, padded frame it is indistinguishable from the
blank filler below it, so those frames are unchanged. `Overview.render` pads
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

**Markdown rendering (incremental, block-by-block)**: an assistant message's
text is rendered as markdown — bold/italic, headings, lists, blockquotes, inline
code, links, and fenced code blocks — via Charm's
[glamour](https://github.com/charmbracelet/glamour) (the library behind `glow`),
in `markdown.go`. A *settled* message (`MessageFinished`) renders its **whole
text at once** (`markdownRenderer.render`), so glamour's cross-block layout is
exactly what a finished reply shows. A *streaming* (still-open) message renders
**block by block** (`markdownRenderer.renderStreaming`): its COMPLETE markdown
blocks are glamoured while the trailing INCOMPLETE block stays raw. A block is
complete when it is a paragraph closed by a blank line (a lone trailing newline
doesn't count — the line is still being typed) or a `` ``` `` fence closed by its
delimiter; the trailing block — a half-arrived fence, or a paragraph no blank
line has terminated yet — is held raw because glamouring a half-block renders
garbage (a lone `` ```go `` reads as an unterminated fence, a mid-typed `**bo` as
literal asterisks). `splitMarkdownBlocks` is that complete-vs-incomplete oracle
(fence-aware: a blank line inside a fence never splits it, and a closing fence
must carry no info string, so a `` ```python `` line *inside* a fence is body,
not a close). Because each complete block is memoized by its own text, a
keystroke that only grows the tail re-renders nothing already complete; on settle
the whole message renders once more (byte-identical to the finished reply it has
always shown). glamour emits one multi-line string that `markdownRenderer`
splits into one entry per physical row (upholding the one-entry-one-row
invariant above), each already wrapped to the transcript width (so it reflows on
resize) and stripped of glamour's right-pad (so a selection copy — and a code
block especially — carries the raw text, not filler spaces). Three seams keep it
honest: (1) **determinism** — under `theme.Test`'s `termenv.Ascii` profile the
output is ANSI-stripped, so golden files stay plain, byte-stable text; the live
adapter's real profile keeps color/attributes. (2) **color-doesn't-move-layout**
— the pad trim is display-width-aware, so stripping the colored render of its
ANSI yields byte-for-byte the Ascii render (the `assertColorLayout` invariant).
The document/paragraph base color is cleared so plain prose emits no ANSI at all
(only genuine elements — headings, code — are colored), which keeps the
styled-golden `TagANSI` harness — it recognizes only the marker palette — valid
for prose fixtures. (3) **cost** — glamour re-parses on every `Render` (~80µs)
and the transcript re-renders on every keystroke, so each distinct text (a
settled message, or a streaming message's every complete block) is rendered once
and memoized by `(width, text)`; the memo lives on a pointer field shared across
`Model`'s copy-on-write copies and is cleared on a width change.
`internal/tui/markdown_internal_test.go` covers all three plus the block splitter
(`TestSplitMarkdownBlocks`), the progressive complete-vs-raw contract, and
code-block verbatim selection; the `markdown_rendered` golden locks the settled
plain layout and `markdown_streaming_progressive` the mid-stream frame.

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
still scrolled into view) or the overview roster body. On the attach screen
that measurement now goes through the *same* `App.attachModel` helper `render`
draws from — the fully composed model with the background-agents and `!`/`!!`
shell-run blocks appended to the transcript — so those tail blocks are inside
the selectable region and a `$ ls` shell block (or a background-agents line)
can be selected and copied like any other transcript row. Before that shared
helper, `render` drew the composed blocks but `transcriptRegion` measured the
bare `a.sess` without them, so the tail blocks fell below the computed region
and could not be selected. A drag that runs off
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
(`bash(find . -type f | wc -l)` rather than `bash({"command":"…"})`); the
`ask_user` call summarizes to its question(s) — the first question's title (or
its text), or `N questions` for a batch, so the block reads
`ask_user(Choose a task)` over its answer line, never the raw
`{"questions":[…]}` payload; other unknown tool shapes fall back to compact
JSON. While a call is still running its input is
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

**One block renderer.** The tool call, the background-agents summary, and a
`!`/`!!` shell run all render through a single `Model.renderBlock` (model.go) —
the one place the Claude-Code tool-block grammar lives: a marker glyph +
header, then a `└`-gutter body (`   └ ` on the first row, `     ` on every
continuation) with an optional `… +N lines` collapse. A `contentBlock`
describes one block (marker style, glyph, header, per-row-styled `blockRow`s,
and an optional `maxBody` collapse budget); each caller builds one and hands it
over, so the shell block adopts the same `└` gutter the tool tree always had
(only its `$` glyph stays, a meaningful shell affordance). Shell output is
byte-bounded already and the user needs to select/copy it whole, so it sets
`maxBody = 0` and never collapses; the tool block keeps `maxBody = 3`. Because
every block built through `renderBlock` is an ordinary transcript item, it lands
inside `App.transcriptRegion` and is therefore drag-selectable and
OSC 52-copyable for free — a future tool-style block gets the grammar *and* its
selection participation with no extra wiring.

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

## Subagent sessions (M7 · ecosystem)

A subagent is **not a black box within a turn** — it is a real child session
with its own journal, cost, and transcript, linked to its parent.

**Built (the primitive).** `supervisor.CreateOptions{ParentID, Agent}` creates
one: Create resolves the parent (live roster first, then the store root on
disk), derives `Depth = parent + 1`, and refuses an unknown parent
(`ErrNoParent`) or an over-deep chain (`ErrDepthExceeded`). The cap is config,
not a literal — `session.max_subagent_depth`, default 5. The link is durable and
gofer-native: it is written beside the journal as
`<root>/sessions/<slug>/<id>.meta.json` (`{parentId, agent, depth}`), so
`List` reports it for offline sessions and `Resume` restores a child's
attribution. Only a session that has a parent or an agent writes a sidecar, so
nothing changes for a root session. `ParentID`/`Agent`/`Depth` ride the roster
wire (`parentId`/`agent`/`depth`, all omitempty) through to `tui.SessionInfo`;
`session/new` carries the request half in ACP's `_meta` (`gofer/parent`,
`gofer/agent`) and reports what it assigned back (plus `gofer/depth`).
`gofer run --parent <id> --agent <name>` is the CLI spawner. WHO spawns children
from inside a turn is still open — there is deliberately no agent-facing spawn
tool.

**Built (the render).** The overview renders the parent at the root with its
children indented beneath it — a depth-first tree, siblings by the usual recency
rule — each child row the same one-line-per-session shape as a top-level row,
carrying its own summary, run duration, and token tally:

```
~/orchestration
▸!ship the subagent roster      Working · two workers…     41m · ↓ 214.7k tokens
 !  tui-inline-perm-owner       Working · editing ove…   5m 9s · ↓ 214.7k tokens
 !    go-reviewer               Needs input · reviewi…       42s · ↓ 8.4k tokens
    go-developer                Working · running the…  6m 47s · ↓ 128.0k tokens
```

- **Indent inside the title column** (2 cells per level), so every other column
  stays aligned however deep the tree goes. A child is labelled by its **agent**
  (`go-developer`) rather than by the title derived from its parent's prompt.
- **Right column.** A roster holding any subagent swaps the bare age for
  `<elapsed> · ↓ <N> tokens`; one with none renders byte-identically to before.
  The width is one decision per render, not per row — an ordinary roster keeps
  its full-width summary column rather than losing half of it to a tally nobody
  asked for.
- **Blocked rollup.** The `!` gutter marks a row whose session *or any
  descendant* awaits the user, so an approval three levels down is visible
  without descending. Computed once per render, not per row.
- **Overflow.** When the tree outgrows its row budget the last visible line
  reads `↓ N more`.
- **The grouped view (tab) stays flat.** Its sections are status buckets and a
  child's status is independent of its parent's, so nesting there would
  contradict the section label; children keep their own section and are
  identified by the agent label instead.
- **Orphans render as roots.** The roster is a polled snapshot, so a parent can
  legitimately be missing — no row is ever dropped or indented under a parent
  that isn't on screen.

The render reuses the shared row renderer and the id-tracked
selection/windowing the M2 roster already established — a child session is just
a session, so it needed no new navigation model, only the parent→child link and
the indent.

**Built (the navigation).** The fan-out tree is navigable: the tree shows *who
is working*, entering a node shows *what they did*, so a supervisor drills into
any subagent's whole history without losing the parent context.

- **Drill in.** `↑`/`↓` selects a child (a child row is an ordinary roster row),
  and `enter`/`→` opens *that child's* full session — its complete transcript,
  tool blocks, and approvals, exactly as for a top-level session. This needed no
  new code beyond the tree ordering; it is pinned by a test rather than
  reimplemented.
- **Drill out.** `←` on an empty attach input returns to the **parent's**
  session, one level per press, walking a chain back to its root. A root session
  — and a child whose parent is absent from the polled snapshot, the same orphan
  case the roster renders as a root — keeps backing out to the overview. The
  roster selection follows the drill-out, so the header, the panel's session
  views, and the next `←` all agree on where you are.
- **Drill sideways — `↓`.** `↓` on an empty attach input returns to the overview
  with the attached session's **first spawned child** selected; with no children
  it does nothing. This is the key the background-agents block advertises
  ("`↓ to manage`") and it is bound so that caption is literally true: `←` goes
  *up* to the parent, which is not where children are managed — the roster tree
  is, since peek, attach, `ctrl-x` and `ctrl-t` all live there. The empty-input
  guard mirrors `←`'s, but sits in the case expression rather than the body: `←`
  has an editing meaning to fall back on and `↓` has none, so with text pending
  the key is left to the shared input keymap instead of being claimed here.
- **`esc` is NOT a return key — deliberately.** The issue text asked for
  "`esc`/`←` returns to parent"; `esc` on the attach screen is an established,
  tested contract (**interrupt the in-flight turn**) and it is the only
  interrupt binding there is. Hijacking it for navigation would silently delete
  the ability to stop a running turn — a regression dressed as a feature. The
  return path is `←` only. Do not "fix" this.
- **Bulk stop — `ctrl-t`.** On the roster, `ctrl-t` stops every subagent
  *below* the selected row (the whole subtree, one `Supervisor.Kill` per
  descendant), leaving the selected session itself running; `ctrl-x` remains the
  way to stop one session. Kill interrupts and terminates — journals are never
  deleted (invariant #4). A failing kill does not abort the sweep: every
  descendant is attempted and the first error surfaces on the status line. The
  binding avoids `ctrl-s`/`ctrl-q` (flow control), `ctrl-z` (suspend), `ctrl-b`
  (tmux prefix), `ctrl-a`/`e`/`w`/`u`/`k`/`d` (the shared input keymap) and bare
  letters (the dispatch bar is always typeable). The hint line gains
  `ctrl-t stop agents` **only on a tree roster**, in place of `? shortcuts` —
  the flat hint already spends 67 of its 80 cells, and `?` is the one entry
  naming a key nothing handles.

**Built (the transcript blocks).** Two additions, both purely additive — a
session with no subagents renders byte-for-byte what it always did:

- **Background agents.** A session that has spawned children ends its
  transcript with `N background agents launched (↓ to manage)`, then one line
  per child naming it and the agent it runs as. The children are a **roster**
  fact, not an event on this session's stream (a subagent is a separate session
  with its own journal), so the block is composed per frame from the current
  poll (`Model.WithBackgroundAgents`) instead of ingested once and left to go
  stale.
- **Tool-call attribution.** `runner.Options.Agent` (SDK v0.17.0) stamps the
  originating-agent id onto every `tool.call.*` event a session's loop emits and
  the supervisor forwards `CreateOptions.Agent` into it, so a tool block names
  its source: `ToolName(args) · from the <agent> agent`, alongside the existing
  caption, and a transcript interleaving a parent's and its subagents' calls
  reads unambiguously. An event with no agent id renders the un-attributed block
  exactly as before — no placeholder. The attribution rides the transcript item,
  so it outlives the per-call correlation map the approval prompt reads (that
  map is dropped when the call finishes); the two surfaces read the same SDK
  field independently.

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
- **Streaming-markdown block cache** (shipped): a streaming message glamours
  each COMPLETE markdown block once (memoized by its text) and re-renders only
  the incomplete tail block — the block-level realization of "render the settled
  prefix once, re-render only past the last safe markdown boundary". See
  `renderStreaming`/`splitMarkdownBlocks` in `markdown.go`.
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
   tests (`color_layout_test.go`, `dialog_color_test.go`,
   `decision_golden_test.go`, `decision_multi_test.go`) render the same
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
worst-case max. Six builtins register now and open the panel on their tab —
the M4 trio (`/status`, `/config`, `/model`), the M5 read-only pair
(`/usage`, `/stats`), the SDK-catch-up `/thinking`, and `/resume`; each opened
on a one-line placeholder body until its own step landed the real view
(`/status` in step 2, `/config` in step 3, `/model` in step 4, `/usage` +
`/stats` in the M5 usage-panels step, `/thinking` with `Runner.SetEffort`,
`/resume` with the session-lifecycle commands — all below). The first-rune
switch the intercept is built on now carries `!` / `!!` and `@` beside `/` —
see **Built (input prefixes)** below.

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

**Built (SDK catch-up): `/thinking`, the reasoning-effort adjuster.** The
dependency this was waiting on — a runtime `Runner.SetEffort` paralleling the
`Runner.SetModel` that `Supervisor.SetModel` rides on — arrived in agent-sdk-go
v0.17.0, so effort now travels the same road as the model, hop for hop:
`Supervisor.SetEffort` → `gofer/set_effort` (gofer-native JSON-RPC, like
`gofer/set_model`, forwarded router→worker) → `Runner.SetEffort`, with the
level surfaced on the roster row (`SessionInfo.Effort`) and persisted as the
`session.effort` config default. That road stops at the runner, though —
see "Reasoning effort does not reach the provider yet" below before reading
this as an end-to-end feature.

It is its own **Thinking** tab (effortpicker.go) rather than a ←/→ modifier on
the Model tab: ←/→ are claimed by the panel host for tab switching, and effort
is an orthogonal axis. `/thinking` (alias `/effort`) opens it; `/thinking
low|medium|high|off` applies a level directly through the same commit path a
picked row takes — `off` (or `none`/`default`) is the empty level, i.e. "clear
it and let the provider decide". Unlike `/model` there is **no cross-provider
branch**: a provider client is fixed at session creation, which is what
constrains a model swap, but effort is provider-agnostic vocabulary each
backend projects onto its own wire format, so a live session always takes the
change.

What the tab *does* reason about is **model capability**, which is the
"toggle vs effort-picker by model capability" the roadmap asked for. The rule
is the SDK's own, applied client-side so the UI never disagrees with the
runner: reject a **non-empty** level only on **positive registry evidence**
that the active model cannot reason (`provider.Lookup` found it AND
`Reasoning` is false). An unregistered model — anything newer than this binary
— is UNKNOWN, not incapable, so its levels are offered and the runner gets the
final word. The tab issues **no vendor request** on open: unlike the Model
tab's catalog, the level list is a closed four-value enum.

**Clearing is never gated.** `Runner.SetEffort("")` is admitted for any model
whatsoever — the SDK's capability branch sits inside `if effort != ""` —
because asking for no reasoning is coherent everywhere. So on a non-reasoning
model the tab keeps the `off` row selectable and renders the other three muted
rather than collapsing to a bare warning: a session that carried `high` into
that model still has it, and a screen that hides both the level and the way to
drop it is a dead end. `/thinking off` stays legal there too; only
`/thinking low|medium|high` refuses, by name, without writing anything.

### Reasoning effort does not reach the provider yet (SDK gap)

Everything above is real gofer-side state — the roster row, the runner's
`SetEffort`, the wire method — but **no provider request currently changes**,
so `/thinking` cannot yet make a model think harder. Three facts compose:

1. gofer never populates `provider.Params` on any create path, so every runner
   is built with `Thinking{Enabled: false}`.
2. `Runner.Prompt`'s per-turn overlay sets `params.Thinking.Effort` and
   **never** `params.Thinking.Enabled` (agent-sdk-go v0.17.0
   `runner/runner.go`), so `SetEffort` cannot flip it.
3. Both adapters emit reasoning config **only** when `Enabled` is true —
   `provider/openai/request.go` (`if req.Params.Thinking.Enabled`) and
   `provider/anthropic/convert.go` (same).

gofer cannot close this from its side without forcing `Thinking.Enabled: true`
at session creation, which would switch Anthropic extended thinking on (minimum
budget, temperature forbidden) for every affected session — a behavior change
no user asked for — and would *still* leave mid-session `/thinking` inert on
any session created before a default existed. Per invariant #1 it is the
SDK's contract to fix: `SetEffort`'s own doc promises a mid-session effect it
cannot currently deliver.

Consequently the persisted `session.effort` default is **not read at session
creation** (like `session.permission_mode`), and the Thinking tab's ✓
deliberately does **not** fall back to it — the ✓ claims what is *in force*,
and a config default reaches no runner. `/thinking` from the overview says only
"Default reasoning effort saved", claiming nothing about sessions that do not
exist yet. When the SDK gap closes, wire the default into
`Params.Thinking` and restore the config rung in `activeEffort`.

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

**Built (M5 markdown commands)**: `internal/usercmd` turns a saved prompt file
into a slash command. It walks `<store-root>/commands/` (user scope — the
resolved `--root`, not a hardcoded `~/.gofer`) and `<cwd>/.gofer/commands/`
(project scope), both threaded in from `CommandEnv`, taking every `.md` file
recursively; a nested file is namespaced with `:`, so
`commands/git/review.md` is `/git:review`. An optional `---`-delimited header
carries two keys — `description` (the popup's summary) and `argument-hint`
(the `[arg]` beside the name); unknown keys are ignored and a malformed header
degrades to "no frontmatter" plus a warning rather than losing the command.
Running one submits its expanded body through `App.doSend` — the same
`Supervisor.Send` a hand-typed prompt takes, never a second send path — and
refuses with a status note (rather than silently dropping the prompt) when
there is no attached session, or when the body expands to nothing. The
no-session refusal is checked first: when both apply, "attach a session" is
the message the user can act on.

Arguments substitute into the body at dispatch time, in a **single pass**: a
substituted value is never rescanned, so an argument containing `$ARGUMENTS`
is inserted literally instead of injecting tokens into the prompt.

| Token | Expands to |
|---|---|
| `$ARGUMENTS` | every argument, space-joined, in order |
| `$N` / `${N}` | the Nth argument (1-based); missing → empty |
| `${N:-default}` | the Nth argument, or `default` when missing or empty |
| `${@:N}` | arguments N through the end, space-joined |
| `$$` | a literal `$` |

Tokens are recognized inside a word (`internal/$1/doc.go`), `$N` consumes a
maximal digit run (`$12` is the twelfth argument — brace it as `${1}2` for the
other reading), and any `$` that doesn't start a recognized token stays
literal. `internal/usercmd`'s package doc is the full contract, including the
`${@:0}` and out-of-range answers.

`Registry` became **layered** to hold this: `CommandSource` ranks
`extension > markdown > builtin` (docs' long-standing intended order), each
layer is replaceable wholesale, and a name resolves by rank — so a
`status.md` genuinely overrides the builtin `/status`, taking its aliases with
it, and a project file overrides a same-named user file. The extension tier is
reserved and asserted but not populated (plugin `registerCommand` is P1).

**The two scopes are not the same trust level.** `<store-root>/commands` holds
files the *user* wrote, so overriding a builtin there is the feature.
`<cwd>/.gofer/commands` holds whatever a *cloned repository* shipped, so a
project file may **not** claim a builtin's name or alias — a checked-in
`model.md` silently turning `/model` into "send this text to the agent" is
refused at load time and reported on the status line, while the same file in
the user directory still applies. `Registry.builtinNames()` is the reserved
set; `usercmd.Options.ReservedForProject` is the seam, so the builtin list
stays internal/tui's business.

The markdown layer is loaded once in `NewApp` — before `tea.NewProgram`, where
there is no loop to block — and refreshed on the closed→open edge of the
autocomplete popup, once per `/` typed, never per keystroke and never inside
`Registry.matching`. That refresh is a `tea.Cmd` (`loadUserCommandsCmd` →
`userCommandsMsg`, the same shape as `discoverModelsCmd`): the walk is
unbounded in time on a network-mounted cwd, so the popup opens instantly on
the registry as it stands and the fresh layer replaces it in place when it
lands. **`tui.max_command_file_bytes`** (default 256 KiB, `0` = unlimited)
caps a single command file; an over-cap file is skipped with a status note,
never truncated — half a prompt is not a prompt, and the body goes to a model
verbatim. Same `config.json`-knob-not-`/config`-row reasoning as
`tui.max_paste_bytes`.

**Built (`/yolo`)**: the guardrail toggle, dual-bound as `/yolo` and **ctrl+y**
— both routed through one commit path (`yolo.go`'s `applyPermissionMode`) so
they cannot drift. Bare `/yolo` flips; `/yolo on|off` states the posture
outright. It writes `session.permission_mode` through the same
`CommandEnv.Config`/`SaveConfig` pair `/model` uses (a failed READ aborts rather
than overwriting config.json with a zero value), and that value is now
**consumed**: `supervisor.Config.PermissionMode` resolves it PER SESSION
CREATION — `cmd/gofer`'s `permissionModeResolver` re-reads `config.json` on each
create, the same shape as the daemon's `ResolveDefaultModel` — so the next
session a running gofer starts gets the new posture with no restart, on every
backend (daemon, worker, local in-process). `ask` builds the SDK's
`loop.RuleGuard` over the sandbox container (contain-or-ask); `yolo` builds
`supervisor`'s `yoloGuard` over the same engine plus the **unwrapped** builtin
registry — no prompt, no containment, but **config `deny` rules still block** (a
rule written as "never" is not repealed by a second knob). It is deliberately
NOT a `Config.Engine` change: the engine's vocabulary is allow/ask/deny and its
allow already means contain-or-ask, so the mode selects the *guard*, not the
ruleset.

**It does not change a session that is already running**, and both status notes
say so ("… for NEW sessions; running sessions keep theirs"). The SDK fixes a
session's guard at construction (`runner.Options.Guard`) and carries no op to
swap it — there is no `session.set_permission_mode` beside `session.set_model`,
and `Runner` exposes no `SetGuard` — so plumbing a live swap would mean reaching
past the Event/Op contract, which invariant #1 forbids. Turning guardrails OFF
is a `sevWarn` (yellow), never a bare `sevOK`: the action succeeded, but the
posture it leaves the user in is the one thing in the TUI that most deserves to
be visible. Turning them back on is `sevOK`.

**Built (`/help`)**: the last command-panel tab (`help.go`), scrollable with
↑/↓/PgUp/PgDn because the whole table is far longer than `panelBodyRows` and a
silently truncated help screen is worse than none. Its **Commands** section
renders straight from `Registry.List()` — the same registry the dispatcher
resolves against, across every layer — so a command registered anywhere (a
builtin, a user's markdown file, a plugin's runtime registration in M7) appears
with no edit to `help.go`; that is pinned by a test that registers a command the
file has never heard of and expects to find it. Aliases ride on the summary
column rather than the name column, since the name column is padded to its
widest entry and one long `ArgHint` would otherwise truncate every other row's
summary. Its **Keys** section renders from `keymap.go`, a new declarative table.
That table is honestly two-tier: its **global** rows (`ctrl+c`, `ctrl+y`) are
LIVE — `App.handleKey` dispatches through `dispatchGlobalKey`, replacing the
per-screen `ctrl+c` copies — while the per-screen rows are **descriptive only
and can drift**, because several of those bindings are conditional on state a
table can't express (a bare → attaches only from an *empty* dispatch bar) and
routing them all through the table is a whole-TUI key refactor, not this
change. `keymap.go` says so at the top; a collision test sweeps every key the
package binds against the global rows so a future global can't silently steal
one. The table also carries an **Input prefixes** section (`/name`, `!cmd`,
`!!cmd`, `@path`): those are a submit-time grammar rather than bindings, but
they are the part of the input surface a user is least likely to find unaided.
`?` on an empty dispatch bar also opens the panel — the roster footer has
advertised "? shortcuts" since M2 with nothing behind it.

Deferred (issue #175): true per-message / per-tool-call token attribution
(needs SDK per-item usage granularity absent from v0.14.2, which reports usage
only at the turn and session level — rendering a synthesized per-message
estimate as fact is what the issue forbids), and the per-turn activity roll-up
line ("read N files, ran M commands") the issue flags as M8 polish (needs
per-tool-call tallying off the event stream this roster-snapshot projection
doesn't consume).

**Built (session-lifecycle commands)**: `/quit` (alias `/exit`), `/new`, and
`/resume` — the three of the P0 session-lifecycle set the SDK can back today.

`/quit` returns exactly `tea.Quit`, the same one line ctrl-c is bound to on
every screen and over the panel; the daemon connection, the subscription, and
the reconstruction core are owned and closed by `cmd/gofer` once the program
returns, so there is no teardown for the command to duplicate (and no
confirmation, which would make it more ceremonious than the key it mirrors).

`/new` starts a fresh session — new id, new journal — through the same
`Supervisor.Create` seam a prompt typed into the dispatch bar takes, and lands
on the same `createdMsg` attach. The previous session is untouched: still
running, still on the roster, journal intact (invariant #4). It is **not**
`/clear`, which resets the transcript view of the session you are in and is a
separate command. It takes no arguments and declares no `ArgHint`: every string
is a valid prompt, so a prompt argument can never be "unusable", and
`TestArgHintCommandsConsumeArgs` requires every hint-declaring command to reject
an unusable argument with a danger note naming it. Stray arguments are reported
rather than swallowed.

`/resume` follows `/model`'s bare-opens-the-picker / argument-applies-directly
shape. Bare, it opens a sixth panel tab (`resumepicker.go`) listing **every
session on disk, live and offline alike** — the roster answers "what is running
now", which is the wrong question for a resume picker, so the list comes from a
new `Supervisor.ListSessions`: ACP `session/list` on the daemon path (paginated,
every page walked) and `supervisor.List` in process. Rows are newest-first with
a live mark, short id, relative last-active, and project directory, filtered by
type-to-search and windowed so the highlight stays on screen. It opens on an
explicit "Loading sessions…" line — there is no offline floor to guess from, and
a failed listing says why rather than rendering as "no sessions". Enter resumes
the highlighted row **into that session's own cwd**; `/resume <id>` resumes
directly into the client's, matching what `gofer resume` sends. Both land in
`App.resumeSession`, which skips the op entirely for a session the roster
already holds — a redundant `session/load` replays the whole history onto the
reconstruction broker a second time and would double the attach transcript. A
typed id is admitted on shape alone (non-empty, usable as a single path
component); whether it exists is the backend's answer, and an unknown one lands
on the same `sevDanger` status line every other failed op does.

The plumbing is ACP-standard, not gofer-native: `session/load` already existed
end to end (it is how `gofer resume` reaches a daemon), so the only thing
missing was the TUI trigger — `tui.Supervisor` gains `Resume`/`ListSessions`,
mirrored in `internal/tuibridge` and `internal/daemonbridge`.

**Built (input prefixes)**: `/` is no longer the only sigil the submit
intercept switches on. `hasInputPrefix`/`App.dispatchInput` (shell.go) are the
single first-rune switch **both** text-entry surfaces route through, so a
prefix cannot mean one thing in the dispatch bar and another in the attach
input — and it is **leading-only**, so `that worked!` and
`mail me@example.com` submit as ordinary prompts.

- **`!` / `!!` shell escape** (shell.go). `!cmd` runs cmd under `$SHELL -c`
  (falling back to `/bin/sh`) in the session's cwd, off the Update loop.
  **`!!` output never reaches the model; `!` output does.** That exclusion is
  structural, not cosmetic: `App.composePrompt` — the one place local content
  becomes model input — walks the run list and skips any run flagged
  not-in-context, so no rendering, copy, or re-submit path can leak it. A `!!`
  run is marked consumed without contributing, so a later prompt can't pick it
  up either.

  **Reply-now vs queue (ask #2).** A `!` run's DEFAULT on the attach screen is
  *reply-now*: the instant it finishes it flushes everything pending through
  `composePrompt` and fires a turn, so the agent replies immediately — "add it
  as a message" without a separate typed prompt. **ctrl+r** toggles a sticky
  *queue* mode where a `!` run instead waits for the user's next Enter, so they
  can stack more commands (or a plain-text message) before the agent responds —
  the old fold-into-your-next-prompt behavior, now opt-in. The startup default
  is persisted in `tui.shell_reply_mode` (`reply` — the default — or `queue`;
  seeded into `App.shellQueue` at construction), so a user who always wants queue
  mode isn't re-pressing ctrl+r every session; ctrl+r flips it for the running
  session without rewriting config, and `/config` edits the persisted default.
  The mode is captured per-run at dispatch (`shellRun.queued`), so a later
  toggle never rewrites a
  run already in flight, and it governs `!` alone: `!!` is never sent regardless
  of the mode, so the reconciliation is that the toggle only ever moves a `!`
  run between "reply now" and "wait", never in or out of context. The `!`
  auto-send is gated on `inContext` exactly as `composePrompt`'s exclusion is,
  so a `!!` run fires no turn even in reply-now mode. Output is bounded
  (`tui.shell_max_output_bytes`, default 64 KiB, with a visible truncation
  marker) and so is runtime (`tui.shell_timeout_ms`, default 30s); a non-zero
  exit is reported as an exit code with the command's own stderr retained,
  stdout and stderr interleave in arrival order, and a bare `!` runs nothing.
  It is **not** a tool call and deliberately touches no part of the
  permission/approval path — the user typed it themselves, and nothing the
  model emits can reach it.

  **Presentation — the sigil is the signal (round-5).** Three things make the
  escape legible, all keyed on the `!` / `!!` sigil:
  - **Input line.** While a `!` / `!!` command is being *typed*, the sigil IS
    the prompt: the `> ` / `❯ ` glyph is dropped and the line starts with the
    accented sigil (`!! rm -rf /tmp/scratch▏`), under a plain (unlabeled) framing
    rule. A single **display-only space** separates the sigil from the command
    (`! ls docs`, not `!ls docs`), present from the first keystroke — a bare `!`
    renders `! ▏`. The space is rendering only: `parseShellEscape` reads the
    buffer, not this line, so `!ls` and `! ls` run byte-identical commands
    (`shellInputLine`, `inputLine`). A non-shell buffer keeps its `> ` / `❯ `
    prompt byte-for-byte as before. The reply-now/queue mode has **no rule
    label** (an earlier verbose `── shell · … ──` label was reverted): ctrl+r
    still flips it (default `tui.shell_reply_mode`), and the tell is the thinking
    indicator below — a reply fired vs. its absence (queued). The empty overview
    dispatch bar carries a subtle `! for shell mode` discoverability hint so a
    user learns the sigil before typing it.
  - **In the transcript, not a pane.** On the attach screen a run renders as a
    transcript block: the **sigil as the block marker** (`! command` /
    `!! command`), the output under the `└` gutter, and its outcome (`exit N` iff
    non-zero, a timeout/failure note, a truncation marker). It reads as part of
    the conversation rather than a dismissible overlay below it. A **pending**
    run (running, or finished but not yet folded) renders render-local at the
    tail (`Model.WithShellRuns`, the background-agents pattern). When it is
    **consumed** — folded into a submitted prompt (which the reply-now default
    does the instant it fires) — it is pinned into the transcript as a persistent
    `itemShellRun` at that position (`Model.CommitShellRuns`, from
    `App.composePrompt`), so it keeps showing as `! command` rather than vanishing
    or re-appearing as the echoed prompt. **The `$ cmd` fold is ONLY the model's
    copy, never on screen**: the daemon echoes the folded prompt verbatim as the
    user message, so the matching `MessageFinished{User}` echo is stripped of the
    fold (a byte-exact prefix match against the queued fold — never `$`-parsing;
    a miss degrades to rendering the echo verbatim). A pure `!` turn strips to
    nothing (the sigil block IS the turn); a `!`+typed submit shows the sigil
    block(s) plus the typed message. Screens with no transcript (the overview
    dispatch bar, peek) acknowledge a finished run on the status line instead
    (`shellRun.shellRunStatus`).
    - **Limitation:** the strip needs the fold this process recorded at submit,
      so a run consumed on a submit that **switches sessions** (typing `!cmd` at
      the overview to create a new session) and any run in a session **resumed**
      from the journal (no in-memory fold record) still show the `$` fold in the
      echo. The clean long-term fix is persisting shell-run structure in the
      journal; tracked as a follow-up.
  - **Private-run signal is the marker.** A `!!` run's marker is a DISTINCT color
    from a `!` run's — `!` accent, `!!` warn (`Model.shellMarker`) — and that,
    with the doubled glyph, is the ONLY at-a-glance "the agent can't see this"
    signal: there is no `· not sent to the agent` text line. Losing it would be a
    safety regression, so the marker must stay unmistakable. The marker is read
    off `shellRun.inContext` for DISPLAY only; `composePrompt` remains the SOLE
    decider of what reaches the model, so no view change can move a byte in or out
    of context. The transcript-less status ack (`shellRun.shellRunStatus`,
    overview/peek) still spells `sent with your next message` / `not sent to the
    agent` in words, since there is no marker there to carry it.
- **Thinking indicator** (`Model.WithThinking`, model.go). A turn in flight shows
  a muted `⋯ working…` at the transcript tail; a queued shell command shows
  nothing — the absence of the indicator means nothing is pending. It is derived
  from `Model.turnActive` (set on `event.TurnStarted`, cleared on `TurnFinished`
  / `SessionError`) and suppressed while an approval or decision prompt owns the
  footer (that is "awaiting you", not "working"). Composed render-local and last
  in `App.attachModel`, so it never enters the durable transcript.
- **`@` file mention** (filemention.go). Typing `@` at a token boundary opens
  the same popup the slash commands use, sourced from the paths under the
  session's cwd — `git ls-files` inside a repository (which is what makes it
  honor `.gitignore` and skip `.git/`), a bounded `filepath.WalkDir`
  otherwise, enumerated off the Update loop once per mention and bounded by
  `tui.file_mention_max_entries` / `tui.file_mention_max_depth`. Tab or Enter
  splices the path into the buffer. **A submitted `@path` passes the PATH
  through as text — it does NOT inline the file's contents.** So a mention
  costs the length of a path, not a file's worth of tokens; the agent reads
  it with its own file tools if it wants more. The trigger is a token
  boundary, so an email address never opens the popup.

- **P0**: `/compact [instructions]` (block-if-busy) — **blocked on the SDK**: `v0.17.0`
  ships the `session.compact` op and `session.compacted` event as data types and
  a `session.NewCompactionEntry` journal entry, but no way for an embedder to
  TRIGGER compaction. There is no `Runner.Compact`, no compaction option on
  `runner.Options` or the loop, and `Runner` keeps its `*session.Journal`
  unexported with no accessor — so gofer cannot summarize-and-append without
  reaching around the contract (invariant #1). Unblocked by a runner-level
  entrypoint, e.g. `func (r *Runner) Compact(ctx context.Context, instructions
  string) error` that folds the history, appends the compaction entry, and emits
  `session.compacted`.
- **P1**: `/init` (first-run project context) · `/fork` · `/tree` ·
  `/export html|jsonl` · `/login` · runtime `registerCommand` from plugins ·
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
