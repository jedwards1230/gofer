package tui

// dialog.go is gofer's first interactive TUI dialog: a modal overlay for
// answering a pending [event.PermissionRequested] (docs/TUI.md's full
// dialog/keymap system lands later; this is the minimal approvals-relay
// landing per docs/M3-PLAN.md's "approvals relay + phone approval UX" item).
// [App] owns at most one [approval] at a time — the request for whichever
// session is currently attached/peeked, if any (see app.go's sessEventMsg
// handling) — and this file renders it as a bordered box composited atop the
// current screen, plus the key handling that captures input while it's
// active.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/charmbracelet/x/ansi"

	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// approval is the state of one pending permission request rendered as an
// interactive modal. A zero value is never exposed to a caller — App holds
// it behind a nil-checked pointer (a.dialog) so "no pending approval" and
// "an approval with the zero id" can never be confused.
type approval struct {
	sessionID string
	id        string
	tool      string
	spec      map[string]any
	remember  bool
}

// dialogInnerWidth is the fixed interior width (in columns, borders and side
// padding excluded) of the approval modal box.
const dialogInnerWidth = 48

// renderApprovalDialog renders d as a bordered modal box: the tool name, an
// args summary from Spec, the session id, and the Allow/Deny/remember action
// row. The whole box is styled as one unit (WarnStyle — a pending approval is
// cautionary), matching the rest of this package's render-then-style
// convention (see e.g. Model.statusLine).
func renderApprovalDialog(th theme.Theme, d approval) string {
	rows := []string{
		"Permission requested",
		"",
		"tool     " + d.tool,
		"args     " + specSummary(d.spec),
		"session  " + d.sessionID,
		"",
		fmt.Sprintf("[a] allow   [d] deny   [r] remember: %s", rememberLabel(d.remember)),
	}

	top := "┌" + strings.Repeat("─", dialogInnerWidth+2) + "┐"
	bottom := "└" + strings.Repeat("─", dialogInnerWidth+2) + "┘"

	var b strings.Builder
	b.WriteString(th.WarnStyle().Render(top))
	for _, r := range rows {
		line := "│ " + padTo(truncate(r, dialogInnerWidth), dialogInnerWidth) + " │"
		b.WriteString("\n")
		b.WriteString(th.WarnStyle().Render(line))
	}
	b.WriteString("\n")
	b.WriteString(th.WarnStyle().Render(bottom))
	return b.String()
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

// overlayCenter composites box (a fixed-shape, newline-joined block) onto
// base, centered within a widthxheight frame: rows and columns box doesn't
// cover keep base's own content, so the result reads as the box sitting atop
// the screen rather than replacing it outright. It is an opaque compositor —
// no transparency within box's own rectangle, which is exactly what a
// bordered dialog wants.
//
// Placement and splicing are done in DISPLAY COLUMNS (terminal cells), never
// rune offsets. renderApprovalDialog styles the box (WarnStyle) and the
// underlying frame is styled too, so a box line holds ANSI escape bytes and
// possibly wide runes; indexing it by rune would treat each escape byte as a
// column and scatter escape fragments across the base frame (the torn-modal
// defect). Each covered base row is instead rebuilt as
// base[:left) + boxLine + base[left+boxWidth:) with ANSI-aware slicing that
// preserves the styling on every retained segment.
func overlayCenter(base, box string, width, height int) string {
	baseLines := fitLines(base, width, height)
	boxLines := strings.Split(box, "\n")
	bw := lineWidth(boxLines)

	top := (height - len(boxLines)) / 2
	if top < 0 {
		top = 0
	}
	left := (width - bw) / 2
	if left < 0 {
		left = 0
	}

	for i, boxLine := range boxLines {
		row := top + i
		if row < 0 || row >= len(baseLines) {
			continue
		}
		baseLines[row] = spliceLine(baseLines[row], boxLine, left, width)
	}
	return strings.Join(baseLines, "\n")
}

// spliceLine overwrites the display-column span starting at left of baseLine
// (which fitLines has sized to exactly frameWidth cells) with boxLine,
// clamping boxLine to the columns actually available so a box wider than the
// frame truncates rather than overflowing. The retained left and right base
// segments and boxLine keep their ANSI styling; the result is re-fit to
// exactly frameWidth cells so a wide rune straddling a cut boundary can never
// shift the frame width.
func spliceLine(baseLine, boxLine string, left, frameWidth int) string {
	avail := frameWidth - left
	if avail <= 0 {
		return baseLine
	}
	boxLine = ansi.Truncate(boxLine, avail, "")
	bw := ansi.StringWidth(boxLine)
	leftPart := padTo(ansi.Truncate(baseLine, left, ""), left)
	rightPart := ansi.TruncateLeft(baseLine, left+bw, "")
	return padTo(leftPart+boxLine+rightPart, frameWidth)
}

// fitLines splits base into exactly height lines, each padded/truncated to
// exactly width display cells (terminal columns, not runes — padTo/truncate
// measure ANSI-styled and wide-rune content correctly), so overlayCenter can
// address it by row and display column without bounds surprises.
func fitLines(base string, width, height int) []string {
	lines := strings.Split(base, "\n")
	out := make([]string, height)
	for i := range height {
		var l string
		if i < len(lines) {
			l = lines[i]
		}
		out[i] = padTo(truncate(l, width), width)
	}
	return out
}

// lineWidth returns the widest line, in terminal cells (display width, so the
// box's ANSI styling and any wide runes are measured correctly), across lines.
func lineWidth(lines []string) int {
	w := 0
	for _, l := range lines {
		if n := ansi.StringWidth(l); n > w {
			w = n
		}
	}
	return w
}

// handleDialogKey handles key presses while an approval dialog is active,
// capturing all input until it resolves or is dismissed — [App.Update]
// routes here instead of the per-screen handlers whenever a.dialog is
// non-nil. Keymap: a/y allow, d/n deny (both reply immediately and dismiss
// the dialog), r toggles remember, esc dismisses the dialog without
// replying — the underlying request stays pending (e.g. a matching
// [event.PermissionResolved] from another attached client, or a later
// re-attach to the same session, can still surface or settle it).
func (a App) handleDialogKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	key := msg.Key()
	switch {
	case key.Mod.Contains(tea.ModCtrl) && key.Code == 'c':
		return a, tea.Quit

	case key.Text == "a" || key.Text == "y":
		return a.resolveDialog(true)

	case key.Text == "d" || key.Text == "n":
		return a.resolveDialog(false)

	case key.Text == "r":
		d := *a.dialog
		d.remember = !d.remember
		a.dialog = &d
		return a, nil

	case key.Code == tea.KeyEscape:
		a.dialog = nil
		return a, nil
	}
	return a, nil
}

// resolveDialog sends the active dialog's verdict via [Supervisor.Reply] and
// dismisses it immediately — an optimistic local dismiss. The matching
// [event.PermissionResolved], when it later arrives over the session's
// event stream, is then a no-op in [App.Update]'s sessEventMsg case, since
// a.dialog is already nil (or, rarely, already reassigned to a newer
// request with a different id).
func (a App) resolveDialog(allow bool) (tea.Model, tea.Cmd) {
	d := *a.dialog
	a.dialog = nil
	return a, a.doReply(d.sessionID, d.id, allow, d.remember)
}

// doReply answers a pending permission request via the Supervisor.
func (a App) doReply(sessionID, id string, allow, remember bool) tea.Cmd {
	return func() tea.Msg {
		err := a.sup.Reply(context.Background(), sessionID, id, allow, remember)
		return opDoneMsg{err: err}
	}
}
