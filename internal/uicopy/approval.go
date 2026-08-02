package uicopy

// approval.go holds the inline approval prompt's copy: the header, the gated
// call's body markers, the "Why you're being asked" rationale section, the
// question and its hint line, the amend editor's labels and warnings, and the
// escape-hatch paragraph that tells an operator how to stop being asked.

import "fmt"

// ApprovalTitleCommandSuffix follows the raw tool name in the prompt's header.
const ApprovalTitleCommandSuffix = " command"

// ApprovalTitleAgentPrefix opens the header's agent attribution, which is
// omitted entirely for an un-attributed call rather than given a placeholder.
const ApprovalTitleAgentPrefix = " · from the `"

// ApprovalTitleAgentSuffix closes the header's agent attribution.
const ApprovalTitleAgentSuffix = "` agent"

// ApprovalNoArgs stands in for the call body when the spec is empty.
const ApprovalNoArgs = "  (no args)"

// ApprovalMoreLines announces the call-body rows dropped by the line cap.
func ApprovalMoreLines(n int) string {
	return fmt.Sprintf("  … +%d more lines", n)
}

// ApprovalRationaleHeader labels the section explaining why the call was gated.
const ApprovalRationaleHeader = "Why you're being asked"

// ApprovalRationaleExplaining suffixes the rationale header while a
// session/explain_permission call is in flight.
const ApprovalRationaleExplaining = " · explaining…"

// ApprovalRationaleAgentAnswer suffixes the rationale header once an explain
// has answered, marking the body below as the agent's own answer rather than
// this client's local derivation of it.
const ApprovalRationaleAgentAnswer = " · the agent's answer"

// ApprovalRationaleExplainHint points at ctrl+e from a collapsed rationale.
const ApprovalRationaleExplainHint = "  … ctrl+e to explain"

// ApprovalRationaleExplainFromAmendHint is the collapsed rationale's pointer
// while the amend editor is open, where ctrl+e is jump-to-end-of-line rather
// than explain — so it names the key sequence that actually works from there.
const ApprovalRationaleExplainFromAmendHint = "  … esc, then ctrl+e to explain"

// ApprovalPolicyPrefix opens the rationale's Policy paragraph.
const ApprovalPolicyPrefix = "Policy: "

// ApprovalPolicySourcePrefix labels a rationale's source inside the Policy
// paragraph, for a source that says something its label does not.
const ApprovalPolicySourcePrefix = "source: "

// ApprovalRememberHint opens the escape-hatch paragraph with the prompt's own
// session-scoped remember toggle.
const ApprovalRememberHint = "Press `r` before allowing to remember this exact call for the rest of the session. "

// ApprovalAddRulePrefix opens the escape hatch's second way out — a rule in the
// config file, whose name the caller appends.
const ApprovalAddRulePrefix = "Add a rule to the `permissions` array in `"

// ApprovalRuleExample renders the pasteable permission-rule example inside the
// escape-hatch paragraph. The JSON keys are config syntax the operator pastes,
// not copy; only the surrounding "e.g." framing is.
func ApprovalRuleExample(verdict, tool, specifier string) string {
	return fmt.Sprintf(" — e.g. `{\"verdict\": %q, \"tool\": %q, \"specifier\": %q}` —", verdict, tool, specifier)
}

// ApprovalStopBeingAskedSuffix closes the escape-hatch paragraph.
const ApprovalStopBeingAskedSuffix = " to stop being asked."

// ApprovalQuestion is the prompt's question, which sits directly above its
// Yes/No choice list as that list's label.
const ApprovalQuestion = "Do you want to proceed?"

// The two answer rows, in the fixed order the cursor indexes them: the
// affirmative first, because that is the row Enter opens on.
const (
	ApprovalChoiceYes = "Yes"
	ApprovalChoiceNo  = "No"
)

// The remember toggle's two states, as the hint lines below render them. They
// describe the toggle only — the words a user types at `/yolo` are that
// command's own vocabulary and live with it.
const (
	ApprovalRememberOn  = "on"
	ApprovalRememberOff = "off"
)

// ApprovalHint is the muted footer beneath the choice list: how to navigate it,
// the remember toggle's live state, and the amend/explain/cancel keys. It fits
// one line at the standard 80-cell width so the whole prompt stays within a
// non-collapsing 80x24 frame.
func ApprovalHint(remember string) string {
	return fmt.Sprintf("enter/↑↓ select · [r] remember: %s · [tab] amend · ctrl+e explain · esc cancel", remember)
}

// The amend editor's two warning sentences. They are the UI half of a real
// SDK property and must never be softened into a claim of safety:
// loop.awaitApproval substitutes event.PermissionReply.Input into the call
// AFTER the guard already evaluated the model's original arguments, and it
// substitutes it BEFORE calling Grant — so an amended allow is a human
// override the rules never saw, and a REMEMBERED amended allow pins the
// edited call as the standing grant. Nothing here may suggest the edit is
// validated, re-run through the rules, or in any way vetted.
const (
	ApprovalAmendOverrideWarning = "Warning: an amended command does not go back through the permission rules. " +
		"Approving it is a manual override — the call runs exactly what you leave here."
	ApprovalAmendRememberWarning = "Remember is on, so the standing grant will pin the edited command, not the model's original."
)

// ApprovalAmendingPrefix opens the amend editor's label, which names the spec
// key being edited.
const ApprovalAmendingPrefix = "Amending `"

// ApprovalAmendLinesAbove announces the editor rows scrolled off the top.
func ApprovalAmendLinesAbove(n int) string {
	return fmt.Sprintf("  … +%d lines above", n)
}

// ApprovalAmendLinesBelow announces the editor rows scrolled off the bottom.
func ApprovalAmendLinesBelow(n int) string {
	return fmt.Sprintf("  … +%d lines below", n)
}

// ApprovalAmendHintPrefix is the amend editor's key hint, up to the remember
// toggle's state, which the caller appends.
const ApprovalAmendHintPrefix = "ctrl+s approve edited · esc cancel · enter newline · remember: "
