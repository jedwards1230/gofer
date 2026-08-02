package uicopy

// decision.go holds the structured-decision prompt's copy: the chip, the
// escape-hatch row labels, the side panel's section headings, the Submit tab's
// label and review wording, and the footer key hint in each of the shapes the
// prompt takes. The prompt's glyphs, gaps, and indents stay in
// [gofer/internal/tui]'s decision.go — they are chrome, not copy.

import "fmt"

// DecisionChip is the prompt's header chip, naming what is blocking the turn.
// A single-question header joins it to the question's own title with a "·".
const DecisionChip = "decision"

// DecisionFreeTextLabel is the free-text escape hatch's placeholder.
const DecisionFreeTextLabel = "Type something."

// DecisionChatLabel is the chat escape hatch's label, which hands the turn back
// to the conversation instead of answering.
const DecisionChatLabel = "Chat about this"

// DecisionRecommendedLabel marks the option the agent recommended, appended to
// that option's own label rather than replacing it.
const DecisionRecommendedLabel = "(Recommended)"

// The side panel's three section headings. Each names where the text under it
// came from — the question's own framing, the focused option's supporting
// material, and the operator's own notes — so the panel never presents one as
// another.
const (
	DecisionPanelContext   = "context"
	DecisionPanelReference = "reference"
	DecisionPanelNotes     = "notes"
)

// DecisionSubmitLabel is the Submit tab's label in the multi-question tab
// strip, which [DecisionSubmitProgress] extends with the answered count.
const DecisionSubmitLabel = "Submit"

// DecisionSubmitProgress is the Submit tab's label once the strip has room for
// its progress, so the strip answers "am I done?" without a trip to the tab.
func DecisionSubmitProgress(answered, total int) string {
	return fmt.Sprintf("%s %d of %d", DecisionSubmitLabel, answered, total)
}

// DecisionUnansweredNote is the Submit tab's summary for a question with no
// chosen outcome — the gate turns an omitted answer into a cancelled one, and
// the operator is entitled to see that before committing.
const DecisionUnansweredNote = "not answered — cancelled on submit"

// DecisionQuestionCount is the chip title on a multi-question request, where
// naming one question's title would name only the focused one while the agent
// is blocked on all of them.
func DecisionQuestionCount(n int) string {
	return fmt.Sprintf("%d questions", n)
}

// DecisionSubmitReview opens the Submit tab's per-question review.
func DecisionSubmitReview(n int) string {
	return fmt.Sprintf("Review and submit %d answers.", n)
}

// The prompt's dim footer key hint, one per shape it takes. Each describes the
// keys that are actually live right now: an editor advertises a different
// contract than the row list (Enter commits what was typed, Esc backs out of
// the editor rather than out of the prompt), and the multi-question shape binds
// two keys the single-question one does not — a key hint that lies about what
// Enter does is worse than none.
const (
	DecisionHintNoting      = "Enter to save the note · Esc to discard"
	DecisionHintTypingMulti = "Enter to save · Esc to discard"
	DecisionHintTyping      = "Enter to submit · Esc to cancel"
	DecisionHintSubmitTab   = "Enter to submit · Tab to switch questions · Esc to cancel"
	DecisionHintMulti       = "Enter to select · ↑/↓ to navigate · n to add notes · Tab to switch questions · Esc to cancel"
	DecisionHintSingle      = "Enter to select · ↑/↓ to navigate · Esc to cancel"
)
