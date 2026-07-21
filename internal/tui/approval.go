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

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/permrationale"
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
	// — the LOCAL source for the rationale body, derived through
	// [permrationale.Derive], used until (and if) an explain answers with the
	// agent's own.
	trace []string

	// explaining reports that a session/explain_permission call is in flight
	// for this request (ctrl+e — see [App.explainApproval]). It only marks the
	// rationale header as in-flight and makes a second ctrl+e a no-op; the
	// request itself stays open and answerable throughout, because an explain
	// never resolves it.
	explaining bool
	// rationale is the AUTHORITATIVE rationale an explain returned, or nil
	// when none has (yet) arrived — in which case the render derives one
	// locally from trace. Distinguishing them is the point: a user must be
	// able to tell the agent's own answer from this client's approximation of
	// it, so the header labels which one is on screen (see
	// [rationaleHeaderLine]).
	rationale *acp.PermissionRationale

	// badgeIdx is the transcript index of the itemApproval badge this request
	// added, so transcriptLines can suppress it while the prompt is showing
	// (the prompt already repeats the tool + args line).
	badgeIdx int

	// amend is the open inline amend editor (amend.go), or nil when the
	// prompt is showing its ordinary decision row. While it is non-nil the
	// prompt renders the editor in place of that row and EVERY key press
	// goes to the editor (see App.handleApprovalKey), so `a`/`d`/`r` type
	// characters rather than resolving the request.
	amend *amendEditor
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
// (the agent's own once an explain has answered, else derived locally from the
// guard's decision trace), the question with its numbered action row, and a
// dim footer hint carrying the cancel key, the explain key, and the session
// id. bodyLimit caps the body's rows (config.TUI.ApprovalBodyLineLimit — the
// gated call's text must never push the question off the frame), and caps the
// amend editor's visible rows too when one is open.
//
// `[tab] amend` rides the ACTION row rather than the hint line beneath it, for
// two reasons: amending IS a decision affordance — the middle option between
// allowing something slightly wrong and denying it — and the hint line has no
// room left. At width 80 "esc cancel · ctrl+e explain · session <uuid>" is 75
// cells; a fourth key would push the session id past the edge and
// [Model.promptLines] would clip it mid-uuid. The action row has ~30 cells
// spare, so both keys stay fully visible at the default terminal size.
//
// With p.amend set (Tab — see amend.go) the numbered action row and the hint
// line are replaced by the inline editor, its cursor line, and its warning;
// everything above them — header, attribution, body, and whichever rationale
// applies — renders identically either way.
//
// collapsed drops the rationale to its opening paragraph plus a pointer at
// ctrl+e, for a frame too short to show the whole block without squeezing the
// transcript out of existence (see [Model.promptLines], which decides). What
// it never collapses is the header, the command body, the question, the action
// row, or the hint: a user must always be able to see what they are allowing
// and how to answer it. The amend editor is likewise never collapsible — the
// collapse only ever shortens the RATIONALE, so an open editor keeps every
// line of its text, its cursor, and its warning in every frame size (pinned by
// TestAmendEditorSurvivesTheShortFrameCollapse). Collapsing the one warning
// that says an amended call is not re-run through the rules, or the line the
// user is typing on, would be the two worst rows in this block to lose.
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
func renderApprovalPrompt(th theme.Theme, p pendingApproval, width, bodyLimit int, collapsed bool) []string {
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
	lines = append(lines, "", rationaleHeaderLine(th, p))
	lines = append(lines, rationaleLines(th, p, width, collapsed)...)
	// While the amend editor is open it REPLACES the decision row and the
	// hint line — the header, attribution, body, and rationale above stay,
	// so the user can still see what they are amending and why it was gated.
	// The rationale above is whichever form applies (local, collapsed, or the
	// agent's own answer): amending changes what is being decided, not why it
	// was gated.
	if p.amend != nil {
		return append(lines, amendLines(th, p, width, bodyLimit)...)
	}
	lines = append(lines,
		"",
		"Do you want to proceed?",
		fmt.Sprintf("  1. [a] Yes   2. [d] No   ·   [r] remember: %s   ·   [tab] amend", rememberLabel(p.remember)),
		"",
		th.MutedStyle().Render("esc cancel · ctrl+e explain · session "+p.session),
	)
	return lines
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
	warnAmendOverride = "Warning: an amended command does not go back through the permission rules. " +
		"Approving it is a manual override — the call runs exactly what you leave here."
	warnAmendRemember = "Remember is on, so the standing grant will pin the edited command, not the model's original."
)

// amendCursorGlyph is the block the editor draws at the cursor — the same
// marker the attach input uses (see [Model.inputLine]), so a cursor reads
// identically on both text surfaces.
const amendCursorGlyph = "▏"

