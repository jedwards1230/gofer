package config

const (
	// DefaultPromptAsset is [Prompt.Files]'s default when unset: the single
	// shipped baseline system prompt, embedded as a binary asset rather than
	// read off disk — a fresh install has a working default prompt with no
	// filesystem dependency. "builtin:<name>" is the only prompt source form
	// that is NOT a path; every other form documented on [Prompt] resolves
	// against the filesystem.
	DefaultPromptAsset = "builtin:system.md"

	// DefaultPromptMaxFileBytes is [Prompt.MaxFileBytes]'s default: 256 KiB,
	// matching [DefaultMaxCommandFileBytes] — a prompt file is read on EVERY
	// session create, and a 256 KiB prompt is already past most models'
	// context windows, so the cap exists to catch a mistake (a log
	// accidentally saved as a prompt file), not to constrain a real one.
	DefaultPromptMaxFileBytes = 256 << 10
)

// Prompt configures the markdown files composed into a new session's system
// prompt. The zero value is fully valid: [Prompt.Sources] resolves to
// [DefaultPromptAsset] alone, so an unconfigured gofer still starts with a
// working baseline prompt and no hardcoded Go string constant (see
// docs/PRD.md's "no hardcoded prompts" constraint — this section is what
// replaces cmd/gofer's defaultSystemPrompt).
//
// Resolution rules (implemented by a later PR; documented here because the
// schema is where the contract belongs — an implementer should not have to
// invent this): each entry in [Prompt.Files] resolves independently —
//
//   - "builtin:<name>" loads an embedded asset; the only non-path form.
//   - An absolute path is read verbatim.
//   - A "~/…" path is expanded against the user's home directory.
//   - Any other relative path is resolved against the session's CWD first,
//     then the store root; the first hit wins.
//
// The resolved files are composed in list order, joined by a blank line; an
// entry that resolves to a path identical to an earlier one in the list is
// skipped (duplicates are first-wins, not repeated). A file that cannot be
// found or read WARNS and is skipped rather than failing the session, unless
// [Prompt.MissingFileIsError] opts into the stricter behavior — a mistyped
// path should not silently blind a session with no visible signal.
//
// Config LAYERING (flags > env > project > home, as an earlier PRD draft
// described) is deliberately NOT part of this type: there is no deep-merge
// anywhere in gofer's config (see the package doc), and cwd-first path
// resolution above already delivers the useful part — a project's own
// AGENTS.md/CLAUDE.md wins simply by being resolved first in its own
// entry. Layering remains its own design and is out of scope here.
type Prompt struct {
	// Files is the ordered list of markdown sources composed into the
	// system prompt. Empty resolves to [DefaultPromptAsset] alone via
	// [Prompt.Sources] — never to "no prompt".
	Files []string `json:"files,omitempty"`

	// MissingFileIsError selects what an unresolvable entry in Files does:
	// false (the default — unset and false share the zero value, so there
	// is no separate "unset" state to track here) WARNS and continues
	// composing the remaining sources; true fails the session create
	// outright. A plain bool suffices — unlike e.g. [TUI.Autoscroll],
	// there is no third state "unset" needs to mean that differs from
	// false.
	MissingFileIsError bool `json:"missing_file_is_error,omitempty"`

	// MaxFileBytes caps how large ONE prompt file may be before it is
	// skipped rather than composed: nil (unset) is
	// [DefaultPromptMaxFileBytes], an explicit 0 is "no limit", and any
	// other value is a byte cap — the same *int contract as
	// [TUI.MaxCommandFileBytes], for the same reason: a plain int can't
	// distinguish "field absent" from an explicit 0. See
	// [Prompt.FileLimitBytes] for the resolved value every caller should
	// read.
	MaxFileBytes *int `json:"max_file_bytes,omitempty"`
}

// Sources resolves [Prompt.Files]'s effective source list: the configured
// list when non-empty, else a single-element list holding
// [DefaultPromptAsset] — the fail-safe "gofer always has SOME prompt" floor
// a fresh install relies on.
func (p Prompt) Sources() []string {
	if len(p.Files) > 0 {
		return p.Files
	}
	return []string{DefaultPromptAsset}
}

// FileLimitBytes resolves [Prompt.MaxFileBytes]'s effective value:
// [DefaultPromptMaxFileBytes] when unset or negative, else the explicit
// stored value (0 = no limit).
func (p Prompt) FileLimitBytes() int {
	if p.MaxFileBytes == nil || *p.MaxFileBytes < 0 {
		return DefaultPromptMaxFileBytes
	}
	return *p.MaxFileBytes
}
