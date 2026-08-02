package uicopy

// skills_view.go holds the /skills tab's copy (internal/tui/skills_view.go).
//
// The loader's own summary line ([gofer/internal/skillset.Summarize]) and a
// diagnostic's detail are rendered verbatim from the backend's answer, so they
// are not copy this package can own.

import "strconv"

// Subjects the /skills tab hands the shared loading/unknown capability bodies.
// They differ in case because the loading body reads "Loading skills…" while
// the unknown one opens a sentence.
const (
	SkillsLoadingSubject = "skills"
	SkillsUnknownSubject = "Skills"
)

// SkillsNoneFound is the empty state — worded distinctly from the UNKNOWN body,
// since "none found" is a read that happened.
const SkillsNoneFound = "Skills: none found."

// SkillsHeaderLoaded opens the header with how many skills loaded.
func SkillsHeaderLoaded(n int) string {
	return "Skills: " + strconv.Itoa(n) + " loaded"
}

// SkillsHeaderDisabled continues the header with how many of the loaded skills
// the skills.disabled config excludes. Appended to [SkillsHeaderLoaded] only
// when nonzero.
func SkillsHeaderDisabled(n int) string {
	return ", " + strconv.Itoa(n) + " disabled"
}

// SkillsHeaderSkipped continues the header with how many candidates the loader
// refused. Appended to [SkillsHeaderLoaded] only when nonzero.
func SkillsHeaderSkipped(n int) string {
	return ", " + strconv.Itoa(n) + " skipped"
}

// Per-skill markers, both WORDS rather than styling because the goldens
// pinning this view cannot see colour: a truncated description is not the whole
// sentence the model sees, and a disabled skill is loaded but excluded from the
// model-facing projection.
const (
	SkillsTruncatedSuffix = " (truncated)"
	SkillsDisabledSuffix  = " (disabled)"
)

// SkillsShadowedRow renders a candidate that lost its name to an earlier
// directory: the label plus the losing path, and deliberately no detail. The
// leading and trailing spaces are the row's column padding — the label column
// is twelve cells wide, shared with [SkillsSkippedRow].
func SkillsShadowedRow(path string) string {
	return "  shadowed  " + path
}

// SkillsSkippedRow renders a candidate the loader refused, with its reason
// verbatim — nothing else on screen says why it was dropped. Padded to the same
// twelve-cell label column as [SkillsShadowedRow].
func SkillsSkippedRow(path, detail string) string {
	return "  skipped   " + path + ": " + detail
}

// SkillsSearchedInOrder heads the resolved discovery order — the useful answer
// to "why did it find nothing".
const SkillsSearchedInOrder = "Searched, in precedence order (first match wins):"

// SkillsSourcePathsOmitted names what the tab does not show. Unlike /mcp's
// omission this one waits on no issue: the SDK's skill index carries no path,
// and a wrong path here would be worse than none.
const SkillsSourcePathsOmitted = "Source paths shown for shadowed/skipped files only — the index carries none"
