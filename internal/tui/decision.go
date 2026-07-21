package tui

// decision.go holds the pending-decision state Model carries (see
// Model.pendingDec) and its inline render: the structured question an agent
// raised with the `ask_user` tool (internal/decision), drawn as a blank-padded
// block that commandeers the whole footer — status line, rules, and input box
// included — exactly the way a pending approval does (see approval.go, and
// Model.View/promptLines for the commandeering itself).
//
// It is the approval prompt's sibling, not a variant of it. An approval is a
// yes/no verdict on a tool call the agent has already decided to make; a
// decision is the agent handing the choice itself back, so the block carries
// the agent's own question and its options rather than a tool name and its
// args. docs/TUI.md's single-question mockup is the render spec — the layout
// below reproduces it column for column.
//
// PR 1 renders the FIRST question of a request only; the multi-question tabbed
// stepper (also in docs/TUI.md) is a follow-up.

import (
	"fmt"
	"strings"

	"github.com/jedwards1230/agent-sdk-go/acp"

	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// pendingDecision is the state of one unresolved structured-decision request.
// Like pendingApproval it is never exposed by value — Model holds it behind a
// nil-checked pointer (Model.pendingDec) so "no pending decision" and "a
// decision with the zero id" can never be confused, and every mutator
// reallocates rather than writing through the pointer (Model's copy-on-write
// discipline).
//
// Unlike pendingApproval it carries no transcript badge index, because there
// is no badge to suppress: a decision never arrives on the event stream (see
// internal/decision's package doc for why it travels its own transport), so
// nothing was appended to m.items for this prompt to duplicate. The agent's
// own `ask_user` call still renders above as an ordinary in-flight tool item,
// which is correct — that item IS the transcript's record that the turn is
// blocked here.
type pendingDecision struct {
	id      string
	session string
	// question is the request's first question. A multi-question request is
	// still answerable through this prompt — the gate fills the questions this
	// client did not answer in as cancelled (see decision.Gate.Answer) — it
	// just isn't yet renderable as the tabbed stepper the design calls for.
	question acp.DecisionQuestion
	// questionIDs is EVERY question id in the request, including question's
	// own. Esc cancels the whole request, not just the rendered question — the
	// agent is blocked on all of them — so the cancel needs the ids this PR
	// does not yet render. (Gate.Answer would fill the omitted ones in as
	// cancelled anyway; sending them explicitly means the cancel does not
	// depend on that fill-in, and it stays correct once the multi-question
	// stepper answers several at once.)
	questionIDs []string

	// cursor is the index into rows() of the focused row.
	cursor int
	// typing reports whether the free-text row is active and owns typed input.
	// While it is set the prompt's keymap hands everything it doesn't itself
	// bind to the shared input keymap, so digits and arrows edit the answer
	// instead of selecting options (see App.handleDecisionKey).
	typing bool
	// input is the free-text answer being edited — the same cursor-aware
	// [inputBuffer] the attach input and dispatch bar use, so the editing
	// keymap is identical in all three places.
	input inputBuffer
}

// decisionRowKind discriminates one selectable row of the prompt.
type decisionRowKind int

const (
	// decisionRowOption is one of the question's enumerated options.
	decisionRowOption decisionRowKind = iota
	// decisionRowFreeText is the "type something" escape hatch.
	decisionRowFreeText
	// decisionRowChat is the "chat about this" escape hatch, which hands the
	// turn back to the conversation instead of answering.
	decisionRowChat
)

// decisionRow is one selectable row: a kind plus, for an option row, which
// option it draws.
type decisionRow struct {
	kind decisionRowKind
	opt  int // index into question.Options; meaningful for decisionRowOption only
}

// rows returns the prompt's selectable rows in render order — every option
// first, then the free-text and chat escape hatches the question allows. It is
// the single source of truth for BOTH the render and the cursor's bounds, so a
// row can never be drawn without being selectable (or selected without being
// drawn), which is the defect class a second hand-maintained row list invites.
func (p pendingDecision) rows() []decisionRow {
	rows := make([]decisionRow, 0, len(p.question.Options)+2)
	for i := range p.question.Options {
		rows = append(rows, decisionRow{kind: decisionRowOption, opt: i})
	}
	if p.question.AllowFreeText {
		rows = append(rows, decisionRow{kind: decisionRowFreeText})
	}
	if p.question.AllowChat {
		rows = append(rows, decisionRow{kind: decisionRowChat})
	}
	return rows
}

// focused returns the row the cursor sits on. ok is false when the question
// offers no rows at all — a question with no options and both escape hatches
// opted out of, which the tool rejects as malformed but which this render must
// still survive rather than index past the end of.
func (p pendingDecision) focused() (decisionRow, bool) {
	rows := p.rows()
	if p.cursor < 0 || p.cursor >= len(rows) {
		return decisionRow{}, false
	}
	return rows[p.cursor], true
}

// hint returns the prompt's dim footer key hint. Typing mode advertises a
// different contract (the second Enter submits the typed answer, Esc backs out
// of the editor rather than out of the prompt), so it says so — a key hint
// that lies about what Enter does is worse than none.
func (p pendingDecision) hint() string {
	if p.typing {
		return "Enter to submit · Esc to cancel"
	}
	return "Enter to select · ↑/↓ to navigate · Esc to cancel"
}

// The prompt's fixed vocabulary. The gutter is exactly as wide as the caret
// plus its space, so focusing a row never shifts the columns beneath it; the
// rationale indent puts the sub-line two cells past its option's label, which
// is what makes it read as belonging to that option rather than to the list.
const (
	decisionChip             = "decision"
	decisionCaret            = "▸" // the same selection caret the roster, command menu, /config and /model rows use
	decisionGutter           = "  "
	decisionRationaleIndent  = "       "
	decisionFreeTextGlyph    = "›"
	decisionFreeTextLabel    = "Type something."
	decisionChatGlyph        = "↳"
	decisionChatLabel        = "Chat about this"
	decisionRecommendedLabel = "(Recommended)"
	decisionCursorGlyph      = "▏" // matches Model.inputLine's cursor
)

// renderDecisionPrompt renders p as the inline decision prompt's blank-padded
// block at the given width, reproducing docs/TUI.md's single-question mockup:
// a full-width rule, a "decision <title>" chip header, a blank separator, the
// question, another blank separator, the numbered options (each with its dim
// indented rationale sub-line when it has one), a blank separator, the
// free-text and chat escape-hatch rows, and a dim footer key hint.
//
// Only the state-bearing tokens are colored, following the transcript's
// marker-only styling discipline: the "decision" chip carries the same warn
// style the approval header uses (both mean "this session is blocked on you"),
// the focused row's caret and an option's "(Recommended)" suffix carry the
// accent, and the rationale/hint are muted. The question, the titles and the
// labels stay unstyled — they are content, not state — so an Ascii golden
// still reads as the mockup does.
//
// No leading blank line: [App.render]'s [layout.TopPadding] already supplies
// the frame's top margin, and Model.View stacks this block straight onto the
// transcript above it. Like renderApprovalPrompt this is plain multi-line text
// composed top to bottom, never composited by absolute display-column
// splicing, and width < 1 floors to 1 so the rule can never strings.Repeat a
// negative count.
func renderDecisionPrompt(th theme.Theme, p pendingDecision, width int) []string {
	if width < 1 {
		width = 1
	}
	q := p.question

	header := th.WarnStyle().Render(decisionChip)
	if q.Title != "" {
		header += "   " + q.Title
	}
	lines := []string{
		strings.Repeat("─", width),
		header,
		"",
		q.Question,
		"",
	}

	rows := p.rows()
	for i, row := range rows {
		// One blank row separates the numbered options from the escape
		// hatches beneath them, per the mockup. Emitted at the boundary rather
		// than unconditionally, so a question with no options at all (free
		// text only) doesn't open with two blank rows.
		if row.kind != decisionRowOption && i > 0 && rows[i-1].kind == decisionRowOption {
			lines = append(lines, "")
		}

		gutter := decisionGutter
		if i == p.cursor {
			gutter = th.AccentStyle().Render(decisionCaret) + " "
		}

		switch row.kind {
		case decisionRowOption:
			opt := q.Options[row.opt]
			label := opt.Label
			if opt.Recommended {
				label += "  " + th.AccentStyle().Render(decisionRecommendedLabel)
			}
			// The digit is the option's own 1-based position, which is also
			// what the 1-9 keys select — see App.selectDecisionOption.
			lines = append(lines, fmt.Sprintf("%s%d  %s", gutter, row.opt+1, label))
			if opt.Rationale != "" {
				lines = append(lines, th.MutedStyle().Render(decisionRationaleIndent+opt.Rationale))
			}

		case decisionRowFreeText:
			body := decisionFreeTextLabel
			if p.typing {
				// The placeholder gives way to the live buffer with its cursor
				// spliced in at the real position, exactly as the attach input
				// renders it.
				body = p.input.Render(decisionCursorGlyph)
			}
			lines = append(lines, gutter+decisionFreeTextGlyph+" "+body)

		case decisionRowChat:
			lines = append(lines, gutter+decisionChatGlyph+" "+decisionChatLabel)
		}
	}

	return append(lines, "", th.MutedStyle().Render(p.hint()))
}
