package config

import "path/filepath"

const (
	// DefaultSkillDirName is the conventional skills subdirectory name
	// under both the store root and a session's `.gofer` directory; see
	// [Skills.Directories].
	DefaultSkillDirName = "skills"

	// DefaultSkillMaxFileBytes is [Skills.MaxFileBytes]'s default: 256 KiB,
	// matching [DefaultMaxCommandFileBytes] — a SKILL.md body is submitted
	// to the model verbatim on invocation, so it gets the same cap and
	// rationale.
	DefaultSkillMaxFileBytes = 256 << 10

	// DefaultSkillDescriptionBytes is [Skills.DescriptionBytes]'s default:
	// 320 bytes. A skill's description is the ONLY selection signal that
	// enters context before the skill is invoked, so it gets more room
	// than a tool's one-line summary ([DefaultToolSummaryBytes]) while
	// staying around 80 tokens.
	DefaultSkillDescriptionBytes = 320
)

// Skills configures where gofer discovers SKILL.md skill directories and the
// size caps that keep them cheap to load. The zero value is fully valid:
// [Skills.Directories] resolves to the two conventional locations.
type Skills struct {
	// Dirs is the ordered list of skill directories to scan. Empty
	// resolves to the two conventional locations via [Skills.Directories].
	// Each entry follows the same path-resolution rules as
	// [Prompt.Files]'s entries (builtin:/absolute/~/cwd-then-root).
	Dirs []string `json:"dirs,omitempty"`

	// Disabled lists skill names to exclude even if discovered under Dirs;
	// see [Skills.IsDisabled].
	Disabled []string `json:"disabled,omitempty"`

	// MaxFileBytes caps how large one SKILL.md may be before it is skipped
	// rather than loaded: nil (unset) is [DefaultSkillMaxFileBytes], an
	// explicit 0 is "no limit", any other value is a byte cap. A skill
	// over the cap is SKIPPED with a note, never truncated — half a skill
	// is not a skill, the same precedent [TUI.MaxCommandFileBytes] sets.
	// See [Skills.FileLimitBytes].
	MaxFileBytes *int `json:"max_file_bytes,omitempty"`

	// DescriptionBytes caps how many bytes of a skill's description enter
	// context in the skill index: nil (unset) or negative resolves to
	// [DefaultSkillDescriptionBytes]. See [Skills.DescriptionLimitBytes].
	DescriptionBytes *int `json:"description_bytes,omitempty"`
}

// Directories resolves [Skills.Dirs]'s effective value: the configured list
// when non-empty, else the two conventional locations — <cwd>/.gofer/skills
// (project) and <root>/skills (global) — the same two directories
// internal/usercmd splits shared vs. project-local discovery across.
//
// Order here IS precedence, and it is deliberately the opposite of
// internal/usercmd.Load's directory order. usercmd.Load loads user scope
// then project scope into a map keyed by name, so the LAST write wins —
// project overrides user. The SDK's skill.Load has no such scope concept: it
// is PATH-style, and the FIRST directory to define a name wins (see its
// package doc). Returning [root, cwd] here — mirroring usercmd's literal
// build order instead of its OUTCOME — would hand skill.Load's first-wins
// semantics to the GLOBAL directory, so a global skill would silently
// shadow a same-named project skill: the opposite of how project commands
// behave. Listing the project directory first makes skill.Load's precedence
// land on the same outcome usercmd's does — project beats global — even
// though the two loaders reach it by opposite mechanisms.
func (s Skills) Directories(root, cwd string) []string {
	if len(s.Dirs) > 0 {
		return s.Dirs
	}
	return []string{
		filepath.Join(cwd, ".gofer", DefaultSkillDirName),
		filepath.Join(root, DefaultSkillDirName),
	}
}

// FileLimitBytes resolves [Skills.MaxFileBytes]'s effective value:
// [DefaultSkillMaxFileBytes] when unset or negative, else the explicit
// stored value (0 = no limit).
func (s Skills) FileLimitBytes() int {
	if s.MaxFileBytes == nil || *s.MaxFileBytes < 0 {
		return DefaultSkillMaxFileBytes
	}
	return *s.MaxFileBytes
}

// DescriptionLimitBytes resolves [Skills.DescriptionBytes]'s effective
// value: [DefaultSkillDescriptionBytes] when unset or negative.
func (s Skills) DescriptionLimitBytes() int {
	if s.DescriptionBytes == nil || *s.DescriptionBytes < 0 {
		return DefaultSkillDescriptionBytes
	}
	return *s.DescriptionBytes
}

// IsDisabled reports whether name appears in [Skills.Disabled].
func (s Skills) IsDisabled(name string) bool {
	for _, d := range s.Disabled {
		if d == name {
			return true
		}
	}
	return false
}
