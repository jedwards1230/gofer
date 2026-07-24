// Package permrationale derives the plain-English answer to "why is this tool
// call being gated?" from what the guard reported about it.
//
// It exists because that answer has TWO producers that must agree: the TUI
// renders it locally from the [event.PermissionRequested] it already holds
// (see internal/tui's approval prompt), and the daemon returns it as the
// authoritative [acp.PermissionRationale] of an ACP session/explain_permission
// request (see internal/daemon's handleExplainPermission). Two copies of the
// grammar would drift, and the drift would be invisible: both renders look
// plausible, and only a user comparing the local approximation against the
// agent's own answer would ever notice they disagree.
//
// It is a leaf — the SDK's acp + event packages and nothing of gofer's — so
// both the TUI and the daemon can depend on it without either depending on the
// other (the same shape internal/render and internal/modelmeta have).
//
// It deliberately does NOT own the prompt's escape hatch ("press `r` …, or add
// a rule to config.json"). That is advice about the CLIENT a human is sitting
// in front of, not a fact about the gating decision: a phone answering the same
// request over ACP has no `r` key and no config.json, so putting it in a wire
// [acp.PermissionRationale] the daemon hands to any client would be a lie. It
// stays in internal/tui, which knows its own affordances.
package permrationale

import (
	"strings"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"
)

// Derive builds the rationale for a gated call from what it is known by: the
// tool it invoked and the guard's own decision trace (the SDK's
// [loop.RuleGuard] emits "rule: <label>" plus, on an allow-matched call it had
// to escalate, a "containable: …" entry — see loop/guard.go's
// Evaluate/containOrAsk).
//
// The four fields carry deliberately different confidence levels:
//
//   - Reason is the prose summary, and says only what the trace supports — an
//     unreadable or absent trace yields an explicit "could not determine why"
//     rather than a plausible guess.
//   - Policy is the matched rule label verbatim.
//   - Source is the label's provenance, and is set ONLY when the label is one
//     gofer itself stamps (see [source]); an agent-specific label is left
//     unattributed rather than mislabeled.
//   - Trace is the raw entries, verbatim and complete — nothing the guard
//     reported is dropped by the prose above it, so a reader can always check
//     the summary against the source.
//
// tool names the call in the prose; an empty tool degrades to "this call"
// rather than rendering empty backticks.
func Derive(tool string, trace []string) acp.PermissionRationale {
	rule, rest := SplitTrace(trace)
	return acp.PermissionRationale{
		Reason: reason(tool, rule, rest),
		Policy: rule,
		Source: source(rule),
		Trace:  trace,
	}
}

// reason turns the trace's rule label (and the containability entry riding
// with it) into the "what happened" summary. The two labels the SDK actually
// produces for a gated call are "unmatched" and a matched rule's own label
// (its permission.Rule.Source when it has one — gofer's config sets
// "config"/"default", a session grant sets "session" — else
// "<verdict> <tool>(<specifier>)"), so only the latter shape can reveal the
// verdict; anything else stays deliberately generic rather than asserting a
// verdict the label never carried.
func reason(tool, rule string, rest []string) string {
	call := "this call"
	if tool != "" {
		call = "this `" + tool + "` call"
	}

	var reason string
	switch {
	case rule == "":
		return "gofer could not determine why " + call + " was gated."
	case rule == "unmatched":
		reason = "No permission rule matched " + call + ", so gofer is asking before it runs."
	case strings.HasPrefix(rule, string(event.VerdictAsk)+" "):
		reason = "A permission rule matched " + call + " with the `ask` verdict."
	default:
		reason = "The `" + rule + "` permission rule matched " + call + ", and it was still gated for a decision."
	}
	// A containable:false entry is the other half of the story on an
	// allow-matched call: the guard's contain-or-ask policy escalates to a
	// human precisely because the sandbox can't hold it (see the SDK's
	// loop.RuleGuard). Saying so is what makes "just add an allow rule"
	// visibly not the answer here. A "containable: error:" entry is left to
	// the raw trace verbatim — an uncertain containment check is not the same
	// claim as a negative one.
	for _, entry := range rest {
		if strings.HasPrefix(entry, "containable: false") {
			return reason + " It also cannot be sandboxed on this host, so an allow rule alone will not let it run unattended."
		}
	}
	return reason
}

// source reports the provenance of a rule label, or "" when it cannot be
// identified honestly. The SDK's ruleLabel returns the matched rule's Source
// VERBATIM when it has one, so a label that IS one of the three sources gofer
// stamps — "session" (the guard's own remember-this-call grant), "config" (a
// rule in config.json's permissions array), "default" (gofer's built-in
// contain-or-ask catch-all) — is that source. Every other label ("unmatched",
// a "<verdict> <tool>(<specifier>)" summary, an agent's own label) carries no
// provenance this package can vouch for, and [acp.PermissionRationale.Source]
// omits an empty value, so it says nothing rather than guessing.
func source(rule string) string {
	switch rule {
	case "session", "config", "default":
		return rule
	default:
		return ""
	}
}

// SplitTrace separates the guard's trace into the rule label (the "rule: "
// entry's value, "" when the trace carries none) and every other entry in
// order. Parsing by prefix rather than position: the SDK appends the
// containability entries after the rule entry today, but a trace is a
// human-readable diagnostic list, not a positional tuple.
//
// It is exported because a renderer of an already-derived rationale needs the
// same split to print the label once and the remaining entries beside it (see
// internal/tui's policy line) — re-deriving it there would be the exact
// duplication this package exists to prevent.
func SplitTrace(trace []string) (rule string, rest []string) {
	const rulePrefix = "rule: "
	for _, entry := range trace {
		if rule == "" && strings.HasPrefix(entry, rulePrefix) {
			rule = strings.TrimPrefix(entry, rulePrefix)
			continue
		}
		rest = append(rest, entry)
	}
	return rule, rest
}
