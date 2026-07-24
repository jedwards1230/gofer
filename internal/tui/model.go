// Package tui is gofer's minimal attach surface: an ordered transcript, an
// input buffer, and a status line, rendered as a projection of a session's
// typed Event stream (per docs/CONTRACT.md's Event/Op contract in
// agent-sdk-go).
//
// [Model] is the pure, headlessly-testable core — it has no bubbletea
// dependency. A caller wires it to a live session by subscribing to the
// session's *event.Subscription and forwarding each event.Event into
// [Model.Ingest] (the bubbletea [Program] adapter in adapter.go does this
// for a real terminal, wrapping each event as an [EventMsg]). This is the
// seed of the full screen-stack design in docs/TUI.md (overview ⇄ peek ⇄
// attach); the overview⇄peek⇄attach navigation shipped in M2, a first
// interactive prompt — the inline permission-approval block rendered by
// View (see approval.go, and dialog.go for the key handling)
// landed in M3, and the fuller dialog/keymap system lands later.
package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/decision"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// itemKind distinguishes transcript item shapes.
type itemKind int

const (
	itemAssistantText itemKind = iota
	itemAssistantReasoning
	itemUser
	itemTool
	itemError
	itemApproval
	itemApprovalResolved
	// itemBackgroundAgents is the block naming the subagent sessions this
	// session spawned. Unlike every kind above it, no event produces it: a
	// subagent is a separate session with its own journal and event stream, so
	// the children are a ROSTER fact the app composes onto a render-local copy
	// of the model (see [Model.WithBackgroundAgents]), never an ingested one.
	itemBackgroundAgents
	// itemShellRun is one `!` / `!!` shell escape the operator ran (shell.go).
	// Like itemBackgroundAgents it is composed onto a render-local copy of the
	// model, never ingested: a run is App state, and an unconsumed one is
	// transient — it stops rendering the moment [App.composePrompt] folds it
	// into a prompt (see [Model.WithShellRuns]).
	itemShellRun
	// itemThinking is the transient "a turn is in flight" indicator at the
	// transcript tail. Like the two above it is composed onto a render-local copy
	// ([Model.WithThinking]), never ingested — it is derived from [Model.turnActive]
	// and vanishes the instant the turn finishes, so it never belongs to the
	// durable item list or the exit-flushed transcript.
	itemThinking
)

// spawnedAgent is one child session named by an [itemBackgroundAgents] block:
// the name to show it under and the agent identity it runs as.
type spawnedAgent struct {
	name  string
	agent string
}

// item is one entry in the transcript. Tool-only fields are zero on every
// other kind.
type item struct {
	kind itemKind
	text string // settled/streaming content for text, reasoning, user, error, approval
	done bool   // MessageFinished / ToolCallFinished has been seen

	toolName   string
	toolInput  string
	toolResult string
	toolErr    bool

	// toolAgent is itemTool-only: the agent id that ISSUED the call, carried by
	// event.ToolCallStarted.Agent (SDK v0.17.0) and stamped by the supervisor
	// from CreateOptions.Agent. "" — the whole single-agent world, and any
	// pre-v0.17.0 daemon — renders the un-attributed block byte-for-byte as
	// before (see [Model.renderToolLines]).
	toolAgent string

	// spawned is itemBackgroundAgents-only: the child sessions this session
	// fanned out to, in roster order.
	spawned []spawnedAgent

	// approvalVerdict is itemApprovalResolved-only: the resolved
	// event.Verdict ("allow"/"deny").
	approvalVerdict string

	// shell is itemShellRun-only: the `!` / `!!` run this block renders. Its
	// inContext flag drives the block's disposition LABEL only — what actually
	// reaches the model is [App.composePrompt]'s call, never this copy.
	shell shellRun
}

// Model is gofer's minimal attach surface. It is immutable from the
// caller's perspective: [Model.Ingest] and the input-editing methods return
// an updated copy rather than mutating in place, so a fixed event sequence
// replays to the same rendered output in every test. The one exception is
// [Model.TakeSubmitted], which has a pointer receiver and mutates in place to
// ensure its take-once semantics (each submitted prompt is observed exactly
// once).
type Model struct {
	theme theme.Theme

	// md renders a settled assistant message's markdown (markdown.go). It is a
	// pointer so the memo it holds survives Model's copy-on-write recopying —
	// every copy shares the one renderer created in New.
	md *markdownRenderer

	items []item

	// openText/openReasoning index into items for the message currently
	// streaming, or -1 when none is open. The loop streams at most one text
	// and one reasoning message at a time.
	openText      int
	openReasoning int

	// toolIndex maps an in-flight tool call's ID to its item index.
	toolIndex map[string]int

	// toolAgents maps an in-flight tool call's ID to the originating agent id
	// its event carried (event.ToolCallStarted.Agent), for the approval prompt
	// to attribute a gated call to the subagent that issued it. The correlation
	// is by tool call id because event.PermissionRequested.ID *is* the tool call
	// id (the SDK's loop.awaitApproval publishes NewPermissionRequested with
	// call.ID), and tool.call.started is emitted while the model streams —
	// before the gate — so the entry is always already recorded when the request
	// arrives. Un-attributed calls are simply absent, and a lookup miss yields
	// "" (the un-attributed rendering), never a placeholder.
	toolAgents map[string]string

	// approvalBodyLines is the resolved tui.approval_body_lines row cap the
	// approval prompt's body honors, or 0 for "use the config default" — the
	// zero value a Model built by [New] carries, so every caller that doesn't
	// plumb config (every golden test, FullTranscript) renders the default. The
	// App sets it per render off its always-current config read (see
	// App.render).
	approvalBodyLines int

	// approvalMinTranscriptRows is the resolved tui.approval_min_transcript_rows
	// floor the approval prompt collapses against, or -1 for "use the config
	// default". It is -1 rather than 0 for the zero value's sake: 0 is a
	// MEANINGFUL setting here ("never collapse", see
	// [config.TUI.ApprovalMinTranscriptRowFloor]), so it cannot double as the
	// unset sentinel the way approvalBodyLines' 0 does. [New] sets it; the App
	// overwrites it per render off its always-current config read. A Model
	// built as a zero value (a struct literal in a test) reads 0 and simply
	// never collapses, which is the pre-collapse behavior.
	approvalMinTranscriptRows int

	// pending is the session's current unresolved permission request, if any
	// (nil = none) — the backing state for the interactive inline approval
	// prompt. It is transient client-side state (like input), NOT a transcript
	// item: while set, it commandeers the whole footer (see View) in place of
	// the status line and input box, and disappears on resolve or dismiss (the
	// footer returns), while the itemApproval badge and itemApprovalResolved
	// line stay as the permanent record. Following Model's copy-on-write
	// discipline the pointer is never mutated in place — every mutator
	// reallocates and repoints.
	pending *pendingApproval

	// pendingDec is the session's current unresolved structured-decision
	// request, if any (nil = none) — the backing state for the inline decision
	// prompt (decision.go). It is `pending`'s sibling in every respect that
	// matters here: transient client-side state, not a transcript item, and
	// while set it commandeers the whole footer the same way. It arrives on a
	// SEPARATE stream ([Model.IngestDecision], not [Model.Ingest]) because the
	// SDK's Event union carries no decision kind — see internal/decision's
	// package doc. An approval outranks it when both are somehow pending (see
	// promptLines): a permission gate blocks a tool call already in flight,
	// which is the more urgent of the two.
	pendingDec *pendingDecision

	usage *provider.Usage
	cost  *provider.Cost

	// input is the attach input's buffer — a cursor-aware [inputBuffer]
	// (inputbuf.go), not just an append-only string.
	input inputBuffer

	submitted    string
	hasSubmitted bool

	// pendingEchoFolds is the FIFO queue of model-facing shell folds
	// ([shellRun.contextBlock] output) that [App.composePrompt] submitted and
	// whose user-message echo has not yet arrived. When a MessageFinished{User}
	// echo begins with the head fold, that prefix is stripped from the displayed
	// message (the shell runs already render as sigil blocks, committed by
	// [Model.CommitShellRuns] at submit time) and the head is popped — so the
	// `$ cmd` fold the MODEL reads is never shown on screen, only the `!` sigil
	// block. A byte-exact prefix match, not parsing: on any miss it degrades to
	// today's behavior (the echo renders verbatim) rather than guessing. Empty on
	// resume — a session attached mid-conversation has no record of past folds, so
	// historical `$` echoes render as-is (see docs/TUI.md).
	pendingEchoFolds []string

	// turnActive reports whether a turn is in flight — the agent is generating a
	// response. Set true on [event.TurnStarted], false on [event.TurnFinished]
	// (and defensively on [event.SessionError], which ends the turn's work). It
	// is the backing state for the transcript's thinking indicator
	// ([Model.WithThinking]); the broker retains TurnStarted as must-deliver, so
	// attaching mid-turn replays it and the indicator shows correctly. A
	// zero-value Model (every golden calling View directly) is idle, so no
	// indicator renders and no existing golden churns.
	turnActive bool
}

