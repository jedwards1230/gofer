package tui

// approval.go holds the pending-approval state Model carries (see
// Model.pending) and its inline render: a WarnStyle-highlighted prompt that
// commandeers the bottom input line (see Model.View) while an
// event.PermissionRequested is awaiting a decision — the input is suppressed
// until the request is answered, then returns. It replaces the M3
// centered-overlay modal (docs/M3-PLAN.md's "approvals relay + phone
// approval UX" item) — the transcript's own ✋ badge (itemApproval) and
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
}

// renderApprovalPrompt renders p as the inline approval prompt's lines,
// structured like a confirm prompt that has taken over the input line: the
// tool + args summary, the question, the allow/deny/remember action row (the
// primary choices, keyed a/d/r), and a dim footer hint carrying the cancel key
// and the session id. The first three lines are WarnStyle'd (a pending approval
// is cautionary); the footer is MutedStyle'd like the status line and peek hint.
// Unlike the old modal, this is plain multi-line text composed top to bottom —
// never composited by absolute display-column splicing, which was the overlay's
// defect class.
func renderApprovalPrompt(th theme.Theme, p pendingApproval) []string {
	return []string{
		th.WarnStyle().Render(th.GlyphApproval + " " + p.tool + " · " + specSummary(p.spec)),
		th.WarnStyle().Render("Allow this tool call?"),
		th.WarnStyle().Render(fmt.Sprintf("  [a] allow   [d] deny   [r] remember: %s", rememberLabel(p.remember))),
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
