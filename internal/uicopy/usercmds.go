package uicopy

import "strconv"

// Copy for user-authored markdown slash commands (internal/tui/usercmds.go).

// UserCommandsSkipped reports files the loader rejected outright. A command
// that silently never appears is the most confusing failure this feature can
// have, so the count is surfaced rather than swallowed; the caller appends the
// first warning's own text after it.
// The plural rule is "> 1", not "!= 1": the only caller is guarded by n > 0,
// so n == 0 is unreachable and the singular it would produce is preserved as
// found rather than quietly corrected (gofer#290 is a pure move).
func UserCommandsSkipped(n int) string {
	if n > 1 {
		return "skipped " + strconv.Itoa(n) + " command files"
	}
	return "skipped " + strconv.Itoa(n) + " command file"
}

// UserCommandNeedsSession is the refusal for a markdown command run with
// nothing attached: the overview has nothing to send to, so the note says what
// to do instead rather than quietly picking a session.
func UserCommandNeedsSession(name string) string {
	return "/" + name + " sends a prompt — attach a session first (→ on the roster)"
}

// UserCommandEmptyExpansion is the refusal for a command whose body expanded
// to nothing — an empty file, or a body that was only `$1` with no argument.
func UserCommandEmptyExpansion(name string) string {
	return "/" + name + " expanded to an empty prompt — nothing sent"
}