// New returns an empty Model rendering through th.
func New(th theme.Theme) Model {
	return Model{
		theme:                     th,
		md:                        newMarkdownRenderer(th.Profile),
		openText:                  -1,
		openReasoning:             -1,
		toolIndex:                 map[string]int{},
		toolAgents:                map[string]string{},
		approvalMinTranscriptRows: -1, // "use the config default" — see the field doc
	}
}

// Ingest applies e to the transcript and returns the updated Model. Event
// kinds the minimal attach surface doesn't render (session lifecycle,
// permission resolution) are accepted and ignored, so a caller can forward
// the full stream unfiltered.
func (m Model) Ingest(e event.Event) Model {
	m.items = append([]item(nil), m.items...)
	toolIndex := make(map[string]int, len(m.toolIndex))
	for k, v := range m.toolIndex {
		toolIndex[k] = v
	}
	m.toolIndex = toolIndex
	toolAgents := make(map[string]string, len(m.toolAgents))
	for k, v := range m.toolAgents {
		toolAgents[k] = v
	}
	m.toolAgents = toolAgents
	// Clone the fold queue up front, alongside items/maps: Model is value-copied
	// per frame, so the MessageFinished{User} case below reslicing this to pop the
	// head must not write through to a prior Model's shared backing array. Same
	// nil-when-empty idiom as m.items above.
	m.pendingEchoFolds = append([]string(nil), m.pendingEchoFolds...)

	switch ev := e.(type) {
	case event.TurnStarted:
		// A turn is now in flight — the agent is generating. Drives the thinking
		// indicator ([Model.WithThinking]); no transcript item, just the flag.
		m.turnActive = true

	case event.TurnFinished:
		m.turnActive = false
		usage := ev.Usage
		m.usage = &usage
		m.cost = ev.Cost

	case event.MessageStarted:
		// event.MessageUser (the user's own prompt turn) is a settled
		// Started/Finished pair with no deltas — see event.MessageUser's doc —
		// so it never opens a streaming item the way assistant text/reasoning
		// does. Ignoring MessageStarted here and building the whole item on
		// MessageFinished (below) makes this Ingest robust to either arrival
		// order, and keeps a user message from ever colliding with
		// openText/openReasoning's single-open-item-per-kind bookkeeping.
		if ev.MessageKind == event.MessageUser {
			break
		}
		idx := len(m.items)
		kind := itemAssistantText
		if ev.MessageKind == event.MessageReasoning {
			kind = itemAssistantReasoning
		}
		m.items = append(m.items, item{kind: kind})
		m.setOpen(ev.MessageKind, idx)

	case event.MessageDelta:
		if idx, ok := m.openIndex(ev.MessageKind); ok {
			m.items[idx].text += ev.Text
		}

	case event.MessageFinished:
		if ev.MessageKind == event.MessageUser {
			// Strip the model-facing shell fold from the echo: a reply-now `!` run
			// (or a `!`+typed submit) reaches the model as `$ cmd\n<output>\n\n` +
			// the typed text, and the daemon echoes that verbatim. The shell runs
			// already render as sigil blocks (CommitShellRuns at submit); showing
			// the `$` fold here too would duplicate them in the model's format. A
			// byte-exact prefix match against the head pending fold — never
			// parsing `$`, so a miss just renders verbatim (today's behavior).
			content := ev.Content
			if len(m.pendingEchoFolds) > 0 && strings.HasPrefix(content, m.pendingEchoFolds[0]) {
				content = strings.TrimPrefix(content, m.pendingEchoFolds[0])
				m.pendingEchoFolds = m.pendingEchoFolds[1:] // safe: the queue was cloned at the top of Ingest
			}
			// A pure `!` turn (no typed text) strips to nothing: the sigil block is
			// the whole user turn, so add no empty user message.
			if content != "" {
				m.items = append(m.items, item{kind: itemUser, text: content, done: true})
			}
			break
		}
		if idx, ok := m.openIndex(ev.MessageKind); ok {
			m.items[idx].text = ev.Content
			m.items[idx].done = true
		}
		m.setOpen(ev.MessageKind, -1)

	case event.ToolCallStarted:
		idx := len(m.items)
		m.items = append(m.items, item{
			kind:      itemTool,
			toolName:  ev.Name,
			toolInput: compactJSON(ev.Input),
			// The attribution rides the ITEM, not just the per-call toolAgents
			// map below: the map is dropped on ToolCallFinished (it exists to
			// correlate a gated call to its approval prompt, a window that closes
			// when the call runs), while the transcript block keeps naming its
			// source for as long as the transcript exists.
			toolAgent: ev.Agent,
		})
		m.toolIndex[ev.ID] = idx
		if ev.Agent != "" {
			m.toolAgents[ev.ID] = ev.Agent
		}

	case event.ToolCallDelta:
		// ToolCallDelta carries a fragment of the streaming INPUT (partial
		// JSON arguments as the provider assembles them), not the result —
		// see event.ToolCallDelta's doc. The authoritative input and result
		// both arrive together on ToolCallFinished (below), so this is
		// deliberately a no-op; the toolIndex bookkeeping above still
		// applies to it.

	case event.ToolCallFinished:
		if idx, ok := m.toolIndex[ev.ID]; ok {
			if len(ev.Input) > 0 {
				m.items[idx].toolInput = compactJSON(ev.Input)
			}
			m.items[idx].toolResult = ev.Result
			m.items[idx].toolErr = ev.IsError
			m.items[idx].done = true
		}
		// The call is over: both per-call maps drop it in the same place, so an
		// attribution lives exactly as long as the item index it is keyed
		// alongside. The PermissionRequested window has necessarily closed by
		// now — the guard gates a call BEFORE it runs, so a finished call can
		// no longer be awaiting approval — which is also why ToolCallFinished
		// deliberately does NOT record ev.Agent: an entry written here would be
		// deleted on the next line and could never be read.
		delete(m.toolIndex, ev.ID)
		delete(m.toolAgents, ev.ID)

	case event.SessionError:
		// A turn that errors is no longer working: clear the thinking indicator
		// so it can't stick "working…" on after a failure that emits no
		// TurnFinished.
		m.turnActive = false
		m.items = append(m.items, item{kind: itemError, text: ev.Err, done: true})

	case event.PermissionRequested:
		// The inline badge is the permanent transcript record; m.pending is
		// the transient interactive prompt state rendered beneath it (see
		// View). A second request supersedes the first — last one shown wins;
		// the superseded request stays pending server-side and its own
		// PermissionResolved simply finds m.pending pointed elsewhere below.
		// badgeIdx records the badge's transcript index so transcriptLines can
		// suppress it while the prompt block is showing (the prompt already
		// repeats the tool + args line).
		// agent/trace are the request's provenance: the agent that issued the
		// gated call (correlated through toolAgents — ev.ID is the tool call
		// id) and the guard's own decision trace, which the prompt renders as
		// the rationale. A miss in toolAgents yields "", the un-attributed
		// rendering.
		idx := len(m.items)
		m.items = append(m.items, item{kind: itemApproval, text: ev.Tool, done: true})
		m.pending = &pendingApproval{
			id:       ev.ID,
			tool:     ev.Tool,
			spec:     ev.Spec,
			session:  ev.SessionID(),
			agent:    m.toolAgents[ev.ID],
			trace:    ev.Trace,
			badgeIdx: idx,
		}

	case event.PermissionResolved:
		// A routine allow is the expected outcome, and its transcript line is
		// noise — the ● badge already recorded the request. Deny/ask stay
		// visible because they change what happened. Either way, clear the
		// interactive prompt if it was still showing this request (this
		// client answered via resolveApproval, or another attached client
		// answered first).
		if ev.Verdict != event.VerdictAllow {
			m.items = append(m.items, item{
				kind:            itemApprovalResolved,
				approvalVerdict: string(ev.Verdict),
				done:            true,
			})
		}
		if m.pending != nil && m.pending.id == ev.ID {
			m.pending = nil
		}

		// event.SessionCreated, event.SessionResumed, event.SessionForked,
		// event.SessionCompacted, event.SessionKilled, and
		// event.SessionArchived carry no transcript-visible state in the
		// minimal attach surface; they fall through untouched.
	}

	return m
}

// openIndex returns the item index currently streaming the given message
// kind, if one is open.
func (m Model) openIndex(kind event.MessageKind) (int, bool) {
	idx := m.openText
	if kind == event.MessageReasoning {
		idx = m.openReasoning
	}
	if idx < 0 || idx >= len(m.items) {
		return 0, false
	}
	return idx, true
}

// setOpen records idx as the open item for the given message kind.
func (m *Model) setOpen(kind event.MessageKind, idx int) {
	if kind == event.MessageReasoning {
		m.openReasoning = idx
	} else {
		m.openText = idx
	}
}

