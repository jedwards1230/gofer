package tui

// skills_view.go implements the /skills command-panel tab (gofer#303): a
// read-only view over the BACKEND's [capability.Answer] answering "which
// SKILL.md skills would a session started here load, and what did the loader
// refuse".
//
// Like mcp_view.go it reads ONLY the fetched answer — never a local directory
// walk — so a daemon-attached panel cannot render this machine's skills as if
// they were the daemon's. It is also where [skillset.Summarize] finally
// reaches a human: that line has existed since M7 and, until this tab, every
// caller computed it and threw it away.
//
// What it deliberately does not show: a source path for a LOADED skill. The
// SDK's skill.Meta records none, and re-walking the discovery directories to
// guess the winner goes wrong precisely when a first-directory candidate
// failed to load for an unrelated reason. The losing side of a shadowing IS
// knowable — it arrives as a diagnostic — and is shown.

import (
	"strings"

	"github.com/jedwards1230/gofer/internal/capability"
	"github.com/jedwards1230/gofer/internal/tui/theme"
	"github.com/jedwards1230/gofer/internal/uicopy"
)

// skillsView renders the Skills tab: a pure value, constructed inline per
// render like every other read-only tab.
type skillsView struct {
	theme theme.Theme
	caps  capabilitiesState
}

// View renders the view's rows, width-truncated and capped to height.
func (v skillsView) View(width, height int) string {
	lines := v.lines(height)
	if height >= 0 && len(lines) > height {
		lines = lines[:height]
	}
	for i, l := range lines {
		lines[i] = truncate(l, width)
	}
	return strings.Join(lines, "\n")
}

// lines builds the body rows for whichever of the three capability states the
// panel is in (see [capabilitiesState]).
func (v skillsView) lines(height int) []string {
	switch {
	case v.caps.pending && !v.caps.loaded:
		return loadingCapabilityLines(uicopy.SkillsLoadingSubject)
	case !v.caps.loaded || !v.caps.answer.Known:
		return unknownCapabilityLines(uicopy.SkillsUnknownSubject)
	}

	skills := v.caps.answer.Snapshot.Skills
	if len(skills.Loaded) == 0 && len(skills.Diagnostics) == 0 {
		// Distinct wording from the UNKNOWN body above — "none found" is a
		// read that happened, and the directories it searched are the useful
		// half of the answer.
		return append([]string{uicopy.SkillsNoneFound}, v.directoryLines(skills)...)
	}

	head := []string{v.headerLine(skills)}
	rows := make([]string, 0, len(skills.Loaded)+len(skills.Diagnostics))
	for _, s := range skills.Loaded {
		rows = append(rows, v.skillLine(s))
	}
	for _, d := range skills.Diagnostics {
		rows = append(rows, v.diagnosticLine(d))
	}

	var tail []string
	if skills.Summary != "" {
		// skillset.Summarize's own line, verbatim — the same sentence the
		// session-create path computes, now actually shown to somebody.
		tail = append(tail, v.theme.WarnStyle().Render(skills.Summary))
	}
	tail = append(tail, v.omissionLine())
	return fitRows(head, rows, tail, height)
}

// headerLine counts what loaded, how many of those are excluded by
// skills.disabled, and how many candidates the loader refused.
func (v skillsView) headerLine(skills capability.Skills) string {
	disabled := 0
	for _, s := range skills.Loaded {
		if s.Disabled {
			disabled++
		}
	}
	line := uicopy.SkillsHeaderLoaded(len(skills.Loaded))
	if disabled > 0 {
		line += uicopy.SkillsHeaderDisabled(disabled)
	}
	if n := len(skills.Diagnostics); n > 0 {
		line += uicopy.SkillsHeaderSkipped(n)
	}
	return line
}

// skillLine renders one loaded skill. Both markers are WORDS rather than
// styling, because the goldens pinning this view cannot see colour:
//
//   - "(disabled)" — loaded but excluded from the model-facing projection by
//     skills.disabled, which is why it still appears here at all.
//   - "(truncated)" — the description was cut to skills.description_bytes, so
//     what the model sees is not the whole sentence on screen.
func (v skillsView) skillLine(s capability.Skill) string {
	line := "  " + padCell(s.Name, 18) + s.Description
	if s.Truncated {
		line += uicopy.SkillsTruncatedSuffix
	}
	if s.Disabled {
		return v.theme.MutedStyle().Render(line + uicopy.SkillsDisabledSuffix)
	}
	return line
}

// diagnosticLine renders one candidate the loader refused. The two branches
// deliberately show DIFFERENT amounts, so read them as two rows, not one:
//
//   - SHADOWED — the label plus the losing path, and NOT d.Detail. The label
//     already carries the whole meaning ("this file lost its name to an earlier
//     directory"), and the SDK's message for it only restates the skill name —
//     which is on screen in the loaded list directly above — and the
//     precedence rule, which is documented. At the panel's width the detail
//     would push the path itself toward truncation, costing the one piece of
//     information only this row has.
//   - Everything else — the path plus d.Detail verbatim, because the reason is
//     the entire content of the row. Nothing else on screen says why an
//     oversized or malformed candidate was dropped.
//
// The shadowed classification is a substring test against an SDK message that
// is not contractual (see [skillset.IsShadowed]), so a reworded message makes
// an entry fall into the second branch — where it renders with its full reason.
// A miss therefore costs a label and no information, which is why the short row
// is safe to keep short.
func (v skillsView) diagnosticLine(d capability.Diagnostic) string {
	if d.Shadowed {
		return v.theme.WarnStyle().Render(uicopy.SkillsShadowedRow(d.Path))
	}
	return v.theme.DangerStyle().Render(uicopy.SkillsSkippedRow(d.Path, d.Detail))
}

// directoryLines renders the resolved discovery order, first-wins — the useful
// answer to "why did it find nothing". Omitted entirely when the backend
// reported no directories rather than rendered as a blank row (status.go's
// contingent-row idiom).
func (v skillsView) directoryLines(skills capability.Skills) []string {
	if len(skills.Directories) == 0 {
		return nil
	}
	out := make([]string, 0, len(skills.Directories)+1)
	out = append(out, uicopy.SkillsSearchedInOrder)
	for _, dir := range skills.Directories {
		out = append(out, "  "+displayHome(dir))
	}
	return out
}

// omissionLine names what this panel deliberately does NOT show. Unlike
// mcp_view.go's, this omission is not waiting on an issue: the SDK's skill
// index carries no path, so the winning file is knowable only by re-deriving
// it, and a wrong path here would be worse than none.
func (v skillsView) omissionLine() string {
	return v.theme.MutedStyle().Render(uicopy.SkillsSourcePathsOmitted)
}
