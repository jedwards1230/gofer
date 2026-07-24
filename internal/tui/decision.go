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
// args. docs/TUI.md's mockups are the render spec — the layout below
// reproduces them column for column.
//
// # One widget, two shapes
//
// A request carrying ONE question renders exactly as it shipped: chip header,
// question, numbered options, escape hatches, hint. A request carrying SEVERAL
// grows a tab strip (one tab per question plus a final Submit tab), a side
// panel for the focused question's supporting context, and a notes editor —
// and, crucially, stops answering on every keystroke: each question's answer
// accumulates as a DRAFT and the Submit tab commits the whole set in one
// Supervisor.AnswerDecision call.
//
// The multi-question affordances are gated on len(questions) > 1 rather than
// rendered degenerately for one, because the single-question layout is a
// shipped contract with a golden behind it (docs/TUI.md, "Single question") —
// a tab strip carrying a single tab, or a Submit tab for one answer, would be
// noise on the common case.

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/jedwards1230/agent-sdk-go/acp"

	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// pendingDecision is the state of one unresolved structured-decision request.
// Like pendingApproval it is never exposed by value — Model holds it behind a
// nil-checked pointer (Model.pendingDec) so "no pending decision" and "a
// decision with the zero id" can never be confused, and every mutator
// reallocates rather than writing through the pointer (Model's copy-on-write
// discipline). The drafts slice is the one place that discipline needs care:
// a struct copy shares its backing array, so every mutator that writes a draft
// clones the slice first (see Model.recordDecisionAnswer).
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
	// questions is EVERY question in the request, in the order the agent asked
	// them — which is also the order the answers go back in, the order the tab
	// strip draws, and the order internal/decision's gate normalizes against.
	// Shared read-only with the gate's own snapshot (decision.Request.Questions
	// is documented as read-only once open), so nothing here ever writes to it.
	questions []acp.DecisionQuestion
	// drafts is index-aligned with questions: drafts[i] is the answer being
	// composed for questions[i], and is committed to the gate only by a Submit.
	// A nil Outcome means "not answered yet", which is exactly what the tab
	// strip's ☐/✔ reports and what a submit omits — see submitAnswers.
	drafts []decisionDraft
	// tab is the focused tab: a question index in [0, len(questions)), or
	// len(questions) for the Submit tab. It is always 0 on a single-question
	// request, which has no tab strip and no Submit tab (see multi).
	tab int

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
	// noting reports whether the NOTES editor is active (the `n` key, multi
	// only). It is mutually exclusive with typing by construction: `n` is bound
	// only while neither is active, and the notes editor claims Enter/Esc
	// before the row keymap can start the free-text one.
	noting bool
	// notes is the note being edited, preloaded with whatever note the focused
	// question's draft already carries so `n` re-opens rather than restarts.
	notes inputBuffer
}

// decisionDraft is one question's in-progress answer: the outcome chosen so far
// (nil until the user picks something) plus any note attached with `n`. It
// becomes an [acp.DecisionAnswer] only at submit time (see submitAnswers) —
// holding it here rather than sending each choice as it is made is what makes a
// batch of questions ONE round trip instead of N.
type decisionDraft struct {
	outcome acp.DecisionOutcome
	notes   string
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
	// decisionRowSubmit is the Submit tab's single actionable row: the one that
	// commits every draft in the request. It exists as a ROW rather than as a
	// bare key binding so the Submit tab flows through the same rows()/cursor/
	// Enter machinery as every question tab — one dispatch path, not two.
	decisionRowSubmit
)

// decisionRow is one selectable row: a kind plus, for an option row, which
// option it draws.
type decisionRow struct {
	kind decisionRowKind
	opt  int // index into the focused question's Options; decisionRowOption only
}

// multi reports whether this request carries several questions — the switch
// between the shipped single-question layout and the tabbed multi-question one.
// Every multi-only affordance (the tab strip, the Submit tab, the side panel,
// `n`, Tab/shift+tab, ←/→) is gated on it.
func (p pendingDecision) multi() bool { return len(p.questions) > 1 }