// HasPendingApproval reports whether an unresolved permission request is
// awaiting a decision on this session — the app root consults it to route the
// a/d/r keys to the approval prompt (see App.Update).
func (m Model) HasPendingApproval() bool { return m.pending != nil }

// PendingApproval returns the id and remember-toggle state of the pending
// permission request, for the app root to build the Supervisor.Reply. ok is
// false when nothing is pending.
func (m Model) PendingApproval() (id string, remember bool, ok bool) {
	if m.pending == nil {
		return "", false, false
	}
	return m.pending.id, m.pending.remember, true
}

// ToggleApprovalRemember flips the pending request's remember toggle,
// reallocating rather than mutating in place (Model's copy-on-write
// discipline). A no-op when nothing is pending.
func (m Model) ToggleApprovalRemember() Model {
	if m.pending == nil {
		return m
	}
	p := *m.pending
	p.remember = !p.remember
	m.pending = &p
	return m
}

// ApprovalExplaining reports whether a session/explain_permission call is in
// flight for the pending request — the app root reads it so a second ctrl+e
// while the first is still out is a no-op rather than a stacked call. False
// when nothing is pending.
func (m Model) ApprovalExplaining() bool {
	return m.pending != nil && m.pending.explaining
}

// MarkApprovalExplaining records that an explain is in flight for the pending
// request, reallocating rather than mutating in place (Model's copy-on-write
// discipline). A no-op when nothing is pending.
//
// It does NOT touch the request itself: an explain is read-only, so the prompt
// stays open and answerable while it runs, and the a/d/r/esc keys keep
// working exactly as they did (see [App.explainApproval]).
func (m Model) MarkApprovalExplaining() Model {
	if m.pending == nil {
		return m
	}
	p := *m.pending
	p.explaining = true
	m.pending = &p
	return m
}

// SetApprovalRationale records the authoritative rationale an explain returned
// and clears the in-flight marker, reallocating rather than mutating in place.
// A no-op when nothing is pending. The pending request is untouched — the
// human still answers it.
func (m Model) SetApprovalRationale(r acp.PermissionRationale) Model {
	if m.pending == nil {
		return m
	}
	p := *m.pending
	p.explaining = false
	p.rationale = &r
	m.pending = &p
	return m
}

// ClearApprovalExplaining drops the in-flight marker WITHOUT recording a
// rationale — the failed-explain path. Whatever rationale was on screen (the
// local derivation, or an earlier successful answer) stays, so a failed
// explain costs the user nothing but the status-line note the app root sets.
func (m Model) ClearApprovalExplaining() Model {
	if m.pending == nil {
		return m
	}
	p := *m.pending
	p.explaining = false
	m.pending = &p
	return m
}

// WithApprovalBodyLines sets the row cap the inline approval prompt's body
// honors, reallocating rather than mutating in place (Model's copy-on-write
// discipline). n <= 0 means "the config default" — see [Model.approvalBodyLimit].
// [App.render] calls this with its always-current tui.approval_body_lines
// read; a Model nobody plumbs config into keeps the default.
func (m Model) WithApprovalBodyLines(n int) Model {
	m.approvalBodyLines = n
	return m
}

// WithBackgroundAgents returns a copy of the model whose transcript ends with
// a block naming the child sessions this one spawned — "N background agents
// launched (↓ to manage)", then one line per child. children with no entries
// returns the model untouched, so a session that never fanned out renders
// byte-for-byte as it always has.
//
// It is a per-render composition, not an Ingest: a subagent is a separate
// session with its own event stream, so nothing on THIS session's stream
// announces one (there is deliberately no agent-facing spawn tool — see
// docs/TUI.md § "Subagent sessions"). [App.render] calls it on a render-local
// copy with the current roster's children, exactly as it does
// [Model.WithApprovalBodyLines], so the block tracks the poll instead of
// accumulating a stale item per frame.
//
// The block sits at the TAIL of the transcript rather than at the point of
// spawn: it is a live summary of what is running now, not a historical entry,
// and the transcript is bottom-anchored so the tail is where a reader is
// already looking. Appending also leaves every existing item index untouched,
// which the pending approval's badgeIdx depends on.
func (m Model) WithBackgroundAgents(children []SessionInfo) Model {
	if len(children) == 0 {
		return m
	}
	spawned := make([]spawnedAgent, 0, len(children))
	for _, c := range children {
		spawned = append(spawned, spawnedAgent{name: backgroundAgentName(c), agent: c.Agent})
	}
	items := make([]item, 0, len(m.items)+1)
	items = append(items, m.items...)
	m.items = append(items, item{kind: itemBackgroundAgents, done: true, spawned: spawned})
	return m
}

// WithShellRuns returns a copy of the model whose transcript ends with one
// block per shell escape (shell.go) that is either still running or finished
// but NOT YET consumed by a prompt. A consumed run is deliberately skipped:
//
//   - a `!` run's output has by then been folded into a real prompt
//     ([App.composePrompt]) and comes back as the echoed user message, so
//     rendering the run too would duplicate it; and
//   - a `!!` run's output was operator-only and was never sent, so once the
//     operator moves on (sends their next prompt, consuming it) there is
//     nothing in the thread it belongs beside.
//
// So the only shell blocks on screen are the ones a subsequent prompt will
// act on — which is exactly what makes rendering them at the TAIL correct:
// they are the most recent thing the operator did, and nothing follows them
// until a prompt is sent. runs with no visible entries returns the model
// untouched, byte-for-byte as a session that ran no commands.
//
// Like [Model.WithBackgroundAgents] this is a per-render composition, never an
// Ingest: the runs are App state (they must survive a screen switch and feed
// composePrompt), and appending leaves every existing item index untouched,
// which the pending approval's badgeIdx depends on.
func (m Model) WithShellRuns(runs []shellRun) Model {
	// Count first so the common steady state — no runs, or every run already
	// folded into a prompt (consumed) — returns untouched without cloning the
	// transcript, mirroring WithBackgroundAgents' len(children)==0 short circuit.
	visible := 0
	for _, r := range runs {
		if !r.consumed {
			visible++
		}
	}
	if visible == 0 {
		return m
	}
	items := make([]item, 0, len(m.items)+visible)
	items = append(items, m.items...)
	for _, r := range runs {
		if r.consumed {
			continue
		}
		items = append(items, item{kind: itemShellRun, done: r.done, shell: r})
	}
	m.items = items
	return m
}

// CommitShellRuns pins consumed shell runs into the transcript as PERSISTENT
// sigil blocks at the current tail — the point in the conversation where they
// were folded into a submit, just before that submit's user-message echo. It is
// how a consumed `!` run keeps showing as `! cmd` after it fires its turn
// (WithShellRuns render-local blocks vanish on consume, and would tail-misorder
// after the reply anyway): once committed here they are ordinary ingested items,
// correctly placed and stable across later events. Both `!` and `!!` runs commit
// (each renders with its own sigil marker); fold is the concatenated model-facing
// [shellRun.contextBlock] output of the `!` runs only (a `!!` run contributes
// none — it is never sent), queued so the matching MessageUser echo can be
// stripped of it (see the MessageFinished{User} case in [Model.Ingest]).
//
// Called by [App.composePrompt] at submit time. Copy-on-write like every mutator
// here. An empty runs slice with an empty fold is a no-op.
func (m Model) CommitShellRuns(runs []shellRun, fold string) Model {
	if len(runs) == 0 && fold == "" {
		return m
	}
	if len(runs) > 0 {
		items := make([]item, 0, len(m.items)+len(runs))
		items = append(items, m.items...)
		for _, r := range runs {
			items = append(items, item{kind: itemShellRun, done: r.done, shell: r})
		}
		m.items = items
	}
	if fold != "" {
		m.pendingEchoFolds = append(append([]string(nil), m.pendingEchoFolds...), fold)
	}
	return m
}

// WithThinking returns a copy of the model whose transcript ends with a muted
// "⋯ working…" indicator when a turn is in flight ([Model.turnActive]) — the
// "something is happening" signal for the gaps where nothing streams (before the
// first token, while a tool runs). It renders IFF the turn is active AND no
// approval/decision is pending: an approval commandeers the footer as "awaiting
// YOU," which is the opposite of "working," so the indicator would be a lie
// there (this is the load-bearing gate). Idle or pending returns the model
// untouched, byte-for-byte, so a session between turns looks exactly as it did
// before this indicator existed.
//
// Like [Model.WithShellRuns] it is a per-render composition appended LAST (below
// the shell/background blocks), never an Ingest: the indicator is derived state
// that must vanish the instant the turn finishes and must never enter the
// durable item list or the exit-flushed transcript. Called last in
// [App.attachModel] so both the frame draw and the mouse-selection measurement
// account for the same tail row.
func (m Model) WithThinking() Model {
	if !m.turnActive || m.pending != nil || m.pendingDec != nil {
		return m
	}
	items := make([]item, 0, len(m.items)+1)
	items = append(items, m.items...)
	m.items = append(items, item{kind: itemThinking, done: false})
	return m
}

