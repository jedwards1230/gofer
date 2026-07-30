package tui

// compact.go implements `/compact [instructions]`: the explicit, on-demand
// counterpart to the automatic compaction trigger (internal/supervisor's
// pump). Both paths land on the same SDK seam ([runner.Runner.Compact] via
// [supervisor.Supervisor.Compact]) and both produce the identical visible
// result — a session.compacted event the transcript renders as an
// [itemSessionCompacted] block (see model.go's Ingest and
// [Model.renderSessionCompactedLines]) — so this command carries no rendering
// logic of its own; it only dispatches the op and reports whether the call
// itself succeeded.

import (
	"context"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// runCompact is /compact's [Command.Run]. It requires an attached session —
// unlike /new or /resume, there is no "apply to the default" reading for a
// command that summarizes a specific session's own history — and forwards
// every argument, space-joined, as the summarizer's instructions (matching
// [runner.Runner.Compact]: "" lets the SDK fall back to its own default
// instructions, so a bare `/compact` is a legitimate, common invocation, not
// a missing-argument error).
//
// The status note is OPTIMISTIC, set before the async call resolves — mirroring
// [App.applyModelSelection]/[App.applyEffortSelection]'s live-swap notes —
// because the operation's real visible effect is the transcript item Ingest
// appends once session.compacted actually arrives, not this status line. A
// failure (the session is running — [supervisor.ErrRunning] — or has nothing
// to compact — runner.ErrNothingToCompact) still overrides it through the
// ordinary opDoneMsg error path, the same as any other dispatched op.
func runCompact(a App, args []string) (App, tea.Cmd) {
	sess := a.currentSessionInfo()
	if sess == nil {
		a.setStatus(sevDanger, "/compact needs an attached session")
		return a, nil
	}
	instructions := strings.Join(args, " ")
	a.setStatus(sevOK, "Compacting context…")

	sessionID, sup := sess.ID, a.sup
	return a, func() tea.Msg {
		return opDoneMsg{err: sup.Compact(context.Background(), sessionID, instructions)}
	}
}