// amendLines renders the open amend editor in place of the prompt's decision
// row: a label naming the spec key being edited, the edited text with a
// visible cursor on the focused line, the persistent warning above, and the
// editor's own key hints.
//
// limit caps the VISIBLE editor rows with the same config-derived budget the
// body uses (config.TUI.ApprovalBodyLineLimit). Unlike the body, an over-cap
// editor scrolls rather than collapsing its tail into "… +N more lines":
// truncating could hide the very line being typed on, and a cursor you
// cannot see is unusable. The hidden lines are still announced, above and
// below, so the visible window never reads as the whole command.
func amendLines(th theme.Theme, p pendingApproval, width, limit int) []string {
	ed := *p.amend

	lines := []string{"", "Amending `" + ed.key + "`:"}
	start, end := ed.visibleLines(limit)
	if start > 0 {
		lines = append(lines, th.MutedStyle().Render(fmt.Sprintf("  … +%d lines above", start)))
	}
	for i := start; i < end; i++ {
		glyph := ""
		if i == ed.cursorLine {
			glyph = amendCursorGlyph
		}
		lines = append(lines, "  "+ed.lines[i].Render(glyph))
	}
	if hidden := len(ed.lines) - end; hidden > 0 {
		lines = append(lines, th.MutedStyle().Render(fmt.Sprintf("  … +%d lines below", hidden)))
	}

	warns := []string{warnAmendOverride}
	if p.remember {
		warns = append(warns, warnAmendRemember)
	}
	lines = append(lines, "")
	for _, w := range warns {
		for _, l := range wrap(w, width) {
			lines = append(lines, th.WarnStyle().Render(l))
		}
	}
	return append(lines,
		"",
		th.MutedStyle().Render("ctrl+s approve edited · esc cancel · enter newline · remember: "+rememberLabel(p.remember)),
	)
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

// rationaleHeaderLine renders the "Why you're being asked" section header,
// with a muted suffix naming WHICH rationale sits below it — the whole point
// of ctrl+e being that a user can tell the agent's authoritative answer from
// this client's local approximation of it, and can see when one is on its way:
//
//   - an explain in flight  -> "· explaining…"
//   - an explain answered   -> "· the agent's answer"
//   - neither (the default) -> no suffix; the body is the local derivation
//
// The plain header is the un-suffixed case on purpose: labeling the local
// derivation ("· derived locally", say) would put a caveat on every approval
// prompt ever shown, which is noise until a user asks for the other one.
func rationaleHeaderLine(th theme.Theme, p pendingApproval) string {
	const header = "Why you're being asked"
	switch {
	case p.explaining:
		return header + th.MutedStyle().Render(" · explaining…")
	case p.rationale != nil:
		return header + th.MutedStyle().Render(" · the agent's answer")
	default:
		return header
	}
}

// rationaleLines renders the "Why you're being asked" body: the rationale's
// paragraphs, each wrapped and indented 2, separated by a blank row and
// preceded by one (the section header sits directly above).
//
// The rationale is the AUTHORITATIVE one an explain returned when p carries
// it, else one derived locally from the guard's decision trace — the same
// grammar either way (see [permrationale.Derive]), so the two are comparable
// rather than differently worded restatements of the same decision.
//
// collapsed keeps only the opening paragraph and points at ctrl+e for the
// rest — the short-frame budget [Model.promptLines] applies. It is
// deliberately the FIRST paragraph that survives: the reason is what a
// decision actually turns on, while the policy detail and the escape hatch are
// reference material a user can pull up when they want it.
//
// With the amend editor open that pointer names the key sequence that
// actually works from there: ctrl+e inside the editor is jump-to-end-of-line,
// not explain (see [App.handleAmendKey]), so the collapsed rationale says to
// leave the editor first rather than advertising a key that would silently do
// something else.
func rationaleLines(th theme.Theme, p pendingApproval, width int, collapsed bool) []string {
	paras := rationaleParagraphs(p)
	if collapsed && len(paras) > 1 {
		paras = paras[:1]
	}

	var lines []string
	for _, para := range paras {
		lines = append(lines, "")
		lines = append(lines, indentWrap(para, width)...)
	}
	if collapsed {
		hint := "  … ctrl+e to explain"
		if p.amend != nil {
			hint = "  … esc, then ctrl+e to explain"
		}
		lines = append(lines, th.MutedStyle().Render(hint))
	}
	return lines
}

// rationaleParagraphs builds the rationale's paragraphs:
//
//  1. Reason — what the gating decision was, in plain English.
//  2. Policy — the matched rule label plus every remaining raw trace entry, so
//     nothing the guard reported is silently dropped by the prose above it.
//  3. Escape hatch — the two ways, both real in this client, to stop being
//     asked: the session-scoped remember toggle and a persisted config rule.
//
// A rationale carrying neither a policy label nor a trace yields the reason
// plus the escape hatch and no Policy paragraph — "Policy:" with nothing after
// it would be worse than its absence.
func rationaleParagraphs(p pendingApproval) []string {
	rationale := p.rationale
	if rationale == nil {
		derived := permrationale.Derive(p.tool, p.trace)
		rationale = &derived
	}

	paras := []string{rationale.Reason}
	if policy := policyParagraph(*rationale); policy != "" {
		paras = append(paras, policy)
	}
	return append(paras, approvalEscapeHatch(p))
}

// policyParagraph renders the Policy paragraph from a rationale's machine-
// readable provenance: the matched label, then every trace entry that is not
// the label's own "rule: " line (which would otherwise print twice), then the
// source when it says something the label does not — all " · "-joined. Empty
// when the rationale carries no provenance at all.
//
// The label falls back to the trace's own rule entry, so a rationale from an
// agent that fills Trace but not Policy still names what matched.
func policyParagraph(r acp.PermissionRationale) string {
	rule, rest := permrationale.SplitTrace(r.Trace)
	label := r.Policy
	if label == "" {
		label = rule
	}

	parts := make([]string, 0, len(rest)+2)
	if label != "" {
		parts = append(parts, label)
	}
	parts = append(parts, rest...)
	// gofer's own labels ARE their source (see permrationale.Derive), so this
	// only speaks up for a rationale whose source adds something the label
	// doesn't already say — an agent-side policy whose origin differs from it.
	if r.Source != "" && r.Source != label {
		parts = append(parts, "source: "+r.Source)
	}
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
