package tui

// approval.go holds the pending-approval state Model carries (see
// Model.pending) and its inline render: a blank-padded prompt block that
// commandeers the whole footer (see Model.View) — status line, rules, and
// input box included — while an event.PermissionRequested is awaiting a
// decision, returning the footer once the request is answered. It replaces
// the M3 centered-overlay modal for the "approvals relay + phone approval
// UX" item — the transcript's own ● badge (itemApproval) and resolution line
// (itemApprovalResolved) are unrelated permanent records that render
// elsewhere in model.go and are unaffected by this file.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// pendingApproval is the state of one unresolved permission request. A zero
// value is never exposed to a caller — Model holds it behind a nil-checked
// pointer (Model.pending) so "no pending approval" and "an approval with the
// zero id" can never be confused.
type pendingApproval struct {
	id       string
	tool     string
	spec     map[string]any
	session  string
	remember bool

	// agent is the originating agent id from the tool call's event.Agent, or
	// "" when the call is un-attributed (no subagent, or a stream that never
	// carried one).
	agent string
	// trace is the guard's decision trace from event.PermissionRequested.Trace
	// — the local source for the rationale body until session/explain_permission
	// lands.
	trace []string

	// badgeIdx is the transcript index of the itemApproval badge this request
	// added, so transcriptLines can suppress it while the prompt is showing
	// (the prompt already repeats the tool + args line).
	badgeIdx int
}

// commandKeys are the spec keys renderApprovalPrompt treats as "the body of
// the call", in preference order: the first present non-empty string value
// wins and is rendered verbatim (multi-line, wrapped), with every other key
// demoted to the residual k=v list. Ordered most-specific-first — a call
// carrying both a command and a path is a shell call operating on that path,
// so the command is what an operator is actually being asked to approve.
var commandKeys = []string{"command", "cmd", "script", "file_path", "path"}

// renderApprovalPrompt renders p as the inline approval prompt's blank-padded
// block at the given width: a full-width rule, an attributed "<tool> command"
// title, the call's own description and body, a plain-English rationale
// derived from the guard's decision trace, the question with its numbered
// action row, and a dim footer hint carrying the cancel key and the session
// id. bodyLimit caps the body's rows (config.TUI.ApprovalBodyLineLimit — the
// gated call's text must never push the question off the frame).
//
// No leading blank line — [App.render]'s [layout.TopPadding] already supplies
// the frame's top margin, and Model.View stacks this block directly onto
// whatever transcript is above it. Every element is its own slice entry,
// composed top to bottom and pre-wrapped here: this is never composited by
// absolute display-column splicing (the overlay's defect class), and it never
// relies on a caller reflowing it either — [Model.promptLines] hard-truncates
// each line to width, so a line this function leaves over-width would be
// clipped, not wrapped. width < 1 floors to 1 (matching every other
// component's width guard), so the rule can never strings.Repeat a negative
// count and the wrap budget below can never go non-positive.
func renderApprovalPrompt(th theme.Theme, p pendingApproval, width, bodyLimit int) []string {
	if width < 1 {
		width = 1
	}

	lines := []string{
		strings.Repeat("─", width),
		th.WarnStyle().Render(approvalTitle(p)),
	}
	// The description (a tool-call convention: the agent's own one-line
	// summary of what it is about to do) reads as a subtitle under the title,
	// with no blank between them; it is omitted entirely when absent rather
	// than leaving an empty row.
	if desc, ok := p.spec["description"].(string); ok && desc != "" {
		for _, l := range indentWrap(desc, width) {
			lines = append(lines, th.MutedStyle().Render(l))
		}
	}

	lines = append(lines, "")
	lines = append(lines, approvalBodyLines(th, p, width, bodyLimit)...)
	lines = append(lines, "", "Why you're being asked")
	lines = append(lines, rationaleLines(p, width)...)
	lines = append(lines,
		"",
		"Do you want to proceed?",
		fmt.Sprintf("  1. [a] Yes   2. [d] No   ·   [r] remember: %s", rememberLabel(p.remember)),
		"",
		th.MutedStyle().Render("esc cancel · session "+p.session),
	)
	return lines
}

