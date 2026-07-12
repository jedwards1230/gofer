// Package tui is gofer's minimal attach surface: an ordered transcript, an
// input buffer, and a status line, rendered as a projection of a session's
// typed Event stream (per docs/CONTRACT.md's Event/Op contract in
// agent-sdk-go).
//
// [Model] is the pure, headlessly-testable core — it has no bubbletea
// dependency. This is the seed of the full screen-stack design in
// docs/TUI.md (overview ⇄ peek ⇄ attach); the navigation, dialogs, and
// keymap system land in M2+.
package tui

import (
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
)

// item is one entry in the transcript.
type item struct {
	kind itemKind
	text string // settled/streaming content
	done bool   // MessageFinished has been seen
}

// Model is gofer's minimal attach surface. It is immutable from the
// caller's perspective: [Model.Ingest] returns an updated copy rather than
// mutating in place, so a fixed event sequence replays to the same
// rendered output in every test.
type Model struct {
	theme theme.Theme

	items []item

	// openText indexes into items for the text message currently
	// streaming, or -1 when none is open.
	openText int

	turn       turnState
	stopReason string
	usage      *provider.Usage
	cost       *provider.Cost

	input string
}

// New returns an empty Model rendering through th.
func New(th theme.Theme) Model {
	return Model{theme: th, openText: -1}
}

// Ingest applies e to the transcript and returns the updated Model. Event
// kinds the minimal attach surface doesn't yet render are accepted and
// ignored, so a caller can forward the full stream unfiltered.
func (m Model) Ingest(e event.Event) Model {
	m.items = append([]item(nil), m.items...)

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
		m.items = append(m.items, item{kind: itemAssistantText})
		m.openText = idx

	case event.MessageDelta:
		if m.openText >= 0 && m.openText < len(m.items) {
			m.items[m.openText].text += ev.Text
		}

	case event.MessageFinished:
		if m.openText >= 0 && m.openText < len(m.items) {
			m.items[m.openText].text = ev.Content
			m.items[m.openText].done = true
		}
		m.openText = -1
	}

	return m
}

// View renders the transcript, status line, and input line at the given
// size. A line longer than width is truncated; height keeps only the most
// recent lines, tailing the transcript like a live attach.
func (m Model) View(width, height int) string {
	if width < 1 {
		width = 1
	}
	if height < 1 {
		height = 1
	}

	lines := make([]string, 0, len(m.items)+2)
	for _, it := range m.items {
		lines = append(lines, truncate(it.text, width))
	}

	const reserved = 2 // status line + input line
	if avail := height - reserved; avail >= 0 && len(lines) > avail {
		lines = lines[len(lines)-avail:]
	}

	lines = append(lines, truncate(m.statusLine(), width), truncate(m.inputLine(), width))
	return strings.Join(lines, "\n")
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