// tabCount is the number of tabs in the strip: one per question plus the final
// Submit tab. A single-question request has exactly one (conceptual) tab and no
// strip at all.
func (p pendingDecision) tabCount() int {
	if !p.multi() {
		return 1
	}
	return len(p.questions) + 1
}

// onSubmitTab reports whether the final commit tab is focused.
func (p pendingDecision) onSubmitTab() bool { return p.multi() && p.tab >= len(p.questions) }

// question returns the question the focused tab renders. It returns the zero
// question on the Submit tab (and for an out-of-range tab, which nothing can
// produce today): a zero question offers no options and no escape hatches, so
// every caller degrades to "nothing to draw, nothing to select" rather than
// indexing past the end of a slice.
func (p pendingDecision) question() acp.DecisionQuestion {
	if p.tab < 0 || p.tab >= len(p.questions) {
		return acp.DecisionQuestion{}
	}
	return p.questions[p.tab]
}

// draft returns the focused question's draft answer, or the zero draft on the
// Submit tab.
func (p pendingDecision) draft() decisionDraft {
	if p.tab < 0 || p.tab >= len(p.drafts) {
		return decisionDraft{}
	}
	return p.drafts[p.tab]
}

// questionIDs returns every question id in the request, in question order —
// what App.cancelDecision names when esc withdraws from the whole batch.
func (p pendingDecision) questionIDs() []string {
	ids := make([]string, len(p.questions))
	for i, q := range p.questions {
		ids[i] = q.QuestionID
	}
	return ids
}

// answeredCount is how many questions carry a chosen outcome — the numerator
// the Submit tab reports.
func (p pendingDecision) answeredCount() int {
	n := 0
	for _, d := range p.drafts {
		if d.outcome != nil {
			n++
		}
	}
	return n
}

// submitAnswers builds the answers a Submit sends: one [acp.DecisionAnswer] per
// question that has something to say, in question order.
//
// A question with no outcome is OMITTED rather than sent as cancelled — the
// gate already fills every omitted question in as [acp.DecisionOutcomeCancelled]
// (decision.Gate.Answer's normalize), and re-implementing that here would mean
// two places deciding what an unanswered question means.
//
// The one exception is a question carrying a note but no outcome: omitting it
// would silently drop text the user typed, so it goes out as an explicit
// cancelled answer WITH the note attached. "I'm not choosing, and here is why"
// is a real thing to want to tell an agent, and cancelled is always a valid
// outcome (decision.validateOutcome).
func (p pendingDecision) submitAnswers() []acp.DecisionAnswer {
	answers := make([]acp.DecisionAnswer, 0, len(p.questions))
	for i, q := range p.questions {
		if i >= len(p.drafts) {
			break
		}
		d := p.drafts[i]
		switch {
		case d.outcome != nil:
			answers = append(answers, acp.DecisionAnswer{QuestionID: q.QuestionID, Outcome: d.outcome, Notes: d.notes})
		case d.notes != "":
			answers = append(answers, acp.DecisionAnswer{QuestionID: q.QuestionID, Outcome: acp.DecisionOutcomeCancelled{}, Notes: d.notes})
		}
	}
	return answers
}