// backgroundAgentName is the name a spawned child is listed under: its own
// title when it has one, else its agent identity, else a short form of its id.
// A child's title is derived from the prompt its parent handed it, so it is the
// most specific thing available — unlike the roster row, which is read as a
// column of siblings and so leads with the agent instead (see
// [Overview.rowLabel]). The id fallback exists because a row with neither is
// still a session the operator has to be able to find.
func backgroundAgentName(s SessionInfo) string {
	switch {
	case s.Title != "":
		return s.Title
	case s.Agent != "":
		return s.Agent
	case len(s.ID) > 8:
		return s.ID[:8]
	default:
		return s.ID
	}
}

// approvalBodyLimit resolves the effective approval-prompt body row cap:
// config.DefaultApprovalBodyLines unless a caller plumbed one in through
// [Model.WithApprovalBodyLines]. The resolution lives here, not at the call
// site, so every render path — App.render, a golden test calling View
// directly — agrees on the default.
func (m Model) approvalBodyLimit() int {
	if m.approvalBodyLines <= 0 {
		return config.DefaultApprovalBodyLines
	}
	return m.approvalBodyLines
}

// WithApprovalMinTranscriptRows sets the transcript-row floor the inline
// approval prompt collapses against, reallocating rather than mutating in
// place (Model's copy-on-write discipline). n < 0 means "the config default"
// — see [Model.approvalTranscriptFloor]. [App.render] calls this with its
// always-current tui.approval_min_transcript_rows read.
func (m Model) WithApprovalMinTranscriptRows(n int) Model {
	m.approvalMinTranscriptRows = n
	return m
}

// approvalTranscriptFloor resolves the effective transcript-row floor:
// config.DefaultApprovalMinTranscriptRows unless a caller plumbed one in
// through [Model.WithApprovalMinTranscriptRows]. Resolved here, not at the
// call site, so every render path — App.render, App.transcriptRegion, a golden
// test calling View directly — agrees on the default.
func (m Model) approvalTranscriptFloor() int {
	if m.approvalMinTranscriptRows < 0 {
		return config.DefaultApprovalMinTranscriptRows
	}
	return m.approvalMinTranscriptRows
}

// DismissApproval clears the pending request without resolving it — the esc
// dismiss and the optimistic local clear after a reply. The underlying request
// stays pending server-side; a re-attach replays PermissionRequested and
// re-surfaces it.
func (m Model) DismissApproval() Model {
	m.pending = nil
	return m
}

// IngestDecision applies one [decision.Update] to the prompt state and returns
// the updated Model. It is [Model.Ingest]'s twin for the decision stream: the
// SDK's Event union carries no decision kind (internal/decision's package doc
// has the why), so a structured question arrives on its own subscription and
// lands here rather than in Ingest's event switch. It adds no transcript
// items, which is why the app root ingests it directly instead of through
// [App.ingestAttach]'s autoscroll accounting — there are no new transcript
// lines for a scrolled-back reader's window to be pulled off by.
//
// An [decision.UpdateRequested] opens the prompt, superseding whatever was
// showing — last one shown wins, exactly like a second PermissionRequested;
// the superseded request stays open server-side and its own UpdateResolved
// simply finds m.pendingDec pointed elsewhere. A request carrying no questions
// is ignored rather than opening an empty prompt (decision.Gate.Request
// already rejects one, so this is belt-and-braces against a future transport).
// An [decision.UpdateResolved] clears the prompt only when the ids match: the
// request this client was showing has been answered by another peer, or its
// turn was interrupted and the agent is no longer waiting.
func (m Model) IngestDecision(u decision.Update) Model {
	switch u.Kind {
	case decision.UpdateRequested:
		if len(u.Request.Questions) == 0 {
			return m
		}
		m.pendingDec = &pendingDecision{
			id:      u.Request.ID,
			session: u.Request.SessionID,
			// Shared with the gate's snapshot, which documents Questions as
			// read-only once the request is open — nothing here writes to it.
			// The parallel drafts slice is this client's own, one entry per
			// question, all unanswered until the user picks something.
			questions: u.Request.Questions,
			drafts:    make([]decisionDraft, len(u.Request.Questions)),
		}
	case decision.UpdateResolved:
		if m.pendingDec != nil && m.pendingDec.id == u.Request.ID {
			m.pendingDec = nil
		}
	}
	return m
}

// HasPendingDecision reports whether an unresolved structured-decision request
// is awaiting an answer on this session — the app root consults it to route
// keys to the decision prompt (see App.Update), one rung below the approval
// prompt.
func (m Model) HasPendingDecision() bool { return m.pendingDec != nil }

// PendingDecision returns the id of the pending decision request, for the app
// root to build the [Supervisor.AnswerDecision] call. ok is false when nothing
// is pending.
func (m Model) PendingDecision() (id string, ok bool) {
	if m.pendingDec == nil {
		return "", false
	}
	return m.pendingDec.id, true
}

// DismissDecision clears the pending decision locally. It is ONLY the
// optimistic local clear every resolution makes before its
// [Supervisor.AnswerDecision] call lands — an answer, or esc's cancel (see
// App.cancelDecision). There is deliberately no "dismiss without resolving"
// path: unlike a permission request, which a re-attach re-surfaces off the
// event stream's replay, a decision left open here would leave the agent's turn
// blocked with no prompt on screen and no transcript badge to find it by.
func (m Model) DismissDecision() Model {
	m.pendingDec = nil
	return m
}

// moveDecisionCursor moves the focused row by delta, CLAMPED to the row list
// rather than wrapping: the list is short and ordered, and wrapping from the
// last escape hatch back onto option 1 is precisely the surprise that gets the
// wrong answer sent. A no-op when nothing is pending, or when the question
// offers no rows at all. Reallocates rather than mutating in place (Model's
// copy-on-write discipline), like every mutator here.
func (m Model) moveDecisionCursor(delta int) Model {
	if m.pendingDec == nil {
		return m
	}
	p := *m.pendingDec
	rows := len(p.rows())
	if rows == 0 {
		return m
	}
	p.cursor = clampCursor(p.cursor+delta, rows-1)
	m.pendingDec = &p
	return m
}

// startDecisionTyping activates the free-text row's editor — the first Enter
// on "Type something.". A no-op when nothing is pending or typing is already
// active, so the buffer a user has half-typed is never cleared by a stray
// repeat.
func (m Model) startDecisionTyping() Model {
	if m.pendingDec == nil || m.pendingDec.typing {
		return m
	}
	p := *m.pendingDec
	p.typing = true
	m.pendingDec = &p
	return m
}

// stopDecisionTyping leaves typing mode, discarding the half-typed answer —
// esc's FIRST press while the free-text row is active. A second esc then
// dismisses the whole prompt (see App.handleDecisionKey), so escape never
// throws away more than one thing at a time.
func (m Model) stopDecisionTyping() Model {
	if m.pendingDec == nil {
		return m
	}
	p := *m.pendingDec
	p.typing = false
	p.input = inputBuffer{}
	m.pendingDec = &p
	return m
}

// withDecisionInput replaces the free-text answer's buffer with buf — the
// write-back half of routing a key press through the shared input keymap
// (input_keymap.go), which returns an updated buffer rather than mutating one.
func (m Model) withDecisionInput(buf inputBuffer) Model {
	if m.pendingDec == nil {
		return m
	}
	p := *m.pendingDec
	p.input = buf
	m.pendingDec = &p
	return m
}

// moveDecisionTab moves the focused tab by delta on a multi-question request —
// Tab/shift+tab and ←/→ (see App.handleDecisionKey). A no-op on a
// single-question request, which has no tab strip at all.
//
// Unlike the row cursor, which CLAMPS (moveDecisionCursor: wrapping from the
// last row onto option 1 is how a stray press sends the wrong answer), tab
// movement WRAPS. Switching tabs commits nothing — no answer is sent until the
// Submit row — so the surprise the clamp protects against does not exist here,
// and cycling is what a tab strip and its two end arrows advertise.
//
// Switching leaves both editors, discarding whatever was half-typed in them:
// an editor belongs to the question that opened it, and carrying a half-typed
// answer onto the next question is a worse outcome than losing it. The cursor
// lands on the answer this question already has, if any (see draftRow).
func (m Model) moveDecisionTab(delta int) Model {
	if m.pendingDec == nil || !m.pendingDec.multi() {
		return m
	}
	p := *m.pendingDec
	n := p.tabCount()
	p.tab = ((p.tab+delta)%n + n) % n
	p.typing = false
	p.input = inputBuffer{}
	p.noting = false
	p.notes = inputBuffer{}
	p.cursor = p.draftRow()
	m.pendingDec = &p
	return m
}

