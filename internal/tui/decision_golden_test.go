package tui_test

// decision_golden_test.go covers the inline structured-decision prompt
// (internal/tui/decision.go) at three of docs/TUI.md's four test layers: the
// Ascii golden that locks its LAYOUT against the mockup, the styled golden
// that locks the color state an Ascii render is blind to (the accent
// "(Recommended)" suffix and focused caret, the warn "decision" chip, the
// muted rationale/hint), and the narrow-width colored layout test that catches
// the #61 ANSI-width class neither golden asserts. The fourth layer — the
// Update-level key behavior — is decision_test.go, next door in package tui.
//
// The prompt is driven through the exported [tui.Model.IngestDecision] with a
// hand-built [decision.Update], the same shape [App]'s decision pump feeds it
// from a real gate: a golden fixture should exercise the production entry
// point, not a test-only setter.

import (
	"testing"

	"github.com/jedwards1230/agent-sdk-go/acp"

	"github.com/jedwards1230/gofer/internal/decision"
	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// decisionFixture is docs/TUI.md's own single-question mockup, verbatim: the
// migration-strategy decision with two options, the second recommended, both
// carrying a rationale sub-line, and both escape hatches on. Its ids come from
// [decision.AssignIDs], exactly as a live [decision.Gate.Request] stamps them,
// so the fixture can never drift from the real id scheme.
func decisionFixture() decision.Update {
	return decision.Update{
		Kind: decision.UpdateRequested,
		Request: decision.Request{
			ID:        "dec-1",
			SessionID: sid,
			Questions: decision.AssignIDs([]acp.DecisionQuestion{{
				Title:    "Pick a migration strategy",
				Question: "Which approach should I take?",
				Options: []acp.DecisionOption{
					{
						Label:     "In-place ALTER",
						Rationale: "fastest, but locks the table for the duration",
					},
					{
						Label:       "Shadow table + backfill",
						Rationale:   "online, but doubles disk until cutover",
						Recommended: true,
					},
				},
				AllowFreeText: decision.DefaultAllowFreeText,
				AllowChat:     decision.DefaultAllowChat,
			}}),
		},
	}
}

// TestGoldenDecisionPromptInline covers the pending structured decision: the
// footer-commandeering block (rule, "decision <title>" chip, the question, the
// numbered options with their dim rationale sub-lines, the free-text and chat
// escape-hatch rows, and the dim key hint) that docs/TUI.md's single-question
// mockup specifies.
func TestGoldenDecisionPromptInline(t *testing.T) {
	m := tui.New(theme.Test()).IngestDecision(decisionFixture())
	testkit.AssertGolden(t, "decision_prompt_inline", testkit.Render(m, testkit.Width, testkit.Height))
}

// TestGoldenStyledDecisionPrompt is the inline prompt's styled-golden
// counterpart: it locks the color state the Ascii golden above cannot see —
// the warn "decision" chip, the accent caret on the focused row, the accent
// "(Recommended)" suffix marking the agent's preference, and the muted
// rationale/hint lines. A regression that dropped the recommendation's
// emphasis, or colored it as an error, passes every Ascii golden.
func TestGoldenStyledDecisionPrompt(t *testing.T) {
	m := tui.New(testkit.ColorTheme()).IngestDecision(decisionFixture())
	testkit.AssertGoldenStyled(t, "decision_prompt_inline", testkit.Render(m, testkit.Width, testkit.Height))
}

// TestColorDecisionPromptNarrow proves the decision prompt's lines clamp to a
// narrow width (24) instead of overrunning it, and that its styling changes no
// geometry — the #61 display-width lesson, which an Ascii golden alone cannot
// catch because it renders no escapes at all. Every render change in this
// package ships with both a golden and one of these (docs/TUI.md, "How the TUI
// is tested", layer 3).
func TestColorDecisionPromptNarrow(t *testing.T) {
	build := func(th theme.Theme) tui.Model {
		return tui.New(th).IngestDecision(decisionFixture())
	}

	const width = 24
	plain := testkit.Render(build(theme.Test()), width, testkit.Height)
	colored := testkit.Render(build(testkit.ColorTheme()), width, testkit.Height)
	assertColorLayout(t, plain, colored, width)
}
