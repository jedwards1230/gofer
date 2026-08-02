package uicopy

import "fmt"

// Copy for [gofer/internal/tui]'s top-level App (internal/tui/app.go) — the
// status-line notes it posts in response to a key or a landed message.
//
// The bare error passthroughs (setStatus(sevDanger, err.Error())) carry no
// TUI-authored words and so have nothing here; only the notes gofer writes
// itself live in this file.

// QuitArmedNote is the status-line text a first ctrl+c shows while armed for
// the double-tap quit confirm. The wording is load-bearing rather than
// decorative: the confirm changes TEXT, not just color, because the Ascii
// golden profile cannot see the warn styling it renders in.
const QuitArmedNote = "ctrl-c again to quit"

// Notes for the roster's in-place restart of a stale daemon ("R").
const (
	DaemonRestarting = "Restarting daemon…"
	DaemonRestarted  = "Daemon restarted; roster restored."
)

// DaemonRestartFailed reports a daemon restart that never came back up.
func DaemonRestartFailed(reason string) string {
	return fmt.Sprintf("Daemon restart failed: %s", reason)
}

// NoSubagentsToStop is ctrl+t's refusal on a row that fanned out to nothing.
const NoSubagentsToStop = "No subagents under this session."

// StoppingSubagents acknowledges a ctrl+t bulk stop at dispatch time, naming
// how many rows are being stopped.
func StoppingSubagents(n int) string {
	if n == 1 {
		return fmt.Sprintf("Stopping %d subagent.", n)
	}
	return fmt.Sprintf("Stopping %d subagents.", n)
}