// approvalTitle renders the prompt's header text: the raw tool name (never
// title-cased — the tool id is what appears in a permission rule, and matching
// it exactly is what makes the "add a rule" hint below copy-pasteable) plus,
// when the call is attributed, which agent issued it. An un-attributed call
// (p.agent == "") gets NO suffix and no placeholder: the honest rendering of
// "we don't know" is to say nothing, not to invent a name.
func approvalTitle(p pendingApproval) string {
	title := p.tool + " command"
	if p.agent != "" {
		title += " · from the `" + p.agent + "` agent"
	}
	return title
}

// approvalBodyLines renders the gated call itself: the command body (the
// first commandKeys hit, split on its own newlines and wrapped) followed by
// every residual spec key as a sorted `k=v` row, all indented 2. With no
// command key present this degrades to the pre-redesign sorted summary, just
// one key per row instead of one long line; an empty spec keeps "(no args)".
//
// The whole block is capped at limit rows: over it, the first limit-1 rows
// render followed by a muted "… +N more lines", so a pasted 200-line heredoc
// can never push the question and action row off the top of the frame. A
// non-positive limit is treated as uncapped — callers resolve the configured
// default before calling (see [Model.approvalBodyLimit]), so this only
// triggers for a caller that deliberately wants everything.
func approvalBodyLines(th theme.Theme, p pendingApproval, width, limit int) []string {
	body, key := commandBody(p.spec)

	var rows []string
	if key != "" {
		for _, physical := range strings.Split(body, "\n") {
			rows = append(rows, indentWrap(physical, width)...)
		}
	}
	for _, k := range residualKeys(p.spec, key) {
		rows = append(rows, indentWrap(fmt.Sprintf("%s=%v", k, p.spec[k]), width)...)
	}
	if len(rows) == 0 {
		return []string{"  (no args)"}
	}

	if limit > 0 && len(rows) > limit {
		keep := limit - 1
		rows = append(rows[:keep:keep], th.MutedStyle().Render(fmt.Sprintf("  … +%d more lines", len(rows)-keep)))
	}
	return rows
}

// commandBody picks the spec value that IS the call — the first non-empty
// string among [commandKeys] — returning it and the key it came from. Both
// are "" when no key matches (or the matching key holds a non-string, e.g. a
// structured edit payload), which callers read as "render the whole spec as
// k=v rows instead".
func commandBody(spec map[string]any) (body string, key string) {
	for _, k := range commandKeys {
		if v, ok := spec[k].(string); ok && v != "" {
			return v, k
		}
	}
	return "", ""
}