// rows returns the prompt's selectable rows in render order — every option
// first, then the free-text and chat escape hatches the question allows, or the
// single Submit row on the commit tab. It is the single source of truth for
// BOTH the render and the cursor's bounds, so a row can never be drawn without
// being selectable (or selected without being drawn), which is the defect class
// a second hand-maintained row list invites.
func (p pendingDecision) rows() []decisionRow {
	if p.onSubmitTab() {
		return []decisionRow{{kind: decisionRowSubmit}}
	}
	q := p.question()
	rows := make([]decisionRow, 0, len(q.Options)+2)
	for i := range q.Options {
		rows = append(rows, decisionRow{kind: decisionRowOption, opt: i})
	}
	if q.AllowFreeText {
		rows = append(rows, decisionRow{kind: decisionRowFreeText})
	}
	if q.AllowChat {
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

// draftRow returns the index into rows() of the row holding the focused
// question's drafted answer, or 0 when it has none. Switching to a tab lands
// the cursor here (see Model.moveDecisionTab) so coming back to an answered
// question SHOWS you what you picked, rather than resetting to option 1 and
// inviting you to answer it a second time.
func (p pendingDecision) draftRow() int {
	outcome := p.draft().outcome
	if outcome == nil {
		return 0
	}
	q := p.question()
	for i, row := range p.rows() {
		switch o := outcome.(type) {
		case acp.DecisionOutcomeSelected:
			if row.kind == decisionRowOption && q.Options[row.opt].OptionID == o.OptionID {
				return i
			}
		case acp.DecisionOutcomeText:
			if row.kind == decisionRowFreeText {
				return i
			}
		case acp.DecisionOutcomeChat:
			if row.kind == decisionRowChat {
				return i
			}
		}
	}
	return 0
}

// chosenOption reports whether the focused question's draft selected option i —
// what puts the ✔ marker beside an already-answered row.
func (p pendingDecision) chosenOption(i int) bool {
	sel, ok := p.draft().outcome.(acp.DecisionOutcomeSelected)
	if !ok {
		return false
	}
	opts := p.question().Options
	return i >= 0 && i < len(opts) && opts[i].OptionID == sel.OptionID
}

// hint returns the prompt's dim footer key hint, which always describes the
// keys that are actually live right now. An editor advertises a different
// contract than the row list (Enter commits what was typed, Esc backs out of
// the editor rather than out of the prompt), and the multi-question shape binds
// two keys the single-question one does not — a key hint that lies about what
// Enter does is worse than none.
func (p pendingDecision) hint() string {
	switch {
	case p.noting:
		return "Enter to save the note · Esc to discard"
	case p.typing && p.multi():
		// Multi-question: Enter records a draft, it does not submit the request.
		return "Enter to save · Esc to discard"
	case p.typing:
		return "Enter to submit · Esc to cancel"
	case p.onSubmitTab():
		return "Enter to submit · Tab to switch questions · Esc to cancel"
	case p.multi():
		return "Enter to select · ↑/↓ to navigate · n to add notes · Tab to switch questions · Esc to cancel"
	default:
		return "Enter to select · ↑/↓ to navigate · Esc to cancel"
	}
}

// The prompt's fixed vocabulary. The focus caret and its gutter are the shared
// [choiceCaret]/[choiceGutter] (choicelist.go) — the same selection marker the
// approval prompt and every other list here use; the rationale indent puts the
// sub-line two cells past its option's label, which is what makes it read as
// belonging to that option rather than to the list.
const (
	decisionChip             = "decision"
	decisionRationaleIndent  = "       "
	decisionFreeTextGlyph    = "›"
	decisionFreeTextLabel    = "Type something."
	decisionChatGlyph        = "↳"
	decisionChatLabel        = "Chat about this"
	decisionRecommendedLabel = "(Recommended)"
	decisionCursorGlyph      = "▏" // matches Model.inputLine's cursor
)

// The multi-question vocabulary: the tab strip's checkboxes and end arrows, the
// side panel's divider and section headings, and the Submit tab's wording.
const (
	decisionAnsweredGlyph   = "✔"
	decisionUnansweredGlyph = "□"
	decisionPrevGlyph       = "←"
	decisionNextGlyph       = "→"
	decisionTabGap          = "   "
	decisionSubmitLabel     = "Submit"
	decisionUnansweredNote  = "not answered — cancelled on submit"
	decisionPanelDivider    = "│"
	decisionPanelContext    = "context"
	decisionPanelReference  = "reference"
	decisionPanelNotes      = "notes"
)

// The side panel's width budget. The panel takes about a third of the frame,
// clamped so it is never too narrow to read a wrapped sentence in and never so
// wide it starves the option column; when what's left for the options would
// fall below decisionOptionMinWidth the split collapses entirely and the panel
// stacks BENEATH the options instead (see questionBody) — a narrow terminal
// loses the two-column layout, never the content.
const (
	decisionPanelMinWidth  = 20
	decisionPanelMaxWidth  = 36
	decisionOptionMinWidth = 24
	decisionPanelGap       = 3 // the " │ " between the columns
)

// renderDecisionPrompt renders p as the inline decision prompt's blank-padded
// block at the given width, reproducing docs/TUI.md's mockups: a full-width
// rule, a "decision <title>" chip header, the tab strip when the request
// carries several questions, a blank separator, the focused question, another
// blank separator, the numbered options (each with its dim indented rationale
// sub-line when it has one) beside the side panel, a blank separator, the
// free-text and chat escape-hatch rows, and a dim footer key hint.
//
// Only the state-bearing tokens are colored, following the transcript's
// marker-only styling discipline: the "decision" chip carries the same warn
// style the approval header uses (both mean "this session is blocked on you"),
// the focused row's caret, an option's "(Recommended)" suffix and an answered
// tab's ✔ carry the accent, and the rationale/panel headings/hint are muted. The
// question, the titles and the labels stay unstyled — they are content, not
// state — so an Ascii golden still reads as the mockup does.
//
// No leading blank line: [App.render]'s [layout.TopPadding] already supplies
// the frame's top margin, and Model.View stacks this block straight onto the
// transcript above it. Unlike renderApprovalPrompt this block is not purely
// top-to-bottom text — the side panel is a genuine two-column composition — so
// the join pads by DISPLAY width (padTo), never by byte or rune count, which is
// the #61 defect class the colored narrow-width test guards. width < 1 floors
// to 1 so the rule can never strings.Repeat a negative count.
func renderDecisionPrompt(th theme.Theme, p pendingDecision, width int) []string {
	if width < 1 {
		width = 1
	}

	// A "·" separator, not a run of spaces, joins the chip to its title: three
	// spaces between two content words reads as two tab cells (the multi strip's
	// own spacing), so a single-question header "decision   Choose a task" was
	// being mistaken for a two-question strip. The bullet binds them as one
	// "decision · <title>" label instead — and a single question still draws no
	// tab strip at all (only p.multi() does, below).
	header := th.WarnStyle().Render(decisionChip)
	if title := p.chipTitle(); title != "" {
		header += th.MutedStyle().Render(" · ") + title
	}
	lines := []string{
		strings.Repeat("─", width),
		header,
	}
	if p.multi() {
		lines = append(lines, p.tabStrip(th, width))
	}
	lines = append(lines, "")

	if p.onSubmitTab() {
		lines = append(lines, p.submitBody(th, width)...)
	} else {
		lines = append(lines, p.questionBody(th, width)...)
	}

	lines = append(lines, "")
	for _, l := range wrap(p.hint(), width) {
		lines = append(lines, th.MutedStyle().Render(l))
	}
	return lines
}

// chipTitle is the text beside the "decision" chip: the question's own title on
// a single-question request (the shipped header), and a count on a
// multi-question one, where the per-question titles are the tab strip's job and
// repeating one of them in the header would name only the focused question
// while the agent is blocked on all of them.
func (p pendingDecision) chipTitle() string {
	if p.multi() {
		return fmt.Sprintf("%d questions", len(p.questions))
	}
	return p.question().Title
}

// tabStrip renders the multi-question tab strip: end affordance arrows around
// one cell per question plus the final Submit tab, each cell carrying the
// caret when focused and a ✔/□ checkbox reporting whether that question has been
// answered yet.
//
// State rides TWO channels on purpose. The caret and the checkbox glyph are
// visible in a plain Ascii render (this TUI's "selection reads without color"
// rule, the same reason the roster marks its selection with ▸); the accent on
// an answered ✔ and on the focused label is the color-only layer the styled
// golden exists to pin.
//
// When the full strip does not fit — a narrow terminal, or simply a lot of
// questions — it degrades to the focused tab plus an "(i/n)" position counter
// rather than being truncated, because a truncated strip can hide the very tab
// you are on, which is the one piece of state the strip exists to report.
func (p pendingDecision) tabStrip(th theme.Theme, width int) string {
	tabs := make([]string, 0, p.tabCount())
	for i := range p.tabCount() {
		tabs = append(tabs, p.tabCell(th, i))
	}
	prev := th.MutedStyle().Render(decisionPrevGlyph)
	next := th.MutedStyle().Render(decisionNextGlyph)

	full := prev + decisionTabGap + strings.Join(tabs, decisionTabGap) + decisionTabGap + next
	if ansi.StringWidth(full) <= width {
		return full
	}
	counter := th.MutedStyle().Render(fmt.Sprintf("(%d/%d)", p.tab+1, p.tabCount()))
	compact := prev + " " + tabs[p.tab] + "  " + counter + " " + next
	return truncate(compact, width)
}

// tabCell renders one tab: focus caret, checkbox, label. The label is the
// question's own Title, falling back to "Q1"/"Q2"/… when the agent gave it none
// — a tab has to be nameable even when the model was terse. The Submit tab's
// checkbox reports whether EVERY question has been answered, so the strip
// answers "am I done?" without a trip to the Submit tab.
func (p pendingDecision) tabCell(th theme.Theme, i int) string {
	label := decisionSubmitLabel
	answered := p.answeredCount() == len(p.questions)
	if i < len(p.questions) {
		label = p.questions[i].Title
		if label == "" {
			label = fmt.Sprintf("Q%d", i+1)
		}
		answered = i < len(p.drafts) && p.drafts[i].outcome != nil
	}

	box := th.MutedStyle().Render(decisionUnansweredGlyph)
	if answered {
		box = th.AccentStyle().Render(decisionAnsweredGlyph)
	}
	caret := " "
	if i == p.tab {
		caret = th.AccentStyle().Render(choiceCaret)
	} else {
		label = th.MutedStyle().Render(label)
	}
	return caret + " " + box + " " + label
}

// questionBody renders the focused question and its rows: the question text
// full width, then the option column beside the side panel.
//
// The panel collapses in two stages as the frame narrows. With nothing to show
// (no context, no reference, no note) there is no panel at all and the options
// take the full width — the common case, and byte-identical to the shipped
// single-question layout. With something to show but no room to put it beside
// the options, the panel stacks beneath them instead of squeezing the labels
// into a two-cell column: a narrow terminal loses the geometry, never the text.
func (p pendingDecision) questionBody(th theme.Theme, width int) []string {
	body := wrap(p.question().Question, width)
	body = append(body, "")

	if !p.multi() {
		return append(body, p.rowLines(th, width)...)
	}
	if !p.hasPanel() {
		return append(body, p.rowLines(th, width)...)
	}

	left, panel := decisionSplit(width)
	if panel == 0 {
		body = append(body, p.rowLines(th, width)...)
		body = append(body, "")
		return append(body, p.panelLines(th, width)...)
	}
	return append(body, joinDecisionColumns(th, p.rowLines(th, left), p.panelLines(th, panel), left)...)
}

// decisionSplit apportions width between the option column and the side panel.
// panel == 0 reports that the two-column layout does not fit and the caller
// must stack instead (see questionBody).
func decisionSplit(width int) (left, panel int) {
	panel = min(max(width/3, decisionPanelMinWidth), decisionPanelMaxWidth)
	left = width - panel - decisionPanelGap
	if left < decisionOptionMinWidth {
		return width, 0
	}
	return left, panel
}

// joinDecisionColumns composes the option column and the side panel into one
// block, padding each left cell to exactly leftWidth DISPLAY cells (padTo, not
// len — the cells carry ANSI) so the divider forms a straight vertical rule
// whatever styling the rows picked up. The divider is drawn on every row of the
// block, including rows where one column has run out, so the panel reads as a
// column rather than as floating text.
func joinDecisionColumns(th theme.Theme, left, right []string, leftWidth int) []string {
	divider := " " + th.MutedStyle().Render(decisionPanelDivider) + " "
	out := make([]string, 0, max(len(left), len(right)))
	for i := range max(len(left), len(right)) {
		l, r := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		// TrimRight only ever removes the divider's own trailing space (the
		// left cell's padding sits before the divider), so it can never eat a
		// style's reset sequence.
		out = append(out, strings.TrimRight(padTo(l, leftWidth)+divider+r, " "))
	}
	return out
}

// hasPanel reports whether the side panel has anything to show for the focused
// question: its supporting context, the focused option's reference, or a note.
func (p pendingDecision) hasPanel() bool {
	if p.question().Context != "" || p.draft().notes != "" || p.noting {
		return true
	}
	row, ok := p.focused()
	return ok && row.kind == decisionRowOption && p.question().Options[row.opt].Reference != ""
}

// panelLines renders the side panel's sections at the given width: the
// question's supporting Context, the focused option's Reference, and the note
// attached to this question's answer (live, with its cursor, while the notes
// editor is open). Each section is a muted heading over plain wrapped body
// text — headings are chrome, the text is content, which is the same
// marker-only styling split the rest of the prompt uses.
//
// The panel carries the question's Context and the FOCUSED option's Reference
// because that is the split docs/TUI.md's schema defines: context belongs to
// the decision, a reference belongs to one choice within it.
func (p pendingDecision) panelLines(th theme.Theme, width int) []string {
	section := func(out []string, heading, body string) []string {
		if body == "" {
			return out
		}
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, th.MutedStyle().Render(heading))
		return append(out, wrap(body, width)...)
	}

	var out []string
	out = section(out, decisionPanelContext, p.question().Context)
	if row, ok := p.focused(); ok && row.kind == decisionRowOption {
		out = section(out, decisionPanelReference, p.question().Options[row.opt].Reference)
	}
	if p.noting {
		// The live editor renders even when empty — an open editor with nothing
		// typed still has to show its cursor somewhere.
		if len(out) > 0 {
			out = append(out, "")
		}
		out = append(out, th.MutedStyle().Render(decisionPanelNotes))
		out = append(out, wrap(p.notes.Render(decisionCursorGlyph), width)...)
		return out
	}
	return section(out, decisionPanelNotes, p.draft().notes)
}

// rowLines renders the prompt's selectable rows at the given width through the
// shared [choiceListLines] — the same vertical caret list the approval prompt
// answers through — after composing each row's leader, label, and sub-lines.
// Each row wraps onto continuation lines indented under its own label rather
// than being truncated: an option whose label or rationale runs past the column
// is the normal case once the side panel takes a third of the frame, and a
// choice you can't read in full is a choice you can't make.
func (p pendingDecision) rowLines(th theme.Theme, width int) []string {
	q := p.question()
	rows := p.rows()
	crows := make([]choiceRow, 0, len(rows))
	for i, row := range rows {
		cr := choiceRow{
			// One blank row separates the numbered options from the escape
			// hatches beneath them, per the mockup. Emitted at the boundary
			// rather than unconditionally, so a question with no options at all
			// (free text only) doesn't open with two blank rows.
			sepBefore: row.kind != decisionRowOption && i > 0 && rows[i-1].kind == decisionRowOption,
		}

		switch row.kind {
		case decisionRowOption:
			opt := q.Options[row.opt]
			label := opt.Label
			if opt.Recommended {
				label += "  " + th.AccentStyle().Render(decisionRecommendedLabel)
			}
			if p.chosenOption(row.opt) {
				label += "  " + th.AccentStyle().Render(decisionAnsweredGlyph)
			}
			// The digit is the option's own 1-based position, which is also
			// what the 1-9 keys select — see App.selectDecisionOption.
			cr.leader = fmt.Sprintf("%d  ", row.opt+1)
			cr.label = label
			if opt.Rationale != "" {
				for _, l := range wrap(opt.Rationale, width-len(decisionRationaleIndent)) {
					cr.sublines = append(cr.sublines, th.MutedStyle().Render(decisionRationaleIndent+l))
				}
			}

		case decisionRowFreeText:
			body := decisionFreeTextLabel
			switch text, ok := p.draftText(); {
			case p.typing:
				// The placeholder gives way to the live buffer with its cursor
				// spliced in at the real position, exactly as the attach input
				// renders it.
				body = p.input.Render(decisionCursorGlyph)
			case ok:
				// A drafted free-text answer replaces the placeholder, so a tab
				// you come back to shows what you typed rather than inviting you
				// to type it again.
				body = text + "  " + th.AccentStyle().Render(decisionAnsweredGlyph)
			}
			cr.leader = decisionFreeTextGlyph + " "
			cr.label = body

		case decisionRowChat:
			label := decisionChatLabel
			if _, chat := p.draft().outcome.(acp.DecisionOutcomeChat); chat {
				label += "  " + th.AccentStyle().Render(decisionAnsweredGlyph)
			}
			cr.leader = decisionChatGlyph + " "
			cr.label = label

		case decisionRowSubmit:
			cr.leader = th.AccentStyle().Render(decisionAnsweredGlyph) + " "
			cr.label = fmt.Sprintf("%s %d of %d", decisionSubmitLabel, p.answeredCount(), len(p.questions))
		}
		crows = append(crows, cr)
	}
	return choiceListLines(th, crows, p.cursor, width)
}

// draftText returns the focused question's drafted free-text answer, if it has
// one.
func (p pendingDecision) draftText() (string, bool) {
	text, ok := p.draft().outcome.(acp.DecisionOutcomeText)
	return text.Text, ok
}

// submitBody renders the Submit tab: a per-question review of what is about to
// be sent, then the Submit row itself. Showing the unanswered questions as
// "cancelled on submit" is the point — the gate turns an omitted answer into a
// cancelled one, and the user is entitled to see that before committing rather
// than discover it in the agent's next message.
func (p pendingDecision) submitBody(th theme.Theme, width int) []string {
	body := wrap(fmt.Sprintf("Review and submit %d answers.", len(p.questions)), width)
	body = append(body, "")

	labels := make([]string, len(p.questions))
	labelWidth := 0
	for i, q := range p.questions {
		labels[i] = q.Title
		if labels[i] == "" {
			labels[i] = fmt.Sprintf("Q%d", i+1)
		}
		labelWidth = max(labelWidth, ansi.StringWidth(labels[i]))
	}
	// Cap the label column so one verbose title can't squeeze the summaries it
	// sits beside off the frame.
	labelWidth = min(labelWidth, max(width/3, 1))

	for i, label := range labels {
		draft := decisionDraft{}
		if i < len(p.drafts) {
			draft = p.drafts[i]
		}
		box := th.MutedStyle().Render(decisionUnansweredGlyph)
		if draft.outcome != nil {
			box = th.AccentStyle().Render(decisionAnsweredGlyph)
		}
		head := choiceGutter + box + " " + padTo(label, labelWidth) + "  "
		body = append(body, hangingIndent(head, th.MutedStyle().Render(p.summarize(i, draft)), width)...)
	}

	body = append(body, "")
	return append(body, p.rowLines(th, width)...)
}

// summarize renders one question's drafted answer as the Submit tab's one-line
// summary.
func (p pendingDecision) summarize(i int, draft decisionDraft) string {
	summary := decisionUnansweredNote
	switch o := draft.outcome.(type) {
	case acp.DecisionOutcomeSelected:
		summary = o.OptionID
		if i < len(p.questions) {
			for _, opt := range p.questions[i].Options {
				if opt.OptionID == o.OptionID {
					summary = opt.Label
					break
				}
			}
		}
	case acp.DecisionOutcomeText:
		summary = o.Text
	case acp.DecisionOutcomeChat:
		summary = decisionChatLabel
	case acp.DecisionOutcomeCancelled:
		summary = decisionUnansweredNote
	}
	if draft.notes != "" {
		summary += " · " + decisionPanelNotes
	}
	return summary
}

// hangingIndent lays head + body out at width, wrapping body onto continuation
// lines indented to head's DISPLAY width — so a wrapped option label lines up
// under the label rather than under its number, and a head carrying ANSI (the
// accent caret) still indents by the cells it actually occupies rather than by
// the bytes it takes.
func hangingIndent(head, body string, width int) []string {
	headWidth := ansi.StringWidth(head)
	indent := strings.Repeat(" ", headWidth)
	lines := wrap(body, width-headWidth)
	out := make([]string, 0, len(lines))
	for i, l := range lines {
		if i == 0 {
			out = append(out, head+l)
			continue
		}
		out = append(out, indent+l)
	}
	return out
}