// recordDecisionAnswer stores outcome as the focused question's drafted answer.
// It is where EVERY answer lands, single-question and multi alike: the
// single-question path then submits immediately (App.answerDecision), so the
// draft is a moment old rather than long-lived, but there is only one place
// that decides what a question's answer is.
//
// The drafts slice is cloned rather than written through: a pendingDecision
// struct copy shares its backing array, so mutating in place would rewrite the
// answer inside every older Model value too — the one hole a copy-on-write
// discipline built on struct copies leaves open.
func (m Model) recordDecisionAnswer(outcome acp.DecisionOutcome) Model {
	if m.pendingDec == nil {
		return m
	}
	p := *m.pendingDec
	if p.tab < 0 || p.tab >= len(p.drafts) {
		return m
	}
	p.drafts = slices.Clone(p.drafts)
	p.drafts[p.tab].outcome = outcome
	p.typing = false
	p.input = inputBuffer{}
	m.pendingDec = &p
	return m
}

// startDecisionNoting opens the notes editor on the focused question's draft —
// the `n` key. It preloads whatever note that draft already carries, so `n`
// re-opens a note for editing rather than silently restarting it.
func (m Model) startDecisionNoting() Model {
	if m.pendingDec == nil || m.pendingDec.noting {
		return m
	}
	p := *m.pendingDec
	p.noting = true
	p.notes = inputBuffer{}.SetText(p.draft().notes)
	m.pendingDec = &p
	return m
}

// stopDecisionNoting closes the notes editor WITHOUT saving — esc's first press
// while a note is being edited, mirroring stopDecisionTyping. The draft's
// existing note is left exactly as it was.
func (m Model) stopDecisionNoting() Model {
	if m.pendingDec == nil {
		return m
	}
	p := *m.pendingDec
	p.noting = false
	p.notes = inputBuffer{}
	m.pendingDec = &p
	return m
}

// commitDecisionNote saves the notes editor's contents onto the focused
// question's draft and closes the editor — Enter in notes mode. An empty (or
// whitespace-only) note CLEARS whatever note was there, which is the only way
// to take a note back off an answer.
func (m Model) commitDecisionNote() Model {
	if m.pendingDec == nil {
		return m
	}
	p := *m.pendingDec
	// A tab with no draft behind it has nowhere to store the note (the Submit
	// tab, which `n` is not bound on). Close the editor anyway rather than
	// leaving it open with no key that can shut it — an editor Enter cannot
	// close is a wedge.
	if p.tab >= 0 && p.tab < len(p.drafts) {
		p.drafts = slices.Clone(p.drafts) // see recordDecisionAnswer
		p.drafts[p.tab].notes = strings.TrimSpace(p.notes.String())
	}
	p.noting = false
	p.notes = inputBuffer{}
	m.pendingDec = &p
	return m
}

// withDecisionNotes replaces the notes editor's buffer with buf —
// withDecisionInput's twin for the second editor.
func (m Model) withDecisionNotes(buf inputBuffer) Model {
	if m.pendingDec == nil {
		return m
	}
	p := *m.pendingDec
	p.notes = buf
	m.pendingDec = &p
	return m
}

// compactJSON renders raw JSON as a single-line, whitespace-collapsed
// string for compact tool-block display. Invalid or empty input renders as
// an empty string rather than failing.
func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return strings.Join(strings.Fields(string(raw)), " ")
	}
	return buf.String()
}

// summarizeToolInput renders a tool call's compact-JSON input as a readable
// one-line header summary: a shell command's own text for a command-shaped
// input, else the compact JSON as-is. Empty or "{}" (the start-of-call seed a
// provider streams before the arguments arrive) yields "", so the header shows
// the bare tool name until the real input lands on ToolCallFinished.
func summarizeToolInput(compact string) string {
	if compact == "" || compact == "{}" {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(compact), &obj); err != nil {
		return compact
	}
	for _, key := range []string{"command", "cmd"} {
		if v, ok := obj[key].(string); ok && v != "" {
			return v
		}
	}
	return compact
}

// TypeRune inserts r into the input buffer at the cursor.
func (m Model) TypeRune(r rune) Model {
	m.input = m.input.InsertRune(r)
	return m
}

// InsertText inserts s into the input buffer at the cursor — key.Text can in
// principle carry more than one rune (an IME commit).
func (m Model) InsertText(s string) Model {
	m.input = m.input.InsertText(s)
	return m
}

// Backspace removes the rune immediately before the cursor, if any.
func (m Model) Backspace() Model {
	m.input = m.input.Backspace()
	return m
}

// DeleteForward removes the rune at the cursor, if any — Delete/Ctrl+D.
func (m Model) DeleteForward() Model {
	m.input = m.input.DeleteForward()
	return m
}

// MoveLeft/MoveRight move the input cursor one rune.
func (m Model) MoveLeft() Model  { m.input = m.input.MoveLeft(); return m }
func (m Model) MoveRight() Model { m.input = m.input.MoveRight(); return m }

// MoveWordLeft/MoveWordRight move the input cursor one word —
// Alt+Left/Alt+Right.
func (m Model) MoveWordLeft() Model  { m.input = m.input.MoveWordLeft(); return m }
func (m Model) MoveWordRight() Model { m.input = m.input.MoveWordRight(); return m }

// MoveHome/MoveEnd jump the input cursor to the buffer's start/end —
// Home/Ctrl+A and End/Ctrl+E.
func (m Model) MoveHome() Model { m.input = m.input.MoveHome(); return m }
func (m Model) MoveEnd() Model  { m.input = m.input.MoveEnd(); return m }

// DeleteWordBackward deletes the word before the cursor — Alt+Backspace/Ctrl+W.
func (m Model) DeleteWordBackward() Model {
	m.input = m.input.DeleteWordBackward()
	return m
}

// DeleteToLineStart/DeleteToLineEnd delete from the cursor to the buffer's
// start/end — Ctrl+U and Ctrl+K.
func (m Model) DeleteToLineStart() Model {
	m.input = m.input.DeleteToLineStart()
	return m
}

func (m Model) DeleteToLineEnd() Model {
	m.input = m.input.DeleteToLineEnd()
	return m
}

// InputEmpty reports whether the input buffer has no pending text. The app
// root consults this to resolve the navigation contract's left-arrow (← in
// an empty attach input backs out to the overview; with text it edits).
func (m Model) InputEmpty() bool { return m.input.Empty() }

// SetInput replaces the input buffer outright, cursor moving to the end —
// used by the command menu's Enter-select (command_menu.go), which clears
// the buffer wholesale rather than one rune at a time.
func (m Model) SetInput(s string) Model {
	m.input = m.input.SetText(s)
	return m
}

// SetInputCursor replaces the input buffer and places the cursor
// explicitly — used by the command menu's Tab-complete (command_menu.go),
// which splices a completion in place of the active token and wants the
// cursor right after it, not at the end of any trailing text the splice
// left in place.
func (m Model) SetInputCursor(s string, cursor int) Model {
	m.input = m.input.SetTextCursor(s, cursor)
	return m
}

// Submit records the current input buffer as submitted (retrievable via
// [Model.TakeSubmitted]) and clears it. Submitting an empty buffer is a
// no-op: there is nothing to send.
func (m Model) Submit() Model {
	if m.input.Empty() {
		return m
	}
	m.submitted = m.input.String()
	m.hasSubmitted = true
	m.input = inputBuffer{}
	return m
}

// TakeSubmitted returns the text from the most recent [Model.Submit] call
// and clears it, so each submission is observed exactly once. The second
// return value reports whether a submission was pending.
//
// [Model] does not send the text itself; a caller wiring this into a
// live session forwards it as the session's prompt Op.
func (m *Model) TakeSubmitted() (string, bool) {
	if !m.hasSubmitted {
		return "", false
	}
	text := m.submitted
	m.submitted = ""
	m.hasSubmitted = false
	return text, true
}

// transcriptGap is the number of blank lines rendered between consecutive
// transcript blocks (items), for visual breathing room.
const transcriptGap = 1

// transcriptLines renders every item's lines, word-wrapped to width (see
// wrap — a chat body reflows across rows rather than clipping at the edge),
// with transcriptGap blank line(s) between consecutive items — no leading gap
// before the first item, no trailing gap after the last. Shared by View and
// FullTranscript so both surfaces render the transcript body identically. A
// pending approval's badge is skipped — it is shown by the prompt block
// instead (see promptLines), not duplicated inline.
func (m Model) transcriptLines(width int) []string {
	lines := make([]string, 0, len(m.items)+2)
	first := true
	for i, it := range m.items {
		if m.pending != nil && i == m.pending.badgeIdx {
			continue // shown by the pending prompt block, not inline
		}
		if !first {
			for range transcriptGap {
				lines = append(lines, "")
			}
		}
		first = false
		for _, line := range m.renderItemLines(it, width) {
			lines = append(lines, wrap(line, width)...)
		}
	}
	return lines
}

