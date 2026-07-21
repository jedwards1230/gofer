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
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

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
)

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

	// approvalVerdict is itemApprovalResolved-only: the resolved
	// event.Verdict ("allow"/"deny").
	approvalVerdict string
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
}

// New returns an empty Model rendering through th.
func New(th theme.Theme) Model {
	return Model{
		theme:         th,
		openText:      -1,
		openReasoning: -1,
		toolIndex:     map[string]int{},
		toolAgents:    map[string]string{},
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

	switch ev := e.(type) {
	case event.TurnFinished:
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
			m.items = append(m.items, item{kind: itemUser, text: ev.Content, done: true})
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

// WithApprovalBodyLines sets the row cap the inline approval prompt's body
// honors, reallocating rather than mutating in place (Model's copy-on-write
// discipline). n <= 0 means "the config default" — see [Model.approvalBodyLimit].
// [App.render] calls this with its always-current tui.approval_body_lines
// read; a Model nobody plumbs config into keeps the default.
func (m Model) WithApprovalBodyLines(n int) Model {
	m.approvalBodyLines = n
	return m
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
		ids := make([]string, len(u.Request.Questions))
		for i, q := range u.Request.Questions {
			ids[i] = q.QuestionID
		}
		m.pendingDec = &pendingDecision{
			id:          u.Request.ID,
			session:     u.Request.SessionID,
			question:    u.Request.Questions[0],
			questionIDs: ids,
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
		for _, line := range m.renderItemLines(it) {
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
	if prompt := m.promptLines(width); prompt != nil {
		footer = prompt
	} else {
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
func (m Model) promptLines(width int) []string {
	var raw []string
	switch {
	case m.pending != nil:
		raw = renderApprovalPrompt(m.theme, *m.pending, width, m.approvalBodyLimit())
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

// renderItemLines renders a single transcript item to its display lines. A
// tool item is a collapsed tree block spanning header + up to three
// result lines; every text-bearing kind renders to one line per physical line
// its content contains (see [styledMarkerLines]) — exactly one for the common
// single-line case. Every kind is marker-only styled: the leading glyph
// carries the state color, the text after it keeps its own styling (plain, or
// muted for reasoning/status body).
func (m Model) renderItemLines(it item) []string {
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

	default: // itemAssistantText
		// Same empty-guard as itemAssistantReasoning above: an assistant-text
		// item with no content yet (or that resolved empty) renders nothing
		// rather than a bare marker.
		if strings.TrimSpace(it.text) == "" {
			return nil
		}
		style := m.theme.WarnStyle()
		if it.done {
			style = m.theme.OKStyle()
		}
		return styledMarkerLines(style, m.theme.GlyphAgent, it.text, plainRender)
	}
}

// renderToolLines renders a tool call as a collapsed tree block: a header
// line, then — once the call has finished with a non-empty result — up to
// three tree-indented result lines, collapsing any remainder into a single
// "… +N lines" line. Marker-only styled: running is yellow, done is green, a
// failed call's marker is red like a session error — the muted body is what
// de-emphasizes the noisy output, not a softer header color.
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
	// A multi-line command (a heredoc, an inline multi-statement script) is a
	// literal "\n" inside summary, not a rare shape for a bash tool call —
	// styledMarkerLines splits it the same way the transcript's own text
	// items are split above, for the same avail/scrollTail height-accounting
	// reason (see its doc).
	lines := styledMarkerLines(style, m.theme.GlyphAgent, header, plainRender)

	if !it.done || it.toolResult == "" {
		return lines
	}

	styleBody := func(s string) string {
		if failed {
			return m.theme.MutedStyle().Render(s)
		}
		return s
	}

	resultLines := strings.Split(it.toolResult, "\n")
	lines = append(lines, styleBody("   └ "+resultLines[0]))
	const maxExtra = 2
	shown := 1
	for _, l := range resultLines[1:] {
		if shown >= 1+maxExtra {
			break
		}
		lines = append(lines, styleBody("     "+l))
		shown++
	}
	if extra := len(resultLines) - shown; extra > 0 {
		lines = append(lines, styleBody(fmt.Sprintf("     … +%d lines", extra)))
	}
	return lines
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

// inputLine renders the input buffer with the cursor marker spliced in at
// its actual position — mid-text when the cursor sits mid-buffer, not
// always at the end.
func (m Model) inputLine() string {
	return "> " + m.input.Render("▏")
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
