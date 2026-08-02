package tui

// homepath.go holds this package's two "~" conversions, which run in opposite
// directions and must not be confused for each other.
//
// [displayHome] CONTRACTS an absolute path back to "~" for RENDERING only —
// never anywhere the result is persisted, journaled, sent over the wire, or
// shown to the model. Its callers are [Overview.cwdLabel] (the roster's cwd
// group headers), [statusView.lines] (the /status panel's "Cwd: " row) and the
// cwd-missing prompt's directory list; the value each reads from
// ([SessionInfo.Cwd]/[OverviewMeta.Cwd] and [CommandEnv.Cwd]) stays untouched
// everywhere else it's used — including CommandEnv.Cwd itself, which
// filepath.Join builds real filesystem paths from elsewhere in this package
// (settingSourcesLine, filemention.go, shell.go, usercmds.go).
//
// [expandHome] runs the other way, matching internal/daemon/handlers.go's
// normalizeCwd and internal/prompt/resolve.go's expandHome: a leading "~"
// resolved against THIS process's home, for a path that is about to become a
// real filesystem path or go on the wire. It has exactly one caller
// ([resolveChosenDir]) and gaining a second should be argued for, not assumed.

import (
	"os"
	"path/filepath"
	"strings"
)

// displayHome renders path with a leading $HOME contracted to "~", for
// display only. If os.UserHomeDir errors — vanishingly rare, since it just
// reads $HOME/USERPROFILE — path renders unchanged rather than failing,
// matching normalizeCwd/expandHome's fallback in the input direction.
func displayHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return contractHome(path, home)
}

// expandHome resolves a leading "~"/"~/" against this client's own home — the
// input direction, for a path that is about to be used as a real directory
// rather than displayed. If os.UserHomeDir errors (or answers ""), path is
// returned unexpanded rather than failing, the same fallback normalizeCwd and
// internal/prompt's expandHome take; the caller's own absolute-path check then
// rejects it as the real problem it is.
func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~"+string(filepath.Separator)) && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == "~" {
		return home
	}
	return filepath.Join(home, path[2:])
}

// contractHome is displayHome's pure core, taking home explicitly so tests
// can inject it instead of depending on the real $HOME — deterministic on
// any machine and in CI.
//
// The match must land on a path boundary: path equal to home, or home
// followed by a separator — never a bare string prefix. A naive
// strings.HasPrefix(path, home) contracts a SIBLING directory that merely
// shares home as a text prefix ("/Users/justinother/x" under
// home "/Users/justin") into the nonsensical "~other/x". A trailing
// separator on home itself (e.g. "$HOME" set to "/Users/justin/") is
// trimmed before comparing, for the same boundary reason — but only down to
// a bare root ("/" on POSIX, "C:\" on Windows): trimming THAT away would
// turn a genuine "home is the filesystem root" into the empty string, which
// the empty-home guard below then treats as "nothing to contract" instead of
// the root it actually is.
//
// The separator is [filepath.Separator], not a hardcoded "/" — home/path
// here are native OS paths (os.UserHomeDir's return, and whatever produced
// SessionInfo.Cwd/CommandEnv.Cwd, both filepath.Clean'd — see normalizeCwd
// in internal/daemon/handlers.go), so a literal "/" would silently never
// match on Windows.
func contractHome(path, home string) string {
	sep := string(filepath.Separator)
	for len(home) > len(sep) && strings.HasSuffix(home, sep) {
		home = home[:len(home)-len(sep)]
	}
	if home == "" || path == "" {
		return path
	}
	if path == home {
		return "~"
	}
	// The boundary prefix is normally home+sep. When home IS the bare root
	// (already ends in sep — the loop above stops there rather than
	// emptying it), that would double the separator ("//") and never match
	// any real path, which starts with exactly one; the root's own leading
	// separator already IS the boundary, so nothing more is appended.
	prefix := home + sep
	if home == sep {
		prefix = home
	}
	if rest, ok := strings.CutPrefix(path, prefix); ok {
		return "~" + sep + rest
	}
	return path
}