// View renders the transcript and the footer (status + input, or the
// approval prompt) at the given size. Transcript body lines wrap to width
// ([transcriptLines] runs each rendered line through [wrap]); list, status
// and roster rows still clamp with truncate, which is deliberate — a roster
// row should not reflow. Height keeps only the most recent
// lines, tailing the transcript like a live attach. Carries no identity
// header or scroll offset — the plain golden tests that call this directly
// render the transcript alone; [App.render] goes through ViewWithMenu.
func (m Model) View(width, height int) string {
	return m.view(width, height, nil, nil, 0)
}

// ViewWithMenu renders like View but splices menuLines — pre-rendered,
// already width-truncated rows from [commandMenu.Lines] — directly above the
// input box's rule, the same way [Overview.ViewWithMenu] does. headerLines,
// when non-empty, is [attachHeaderLines] (app.go) — the attach screen's own
// copy of the identity chrome every screen shows — prepended to the
// transcript as part of the same scrollable region (see the scroll doc
// below); a pending approval commandeers the whole footer regardless
// (menuLines is always nil then — there is nothing to type into during an
// approval), so menuLines only ever lands above the rule/input/rule block.
// scroll is the manual scroll-back offset (0 = tail-to-latest, the default).
// Called only from App.render.
func (m Model) ViewWithMenu(width, height int, menuLines, headerLines []string, scroll int) string {
	return m.view(width, height, menuLines, headerLines, scroll)
}

func (m Model) view(width, height int, menuLines, headerLines []string, scroll int) string {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}

	// The identity header (when the caller supplies one — only the attach
	// screen does, via ViewWithMenu) joins the transcript as one scrollable
	// document: short content leaves it pinned at the top exactly like
	// before (scrollTail below is then a no-op), but a transcript long
	// enough to overflow avail scrolls the header up and out of view along
	// with the oldest messages — the "header + transcript are the
	// scrollable region" redesign, with the input/footer staying pinned
	// below regardless (untouched by this).
	lines := m.transcriptLines(width)
	if len(headerLines) > 0 {
		combined := make([]string, 0, len(headerLines)+len(lines))
		combined = append(combined, headerLines...)
		combined = append(combined, lines...)
		lines = combined
	}

	// The input box is framed by full-width rules above and below, with the
	// status line beneath it. A pending approval or structured decision
	// commandeers the whole footer: the rules, input box, and status line are
	// suppressed so the prompt block stands alone — whichever footer shows, the transcript
	// above tails to fit so the footer stays anchored to the bottom however
	// long the conversation grows.
	var footer []string
	if prompt := m.promptLines(width, height); prompt != nil {
		footer = prompt
	} else {
		// Plain framing rules top and bottom (round-5 reverted the labeled
		// shell-mode rule): shell mode is now signalled by the sigil leading the
		// input line itself (see inputLine), not by a rule label.
		rule := strings.Repeat("─", width)
		footer = append(footer, menuLines...)
		footer = append(footer, rule, truncate(m.inputLine(), width), rule)
		// The status line carries only usage/cost now, and only once a turn has
		// finished — omit it (no blank row) until then, so the box sits flush
		// against the transcript.
		if status := m.statusLine(); status != "" {
			footer = append(footer, truncate(status, width))
		}
	}

	// The footer — the menu (when open) + input framing + status, or the
	// approval prompt in its place — is pinned to the bottom of the frame:
	// the header+transcript above it scroll-tail to fit when they overflow
	// avail (scrollTail; offset 0 is the existing tail behavior, unchanged)
	// and are padded with blank filler rows when shorter, so the footer
	// lands on height's last row instead of trailing directly beneath a
	// short conversation (chat-style bottom anchoring, matching how
	// [Overview.render] already pads its body before the dispatch bar).
	// avail is floored at 0 rather than left negative — a terminal shorter
	// than the footer alone (the first frame, before WindowSizeMsg arrives,
	// or a tiny window) skips both scrolling and padding instead of
	// underflowing the slice bound scrollTail guards against.
	avail := height - len(footer)
	if avail < 0 {
		avail = 0
	}
	lines = scrollTail(lines, avail, scroll)
	lines = pad(lines, avail)
	lines = append(lines, footer...)
	return strings.Join(lines, "\n")
}

// promptLines renders whichever inline prompt is pending as the bottom-
// anchored, input-replacing block's lines, each truncated to width. Empty when
// nothing is pending. Used by View to anchor the prompt to the bottom.
//
// An approval outranks a decision when both are somehow pending: a permission
// gate blocks a tool call the agent has ALREADY started, while a decision
// blocks it before it has chosen what to do — the in-flight one is the more
// urgent, and the decision prompt is still there once the approval clears
// (its request stays open on the gate regardless of what this client renders).
//
// height is the frame the prompt has to share with the transcript, and is what
// makes the APPROVAL block adaptive: the full prompt runs ~22 rows, so on an
// 80x24 terminal it would leave a two-line transcript and scroll the identity
// header out — the conversation that LED to the gated call disappearing exactly
// when a human needs it to decide. When the full block would leave fewer than
// [Model.approvalTranscriptFloor] rows, the rationale collapses to its opening
// paragraph plus a ctrl+e pointer (see renderApprovalPrompt); everything a
// decision requires — the header, the gated call, the question, the action row
// — is never collapsed, and a frame too short even for that falls to View's
// existing truncate/avail floor rather than to any special case here.
//
// The collapsed block is rendered fresh rather than sliced out of the full
// one: the paragraphs it drops are wrapped text, so cutting rows off a
// rendered block would leave a half-sentence dangling.
//
// The decision prompt has no collapsed form to fall back to — it is already
// only its question plus one row per answer, and every one of those rows is
// load-bearing (a hidden option is an answer the human cannot give) — so it
// ignores height and relies on the same View-level floor.
func (m Model) promptLines(width, height int) []string {
	var raw []string
	switch {
	case m.pending != nil:
		raw = renderApprovalPrompt(m.theme, *m.pending, width, m.approvalBodyLimit(), false)
		if height-len(raw) < m.approvalTranscriptFloor() {
			raw = renderApprovalPrompt(m.theme, *m.pending, width, m.approvalBodyLimit(), true)
		}
	case m.pendingDec != nil:
		raw = renderDecisionPrompt(m.theme, *m.pendingDec, width)
	default:
		return nil
	}
	out := make([]string, len(raw))
	for i, l := range raw {
		out[i] = truncate(l, width)
	}
	return out
}

// markerLine renders a state marker glyph in style, a space, then the
// caller-styled rest of the line. Only the glyph carries the state color — the
// text after a marker keeps its own styling — so the styled-golden layer reads
// the marker as the single source of state truth. Under theme.Test()'s Ascii
// profile style.Render is a no-op, so the plain golden is just "glyph rest".
func markerLine(style lipgloss.Style, glyph, rest string) string {
	return style.Render(glyph) + " " + rest
}

// styledMarkerLines splits text on embedded "\n" — a real multi-paragraph or
// code-block reply is the ordinary case for streamed assistant/reasoning
// content and pasted user input, not a rare one — into one display line per
// physical line: [markerLine] for the first, then each further physical line
// run through render and indented to align under the marker glyph rather than
// left flush or repeating it. render lets callers apply their own per-line
// styling (e.g. reasoning's muted body) the same way the single-line
// markerLine call always could.
//
// Splitting here — never leaving a raw embedded "\n" inside one []string
// entry — is the fix for the streaming top-anchor bug: transcriptLines'
// returned slice LENGTH is what [Model.view]'s height math (avail,
// scrollTail, pad) budgets against, on the assumption that one slice entry is
// one terminal row. A slice entry carrying a raw "\n" prints as more than one
// terminal row while counting as only one against avail — avail/scrollTail
// then under-clip and pad under-fills, so a multi-line item (which real LLM
// output almost always is) silently overflows past the bottom of the frame
// while the header/oldest messages stay wrongly pinned in view: avail
// (wrongly) thought there was still room, so scrollTail (correctly, on the
// slice length it was given) never scrolled anything away. A single-line text
// (the common short case, and every existing fixture) round-trips through
// this unchanged: strings.Split on a string with no "\n" returns a
// one-element slice, so the loop below never runs and this is byte-identical
// to the old single-markerLine call.
func styledMarkerLines(style lipgloss.Style, glyph, text string, render func(string) string) []string {
	parts := strings.Split(text, "\n")
	lines := make([]string, len(parts))
	lines[0] = markerLine(style, glyph, render(parts[0]))
	indent := strings.Repeat(" ", ansi.StringWidth(glyph)+1)
	for i := 1; i < len(parts); i++ {
		lines[i] = indent + render(parts[i])
	}
	return lines
}

// plainRender is the identity [styledMarkerLines] render func for item kinds
// whose continuation lines carry no extra per-line styling beyond the
// marker's — the rest of the line, on every physical row, is exactly the
// item's own text.
func plainRender(s string) string { return s }

