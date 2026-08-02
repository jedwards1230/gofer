package uicopy

// cwdprompt.go holds the copy for the cwd-missing prompt — what the TUI says
// when a session cannot be attached because the directory it was RECORDED in no
// longer exists (see cwdprompt.go in [gofer/internal/tui]).

import (
	"fmt"
	"strconv"
)

// CwdReinitWarning is the load-bearing copy of this whole prompt: the reason
// "just point it somewhere that exists" is not a safe default.
//
// A session's local context is resolved against its cwd in four separate places
// — project config, user slash commands, skills, and file resolution — none of
// which announce themselves when they silently resolve differently. Re-initing
// elsewhere therefore REBASES the session's environment; it does not merely
// relocate it. That has to be said in the UI, at the moment of the choice, not
// only in a doc a user reading this prompt is not holding.
//
// Kept as a single unwrapped sentence and wrapped at render width by
// [cwdMissingPrompt.sections], so the assertion that it is on screen
// (TestCwdMissingPromptWarnsAboutCwdScopedContext) can normalise whitespace and
// still match regardless of where the wrap lands.
const CwdReinitWarning = "Warning: the session will load DIFFERENT local context there. " +
	"Project config (.gofer/config.json), user commands (<cwd>/.gofer/commands), skills, " +
	"and file resolution are all cwd-scoped — you are rebasing this session's environment, " +
	"not just pointing it somewhere that exists."

// CwdSessionDirGone is the prompt's heading.
const CwdSessionDirGone = "Session directory is gone"

// CwdHeadline states WHICH session, and WHICH directory went missing. session
// is the session's prose label ([CwdSessionLabel] or a quoted roster title);
// dir is the recorded directory, quoted here rather than by the caller.
func CwdHeadline(session, dir string) string {
	return fmt.Sprintf("%s was recorded in %s, which no longer exists — so it cannot be reopened where it was.",
		session, strconv.Quote(dir))
}

// CwdSessionLabel names a session by id, the fallback when it has no title.
func CwdSessionLabel(id string) string { return "Session " + id }

// CwdChoiceReinit is the first choice: re-init in a directory the user picks.
const CwdChoiceReinit = "Re-init this session in a new directory…"

// CwdChoiceCancel is the second (default, harmless) choice.
const CwdChoiceCancel = "Cancel — leave this session untouched (it stays unattachable)."

// CwdChoiceArchive is the third choice: the roster's own lifecycle affordance.
const CwdChoiceArchive = "Archive / delete this session — its journal is kept."

// CwdChoiceHint is the key hint under the three-way choice list.
const CwdChoiceHint = "↑/↓ move · 1-3 pick · enter choose · esc cancel"

// CwdDirectory is the directory picker's free-text entry row.
func CwdDirectory(entry string) string { return "Directory: " + entry }

// CwdFindingDirs is the picker's row while the enumeration is still in flight.
const CwdFindingDirs = "  finding directories…"

// CwdNoDirsFound is the picker's row when the enumeration answered with nothing.
const CwdNoDirsFound = "  no directories found here — type a path instead"

// CwdDirHint is the key hint under the directory picker.
const CwdDirHint = "type a path · ↑/↓ browse · enter re-init here · esc back"

// CwdReopening is the status note naming where a session is being reopened,
// stated before the round trip resolves either way.
func CwdReopening(dir string) string { return fmt.Sprintf("Reopening session in %s.", dir) }