// residualKeys returns spec's keys except the one already rendered as the
// command body and "description" (rendered as the title's subtitle), sorted —
// map iteration order is not stable and this feeds a golden-tested render.
func residualKeys(spec map[string]any, bodyKey string) []string {
	keys := make([]string, 0, len(spec))
	for k := range spec {
		if k == bodyKey || k == "description" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// rationaleLines renders the "Why you're being asked" body: up to three
// paragraphs derived from the guard's decision trace, each wrapped and
// indented 2, separated by a blank row and preceded by one (the section
// header sits directly above).
//
// The trace is the only local source for this until the SDK grows an explain
// request (docs/TUI.md's backlog), so the derivation is deliberately literal
// about what it does and does not know — see [approvalRationale].
func rationaleLines(p pendingApproval, width int) []string {
	var lines []string
	for _, para := range approvalRationale(p) {
		lines = append(lines, "")
		lines = append(lines, indentWrap(para, width)...)
	}
	return lines
}

// approvalRationale derives the rationale paragraphs from p's trace:
//
//  1. Reason — what the trace actually says happened, in plain English.
//  2. Policy — the matched rule label verbatim plus every remaining raw trace
//     entry, so nothing the guard reported is silently dropped by the
//     prose above.
//  3. Escape hatch — the two ways, both real in this codebase, to stop being
//     asked: the session-scoped remember toggle and a persisted config rule.
//
// An empty trace yields the reason paragraph's honest fallback plus the
// escape hatch, and no Policy paragraph — there is no policy detail to print,
// and "Policy:" with nothing after it would be worse than its absence.
func approvalRationale(p pendingApproval) []string {
	rule, rest := splitTrace(p.trace)

	paras := []string{approvalReason(rule, rest)}
	if policy := approvalPolicy(rule, rest); policy != "" {
		paras = append(paras, policy)
	}
	return append(paras, approvalEscapeHatch(p))
}

// approvalReason turns the trace's rule label (and the containability entry
// riding with it) into the "what happened" paragraph. The two labels the SDK
// actually produces for a gated call are "unmatched" and a matched rule's own
// label (its permission.Rule.Source when it has one — gofer's config sets
// "config"/"default" — else "<verdict> <tool>(<specifier>)"), so only the
// latter shape can reveal the verdict; anything else stays deliberately
// generic rather than asserting a verdict the label never carried.
func approvalReason(rule string, rest []string) string {
	var reason string
	switch {
	case rule == "":
		return "gofer could not determine why this call was gated."
	case rule == "unmatched":
		reason = "No permission rule matched this call, so gofer is asking before it runs."
	case strings.HasPrefix(rule, string(event.VerdictAsk)+" "):
		reason = "A permission rule matched this call with the `ask` verdict."
	default:
		reason = "The `" + rule + "` permission rule matched this call, and it was still gated for a decision."
	}
	// A containable:false entry is the other half of the story on an
	// allow-matched call: the guard's contain-or-ask policy escalates to a
	// human precisely because the sandbox can't hold it (see the SDK's
	// loop.RuleGuard). Saying so is what makes "just add an allow rule"
	// visibly not the answer here. A "containable: error:" entry is left to
	// the Policy paragraph verbatim — an uncertain containment check is not
	// the same claim as a negative one.
	for _, entry := range rest {
		if strings.HasPrefix(entry, "containable: false") {
			return reason + " It also cannot be sandboxed on this host, so an allow rule alone will not let it run unattended."
		}
	}
	return reason
}

// approvalPolicy renders the Policy paragraph: the rule label verbatim plus
// every other trace entry, " · "-joined. Empty when the trace carried
// nothing at all.
func approvalPolicy(rule string, rest []string) string {
	parts := make([]string, 0, len(rest)+1)
	if rule != "" {
		parts = append(parts, rule)
	}
	parts = append(parts, rest...)
	if len(parts) == 0 {
		return ""
	}
	return "Policy: " + strings.Join(parts, " · ")
}

// approvalEscapeHatch renders the two concrete ways out, both of which exist
// today: the prompt's own session-scoped remember toggle, and a rule in the
// config file's permissions array. The rule example is built from the call's
// real tool and the first token of its real command so it can be pasted as
// written; with no command body there is no honest example to give, so the
// "e.g." is omitted rather than guessed at.
func approvalEscapeHatch(p pendingApproval) string {
	hatch := "Press `r` before allowing to remember this exact call for the rest of the session. " +
		"Add a rule to the `permissions` array in `" + config.ConfigFileName + "`"

	body, _ := commandBody(p.spec)
	if fields := strings.Fields(body); len(fields) > 0 {
		hatch += fmt.Sprintf(" — e.g. `{\"verdict\": %q, \"tool\": %q, \"specifier\": %q}` —",
			string(event.VerdictAllow), p.tool, fields[0]+" *")
	}
	return hatch + " to stop being asked."
}

// splitTrace separates the guard's trace into the rule label (the "rule: "
// entry's value, "" when the trace carries none) and every other entry in
// order. Parsing by prefix rather than position: the SDK appends the
// containability entries after the rule entry today, but a trace is a
// human-readable diagnostic list, not a positional tuple.
func splitTrace(trace []string) (rule string, rest []string) {
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

// indentWrap word-wraps s to the prompt's two-space-indented body column and
// returns one slice entry per terminal row (see [wrap], which hard-breaks a
// token longer than the budget). The budget is floored at 1 explicitly —
// [wrap] floors it too, but a negative budget here would mean this function
// silently disagreed with the column it claims to wrap to. Below width 3 the
// floored budget makes rows wider than width; [Model.promptLines] truncates
// them, exactly as it does the rule and the title.
func indentWrap(s string, width int) []string {
	budget := width - 2
	if budget < 1 {
		budget = 1
	}
	rows := wrap(s, budget)
	for i, r := range rows {
		rows[i] = "  " + r
	}
	return rows
}

// rememberLabel renders the remember toggle's current state for the action
// row.
func rememberLabel(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