// markerBlockLines is [styledMarkerLines]' counterpart for already-rendered
// rows (markdown out of [markdownRenderer.render], which carry their own ANSI
// per row): it fronts the first row with the state marker and indents the
// continuation rows under the glyph, exactly like styledMarkerLines, but takes
// a pre-split []string instead of a "\n"-joined string and — crucially — keeps
// a blank separator row ("") truly blank rather than indenting it to a run of
// spaces, so a markdown block's inter-paragraph gaps stay clean empty rows (no
// trailing whitespace in a golden, nothing stray for a selection to copy).
func markerBlockLines(style lipgloss.Style, glyph string, rows []string) []string {
	if len(rows) == 0 {
		return nil
	}
	out := make([]string, len(rows))
	out[0] = markerLine(style, glyph, rows[0])
	indent := strings.Repeat(" ", ansi.StringWidth(glyph)+1)
	for i := 1; i < len(rows); i++ {
		if rows[i] == "" {
			out[i] = ""
			continue
		}
		out[i] = indent + rows[i]
	}
	return out
}

// renderItemLines renders a single transcript item to its display lines. A
// tool item is a collapsed tree block spanning header + up to three
// result lines; every text-bearing kind renders to one line per physical line
// its content contains (see [styledMarkerLines]) — exactly one for the common
// single-line case. Every kind is marker-only styled: the leading glyph
// carries the state color, the text after it keeps its own styling (plain, or
// muted for reasoning/status body).
func (m Model) renderItemLines(it item, width int) []string {
	switch it.kind {
	case itemAssistantReasoning:
		// Some providers (Claude included) emit a reasoning/thinking block
		// with no content at all — nothing worth rendering, and rendering it
		// anyway would leave a bare marker glyph with no text after it,
		// floating alone between the user's prompt and the reply. Suppress
		// the line entirely rather than show an empty state marker.
		if strings.TrimSpace(it.text) == "" {
			return nil
		}
		muted := m.theme.MutedStyle()
		return styledMarkerLines(m.theme.WarnStyle(), m.theme.GlyphAgent, it.text, func(s string) string { return muted.Render(s) })

	case itemUser:
		return styledMarkerLines(m.theme.InkStyle(), m.theme.GlyphHuman, it.text, plainRender)

	case itemTool:
		return m.renderToolLines(it)

	case itemError:
		return styledMarkerLines(m.theme.DangerStyle(), m.theme.GlyphAgent, it.text, plainRender)

	case itemApproval:
		return []string{markerLine(m.theme.WarnStyle(), m.theme.GlyphAgent, it.text)}

	case itemApprovalResolved:
		style := m.theme.OKStyle()
		if it.approvalVerdict == string(event.VerdictDeny) {
			style = m.theme.DangerStyle()
		}
		return []string{markerLine(style, m.theme.GlyphAgent, "permission "+it.approvalVerdict)}

	case itemBackgroundAgents:
		return m.renderBackgroundAgentLines(it)

	case itemShellRun:
		return m.renderShellRunLines(it.shell)

	case itemThinking:
		// Transient turn-in-flight indicator: a muted `⋯ working…`. Marker-only
		// styling like every other block, but the text is muted too so the whole
		// line reads as subdued chrome, not a message. "working" (not "thinking")
		// because the turn may be running a tool, not only reasoning — a
		// one-token swap if that preference changes.
		muted := m.theme.MutedStyle()
		return []string{markerLine(muted, "⋯", muted.Render("working…"))}

	default: // itemAssistantText
		// Same empty-guard as itemAssistantReasoning above: an assistant-text
		// item with no content yet (or that resolved empty) renders nothing
		// rather than a bare marker.
		if strings.TrimSpace(it.text) == "" {
			return nil
		}
		if !it.done {
			// Streaming: render the raw deltas plainly. Markdown rendering waits
			// for the message to settle (see markdown.go) — re-running glamour on
			// every delta would flicker and lag, and a half-arrived fence or list
			// renders as garbage anyway.
			return styledMarkerLines(m.theme.WarnStyle(), m.theme.GlyphAgent, it.text, plainRender)
		}
		// Settled: render the message's markdown, wrapped to leave room for the
		// "● " marker glyph and the matching continuation indent so a wrapped
		// row still lands within width.
		glyph := m.theme.GlyphAgent
		rows := m.md.render(it.text, width-(ansi.StringWidth(glyph)+1))
		return markerBlockLines(m.theme.OKStyle(), glyph, rows)
	}
}

// attributeHeader appends the originating-agent clause to a tool block's
// header — "ToolName(args) · from the <agent> agent", docs/TUI.md's shape — so
// a transcript interleaving a parent's calls with its subagents' reads
// unambiguously. An empty agent returns header untouched, which is the whole
// fallback contract: a single-agent session (and any daemon predating the
// SDK's Agent field) renders exactly the bytes it rendered before attribution
// existed, with no placeholder standing in for the missing id.
//
// A multi-line header — a heredoc or multi-statement shell command, which
// [summarizeToolInput] passes through verbatim — takes the clause on its FIRST
// physical line. The attribution answers "who is running this?", which a
// reader needs at the top of the block, not buried at the end of a script.
func attributeHeader(header, agent string) string {
	if agent == "" {
		return header
	}
	clause := " · from the " + agent + " agent"
	first, rest, multiline := strings.Cut(header, "\n")
	if !multiline {
		return header + clause
	}
	return first + clause + "\n" + rest
}

// blockRow is one gutter body row of a [contentBlock]: its text and the
// per-row styling to apply to the whole prefixed row (nil = plainRender).
// Per-row styling is what lets the shell block mix a plain output row with a
// danger exit row and a muted disposition row inside one body.
type blockRow struct {
	text   string
	render func(string) string
}

// contentBlock is the shared transcript-block shape: a marker glyph + header,
// then a └-gutter body with aligned continuation and an optional "… +N lines"
// collapse. It is the one place the Claude-Code tool-block grammar lives, so
// the tool call, the background-agents summary, and a `!`/`!!` shell run all
// read the same — and a future block gets the grammar (and, because every
// block built through it is a transcript item inside App.transcriptRegion, the
// drag-selection + OSC 52 copy) for free.
type contentBlock struct {
	marker      lipgloss.Style
	glyph       string // ● (GlyphAgent) or the shell sigil (! / !!)
	header      string // may contain "\n" (heredoc / multi-statement)
	rows        []blockRow
	maxBody     int                 // 0 = show all rows; >0 = show maxBody then collapse
	collapseRen func(string) string // styling for the "… +N lines" row (nil = plainRender)
}

const (
	// blockGutterHead prefixes a content block's FIRST body row (the tree
	// elbow); blockGutterCont prefixes every continuation row. These are the
	// exact indents the collapsed tool tree has always used, so the three
	// unified blocks stay byte-identical to the tool block's original shape.
	blockGutterHead = "   └ "
	blockGutterCont = "     "
)

// renderBlock renders a [contentBlock] to its display lines: the marker+header
// (split per physical line by [styledMarkerLines], so a heredoc header still
// counts one slice entry per row for the height accounting), then the └-gutter
// body. Each shown row is render(prefix+text) — the WHOLE prefixed row is
// styled, gutter included — reproducing the tool block's original
// styleBody("   └ "+…). With maxBody>0 the body shows maxBody rows then a
// single "… +N lines" collapse row; maxBody==0 shows every row (shell output
// is byte-bounded already, so it is never collapsed — the user needs to
// select/copy it whole). Empty rows renders the header alone.
func (m Model) renderBlock(b contentBlock) []string {
	lines := styledMarkerLines(b.marker, b.glyph, b.header, plainRender)

	shown := b.rows
	collapsed := b.maxBody > 0 && len(b.rows) > b.maxBody
	if collapsed {
		shown = b.rows[:b.maxBody]
	}
	for i, row := range shown {
		prefix := blockGutterCont
		if i == 0 {
			prefix = blockGutterHead
		}
		render := row.render
		if render == nil {
			render = plainRender
		}
		lines = append(lines, render(prefix+row.text))
	}
	if collapsed {
		ren := b.collapseRen
		if ren == nil {
			ren = plainRender
		}
		extra := len(b.rows) - b.maxBody
		lines = append(lines, ren(blockGutterCont+fmt.Sprintf("… +%d lines", extra)))
	}
	return lines
}

