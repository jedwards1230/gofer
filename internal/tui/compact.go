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
	"time"

	tea "charm.land/bubbletea/v2"
)

// compactingNote is the status line a `/compact` dispatched from OFF the attach
// screen leaves behind. It is a named constant because [App.Update]'s
// compactDoneMsg case clears the status only when it still holds exactly this
// text — a note some other event set in the meantime is newer and must win.
const compactingNote = "Compacting context…"

// compactTickInterval is how often the in-progress indicator redraws. One
// second: the indicator's only moving part is a whole-second elapsed counter,
// so a faster tick would redraw identical frames.
const compactTickInterval = time.Second

// compactTickMsg drives the in-progress indicator's elapsed counter. It
// reschedules itself for as long as [App.compactingSince] is set — see
// [App.Update] — so no tick outlives the compaction it counts for.
type compactTickMsg struct{}

// compactDoneMsg carries the result of the dispatched [supervisor.Supervisor.Compact]
// call. It is deliberately NOT [opDoneMsg]: a successful op has to do real work
// here (retire the indicator and the status note), and opDoneMsg's success path
// is a no-op on purpose — several ops leave a note behind that is MEANT to
// persist ("Model set to …"), so teaching the shared message to clear the status
// would erase those.
type compactDoneMsg struct{ err error }

// compactTick schedules the next indicator redraw.
func compactTick() tea.Cmd {
	return tea.Tick(compactTickInterval, func(time.Time) tea.Msg { return compactTickMsg{} })
}

// runCompact is /compact's [Command.Run]. It requires an attached session —
// unlike /new or /resume, there is no "apply to the default" reading for a
// command that summarizes a specific session's own history — and forwards
// every argument, space-joined, as the summarizer's instructions (matching
// [runner.Runner.Compact]: "" lets the SDK fall back to its own default
// instructions, so a bare `/compact` is a legitimate, common invocation, not
// a missing-argument error).
//
// Compaction is a SLOW, SILENT operation — a whole summarizer model call over
// the folded history, which can run for a minute or more with nothing streaming
// — so it needs a live progress affordance, not a fire-and-forget note. This
// command therefore marks [App.compactingSince] and starts a once-per-second
// tick, which together render `⋯ compacting context… (42s)` at the transcript
// tail through [Model.WithCompacting]: the same transient-indicator grammar as
// the turn-in-flight `⋯ working…`, in the transcript where the work is, rather
// than a static line under the input.
//
// The status note stays as the fallback for a `/compact` dispatched from OFF
// the attach screen (peek, the command panel), where that tail indicator is not
// on screen to be seen. Either way the operation's real visible RESULT is the
// itemSessionCompacted block Ingest appends when session.compacted arrives.
//
// Every ending routes through [compactDoneMsg], which is what retires the
// indicator and the note — including a failure (the session is running,
// [supervisor.ErrRunning], or has nothing to compact, runner.ErrNothingToCompact).
func runCompact(a App, args []string) (App, tea.Cmd) {
	sess := a.currentSessionInfo()
	if sess == nil {
		a.setStatus(sevDanger, "/compact needs an attached session")
		return a, nil
	}
	instructions := strings.Join(args, " ")
	a.compactingSince = time.Now()
	if a.scr != screenAttach {
		a.setStatus(sevOK, compactingNote)
	}

	sessionID, sup := sess.ID, a.sup
	return a, tea.Batch(
		func() tea.Msg {
			return compactDoneMsg{err: sup.Compact(context.Background(), sessionID, instructions)}
		},
		compactTick(),
	)
}
