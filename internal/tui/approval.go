package tui

// approval.go holds the pending-approval state Model carries (see
// Model.pending) and its inline render: a blank-padded prompt block that
// commandeers the whole footer (see Model.View) — status line, rules, and
// input box included — while an event.PermissionRequested is awaiting a
// decision, returning the footer once the request is answered. It replaces
// the M3 centered-overlay modal (docs/M3-PLAN.md's "approvals relay + phone
// approval UX" item) — the transcript's own ● badge (itemApproval) and
// resolution line (itemApprovalResolved) are unrelated permanent records
// that render elsewhere in model.go and are unaffected by this file.

import (
	"fmt"
	"sort"
	"strings"

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

	// badgeIdx is the transcript index of the itemApproval badge this request
	// added, so transcriptLines can suppress it while the prompt is showing
	// (the prompt already repeats the tool + args line).
	badgeIdx int
}

// renderApprovalPrompt renders p as the inline approval prompt's blank-padded
// block at the given width, structured like a confirm prompt that has taken
// over the whole footer: a full-width rule, a titled "<tool> command"
// header, the indented args summary, a blank separator, the question and the
// allow/deny/remember action row (the primary choices, keyed a/d/r) in the
// default style, another blank separator, and a dim footer hint carrying the
// cancel key and the session id. This replaces the earlier single
// marker-line "● bash · cmd=…" summary with the rule/title/args block the
// redesign's goldens specify — args now read as an indented, labeled line
// rather than riding the same line as the tool name. No leading blank line —
// [App.render]'s [layout.TopPadding] already supplies the frame's top
// margin, and Model.View stacks this block directly onto whatever transcript
// is above it. Unlike the old modal, this is plain multi-line text composed
// top to bottom — never composited by absolute display-column splicing,
// which was the overlay's defect class. width < 1 floors to 1 (matching
// every other component's width guard), so the rule can never
// strings.Repeat a negative count.
func renderApprovalPrompt(th theme.Theme, p pendingApproval, width int) []string {
	if width < 1 {
		width = 1
	}
	return []string{
		strings.Repeat("─", width),
		th.WarnStyle().Render(p.tool + " command"),
		"",
		"  " + specSummary(p.spec),
		"",
		"Allow this tool call?",
		fmt.Sprintf("  [a] allow   [d] deny   [r] remember: %s", rememberLabel(p.remember)),
		"",
		th.MutedStyle().Render("esc cancel · session " + p.session),
	}
}

// rememberLabel renders the remember toggle's current state for the action
// row.
func rememberLabel(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// specSummary renders a compact, deterministic one-line summary of a
// permission request's Spec: sorted keys, since map iteration order is not
// stable and this feeds a golden-tested render.
func specSummary(spec map[string]any) string {
	if len(spec) == 0 {
		return "(no args)"
	}
	keys := make([]string, 0, len(spec))
	for k := range spec {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, spec[k]))
	}
	return strings.Join(parts, " ")
}