// renderToolLines renders a tool call as a collapsed tree block: a header
// line, then — once the call has finished with a non-empty result — up to
// three tree-indented result lines, collapsing any remainder into a single
// "… +N lines" line. Marker-only styled: running is yellow, done is green, a
// failed call's marker is red like a session error — the muted body is what
// de-emphasizes the noisy output, not a softer header color. It is one caller
// of the shared [Model.renderBlock] grammar.
func (m Model) renderToolLines(it item) []string {
	style := m.theme.WarnStyle() // running = yellow
	failed := false
	if it.done {
		style = m.theme.OKStyle() // done ok = green
		if it.toolErr {
			style = m.theme.DangerStyle() // tool failure = red
			failed = true
		}
	}

	header := it.toolName
	if summary := summarizeToolInput(it.toolInput); summary != "" {
		header = fmt.Sprintf("%s(%s)", it.toolName, summary)
	}
	header = attributeHeader(header, it.toolAgent)

	// A multi-line command (a heredoc, an inline multi-statement script) is a
	// literal "\n" inside the header — styledMarkerLines (via renderBlock)
	// splits it the same way the transcript's own text items are split, for
	// the same avail/scrollTail height-accounting reason (see its doc). Before
	// the call finishes there is no body to render, so emit the header alone.
	if !it.done || it.toolResult == "" {
		return styledMarkerLines(style, m.theme.GlyphAgent, header, plainRender)
	}

	styleBody := func(s string) string {
		if failed {
			return m.theme.MutedStyle().Render(s)
		}
		return s
	}

	resultLines := strings.Split(it.toolResult, "\n")
	rows := make([]blockRow, len(resultLines))
	for i, l := range resultLines {
		rows[i] = blockRow{text: l, render: styleBody}
	}
	return m.renderBlock(contentBlock{
		marker:      style,
		glyph:       m.theme.GlyphAgent,
		header:      header,
		rows:        rows,
		maxBody:     3,
		collapseRen: styleBody,
	})
}

// renderBackgroundAgentLines renders the background-agents block: a header
// counting the sessions this one spawned, then one tree-indented line per
// child naming it and the agent it runs as. It goes through the shared
// [Model.renderBlock] grammar, so it wears the tool block's "   └ " / "     "
// gutter without re-deriving it.
//
// The marker is accent-styled, not one of the state colors: this block reports
// no state of its own (its children each carry their own, on their own roster
// rows), it is a navigational affordance — which is what the "(↓ to manage)"
// caption points at, the roster being where a child is stopped or drilled into.
func (m Model) renderBackgroundAgentLines(it item) []string {
	if len(it.spawned) == 0 {
		return nil
	}
	header := fmt.Sprintf("%s launched (↓ to manage)", plural(len(it.spawned), "background agent"))
	rows := make([]blockRow, len(it.spawned))
	for i, s := range it.spawned {
		label := s.name
		// A child whose name IS its agent id (an untitled session — see
		// backgroundAgentName) states it once, not twice.
		if s.agent != "" && s.agent != s.name {
			label += " · " + s.agent
		}
		rows[i] = blockRow{text: label}
	}
	return m.renderBlock(contentBlock{
		marker: m.theme.AccentStyle(),
		glyph:  m.theme.GlyphAgent,
		header: header,
		rows:   rows,
	})
}

// shellMarker is a run's block marker: the SIGIL is the marker (round-5) — `!`
// for a run the agent will see, `!!` for a private run it never will — and the
// two wear DISTINCT colors. This is load-bearing for safety: the `· not sent to
// the agent` text line is gone, so the doubled glyph plus a distinct color is
// now the ONLY at-a-glance signal that the agent cannot see a `!!` run. `!` is
// accent (the ordinary "shared" sigil); `!!` is warn — attention-drawing and
// unmistakably apart from `!`, not de-emphasized, because you do not mute your
// only safety signal. Derived from r.inContext for DISPLAY only;
// [App.composePrompt] remains the sole decider of what reaches the model.
func (m Model) shellMarker(r shellRun) (glyph string, style lipgloss.Style) {
	if r.inContext {
		return "!", m.theme.AccentStyle()
	}
	return "!!", m.theme.WarnStyle()
}

// renderShellRunLines renders one `!` / `!!` run (shell.go) as a transcript
// block: the sigil marker + command header, the command's output under the
// `└` gutter, and its outcome — a non-zero exit (`exit N`), a timeout/failure
// note, or a truncation marker, each shown only when there is something
// abnormal to say. A clean exit-0 run shows only its output; there is no
// disposition line at all (round-5 dropped the `· not sent to the agent` text —
// the `!!` marker carries that signal now, see [Model.shellMarker]).
//
// A multi-line command (a heredoc, a pasted multi-statement script) splits the
// same way the transcript's text items split, for the same one-slice-entry-per-
// row height accounting (see styledMarkerLines' doc).
func (m Model) renderShellRunLines(r shellRun) []string {
	glyph, marker := m.shellMarker(r)
	block := contentBlock{marker: marker, glyph: glyph, header: r.line}

	muted := func(s string) string { return m.theme.MutedStyle().Render(s) }
	danger := func(s string) string { return m.theme.DangerStyle().Render(s) }

	if !r.done {
		block.rows = []blockRow{{text: "running…", render: muted}}
		return m.renderBlock(block)
	}

	var rows []blockRow
	// A command that printed nothing (or only trailing newlines) adds no blank
	// output row — TrimRight collapses that to "" so the loop is skipped
	// entirely, rather than emitting a lone indented empty line.
	if body := strings.TrimRight(r.output, "\n"); body != "" {
		for _, l := range strings.Split(body, "\n") {
			rows = append(rows, blockRow{text: l})
		}
	}
	switch {
	case r.note != "":
		rows = append(rows, blockRow{text: r.note, render: danger})
	case r.exitCode != 0:
		rows = append(rows, blockRow{text: fmt.Sprintf("exit %d", r.exitCode), render: danger})
	}
	if r.truncated {
		rows = append(rows, blockRow{text: "… output truncated", render: muted})
	}
	block.rows = rows
	// maxBody stays 0: shell output is byte-bounded already, and the user
	// needs to select/copy it whole — do NOT collapse it.
	return m.renderBlock(block)
}

// statusLine reports the turn's token usage and cost once TurnFinished has
// been seen, muted; it returns "" before then (while streaming, mid tool call,
// or before any turn has finished). The per-line marker colors already carry
// turn/tool state, so a bottom state word would only repeat it — usage/cost is
// the one thing that surfaces nowhere else, so it is all this line shows.
func (m Model) statusLine() string {
	if m.usage == nil {
		return ""
	}
	line := fmt.Sprintf("usage=%din/%dout", m.usage.InputTokens, m.usage.OutputTokens)
	if m.cost != nil {
		line += fmt.Sprintf("  $%.4f", m.cost.USD)
	}
	return m.theme.MutedStyle().Render(line)
}

// inputLine renders the input buffer with the cursor marker spliced in at its
// actual position — mid-text when the cursor sits mid-buffer, not always at the
// end. In shell mode the sigil IS the prompt: the `> ` glyph is dropped and the
// line starts with the accented `!` / `!!` (round-5), so the sigil that triggers
// shell mode is the leading signal. An ordinary prompt keeps `> `.
func (m Model) inputLine() string {
	if strings.HasPrefix(m.input.String(), "!") {
		return shellInputLine(m.theme, m.input, "▏")
	}
	return "> " + shellInputLine(m.theme, m.input, "▏")
}

// FullTranscript renders every transcript item unclipped by height, followed
// by the final usage/cost status line when there is one. It is what the attach
// TUI flushes to the terminal on exit, so the scrollback holds the whole
// conversation — not the viewport-clipped final frame the live view leaves
// behind (the M1 exit-truncation bug). The input line is omitted: there is no
// more input once the program has exited.
func (m Model) FullTranscript(width int) string {
	if width < 1 {
		width = 1
	}
	if len(m.items) == 0 {
		return "" // nothing streamed; nothing to flush
	}

	lines := m.transcriptLines(width)
	if status := m.statusLine(); status != "" {
		lines = append(lines, truncate(status, width))
	}
	return strings.Join(lines, "\n")
}

// truncate clips s to at most w terminal cells (display width, not rune
// count — so ANSI styling is measured correctly, not counted as visible
// width), marking a clipped line with a trailing ellipsis.
func truncate(s string, w int) string {
	if w < 0 {
		w = 0
	}
	if ansi.StringWidth(s) <= w {
		return s
	}
	if w <= 1 {
		return ansi.Truncate(s, w, "")
	}
	return ansi.Truncate(s, w, "…")
}

// wrap word-wraps s to at most w terminal cells per line (display width, not
// rune count — so ANSI styling is measured correctly and wide chars count as
// two), hard-breaking any single token longer than w. It returns one slice
// entry per resulting terminal row, preserving transcriptLines' "one slice
// entry == one terminal row" invariant (see styledMarkerLines) that view's
// height math budgets against — unlike truncate, which clips an over-width
// line to a single row, wrap reflows it across as many rows as it needs.
//
// An empty input yields a single empty line (not zero lines), so a transcript
// gap row stays exactly one blank row. w is floored at 1 (mirroring view):
// ansi.Wrap leaves the string unwrapped for limit < 1, so the guard is ours.
func wrap(s string, w int) []string {
	if w < 1 {
		w = 1
	}
	if s == "" {
		return []string{""}
	}
	return strings.Split(ansi.Wrap(s, w, ""), "\n")
}
