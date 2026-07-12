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
// attach); the navigation, dialogs, and keymap system land in M2+.
package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// turnState is the attach surface's coarse turn lifecycle, shown in the
// status line.
type turnState int

const (
	turnIdle turnState = iota
	turnStreaming
)

// itemKind distinguishes transcript item shapes.
type itemKind int

const (
	itemAssistantText itemKind = iota
	itemAssistantReasoning
	itemTool
	itemError
	itemApproval
)

// item is one entry in the transcript. Tool-only fields are zero on every
// other kind.
type item struct {
	kind itemKind
	text string // settled/streaming content for text, reasoning, error, approval
	done bool   // MessageFinished / ToolCallFinished has been seen

	toolName   string
	toolInput  string
	toolResult string
}

// Model is gofer's minimal attach surface. It is immutable from the
// caller's perspective: [Model.Ingest] and the input-editing methods return
// an updated copy rather than mutating in place, so a fixed event sequence
// replays to the same rendered output in every test.
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

	turn       turnState
	stopReason string
	usage      *provider.Usage
	cost       *provider.Cost

	input string

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

	switch ev := e.(type) {
	case event.TurnStarted:
		m.turn = turnStreaming

	case event.TurnFinished:
		m.turn = turnIdle
		m.stopReason = ev.StopReason
		usage := ev.Usage
		m.usage = &usage
		m.cost = ev.Cost

	case event.MessageStarted:
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

	case event.ToolCallDelta:
		if idx, ok := m.toolIndex[ev.ID]; ok {
			m.items[idx].toolResult += ev.Delta
		}

	case event.ToolCallFinished:
		if idx, ok := m.toolIndex[ev.ID]; ok {
			m.items[idx].toolResult = ev.Result
			m.items[idx].done = true
		}
		delete(m.toolIndex, ev.ID)

	case event.SessionError:
		m.items = append(m.items, item{kind: itemError, text: ev.Err, done: true})

	case event.PermissionRequested:
		m.items = append(m.items, item{kind: itemApproval, text: ev.Tool, done: true})

		// event.SessionCreated, event.SessionResumed, event.SessionForked,
		// event.SessionCompacted, event.SessionKilled, event.SessionArchived,
		// and event.PermissionResolved carry no transcript-visible state in
		// the minimal attach surface; they fall through untouched.
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

// TypeRune appends r to the input buffer.
func (m Model) TypeRune(r rune) Model {
	m.input += string(r)
	return m
}

// Backspace removes the last rune in the input buffer, if any.
func (m Model) Backspace() Model {
	if m.input == "" {
		return m
	}
	runes := []rune(m.input)
	m.input = string(runes[:len(runes)-1])
	return m
}

// Submit records the current input buffer as submitted (retrievable via
// [Model.TakeSubmitted]) and clears it. Submitting an empty buffer is a
// no-op: there is nothing to send.
func (m Model) Submit() Model {
	if m.input == "" {
		return m
	}
	m.submitted = m.input
	m.hasSubmitted = true
	m.input = ""
	return m
}

// TakeSubmitted returns the text from the most recent [Model.Submit] call
// and clears it, so each submission is observed exactly once. The second
// return value reports whether a submission was pending.
//
// There is no daemon in M1 to send the text to; a caller wiring this into a
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

// View renders the transcript, status line, and input line at the given
// size. Width wraps nothing (M1's virtualized transcript and stable-prefix
// markdown cache from docs/TUI.md land later); a line longer than width is
// truncated. Height keeps only the most recent lines, tailing the
// transcript like a live attach.
func (m Model) View(width, height int) string {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}

	lines := make([]string, 0, len(m.items)+2)
	for _, it := range m.items {
		lines = append(lines, truncate(m.renderItem(it), width))
	}

	const reserved = 2 // status line + input line
	if avail := height - reserved; avail >= 0 && len(lines) > avail {
		lines = lines[len(lines)-avail:]
	}

	lines = append(lines, truncate(m.statusLine(), width), truncate(m.inputLine(), width))
	return strings.Join(lines, "\n")
}

// renderItem renders a single transcript item to one line.
func (m Model) renderItem(it item) string {
	switch it.kind {
	case itemAssistantReasoning:
		return m.theme.MutedStyle().Render("» " + it.text)

	case itemTool:
		glyph := m.theme.GlyphStreaming
		if it.done {
			glyph = m.theme.GlyphOK
		}
		line := fmt.Sprintf("%s %s(%s)", glyph, it.toolName, it.toolInput)
		if it.done {
			line += " → " + it.toolResult
		}
		return line

	case itemError:
		return m.theme.DangerStyle().Render(m.theme.GlyphErr + " " + it.text)

	case itemApproval:
		return m.theme.WarnStyle().Render(m.theme.GlyphApproval + " " + it.text)

	default: // itemAssistantText
		return it.text
	}
}

// statusLine reports the turn's lifecycle state and, once TurnFinished has
// been seen, its stop reason, usage, and cost.
func (m Model) statusLine() string {
	glyph, label := m.theme.GlyphIdle, "idle"
	if m.turn == turnStreaming {
		glyph, label = m.theme.GlyphStreaming, "streaming"
	}
	line := glyph + " " + label
	if m.usage != nil {
		line += fmt.Sprintf("  stop=%s  usage=%din/%dout", m.stopReason, m.usage.InputTokens, m.usage.OutputTokens)
		if m.cost != nil {
			line += fmt.Sprintf("  $%.4f", m.cost.USD)
		}
	}
	return m.theme.MutedStyle().Render(line)
}

// inputLine renders the input buffer with a trailing cursor marker.
func (m Model) inputLine() string {
	return "> " + m.input + "▏"
}

// truncate clips s to at most w runes, marking a clipped line with a
// trailing ellipsis.
func truncate(s string, w int) string {
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	if w <= 1 {
		return string(r[:w])
	}
	return string(r[:w-1]) + "…"
}
