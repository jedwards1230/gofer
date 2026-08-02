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
	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/uicopy"
)

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

// applyCompactionEvent drives the in-flight indicator from the session
// compaction event contract, and reports whether the caller must arm a tick.
//
// This is what makes AUTOMATIC compaction visible (gofer#300). The explicit
// `/compact` path can latch on its own RPC's lifetime, but an automatic
// compaction is triggered supervisor-side with no client call in flight, so
// before session.compaction_started existed there was nothing to hang an
// indicator on and the transcript simply froze for a minute. The rule this
// package holds to is that the state is READ FROM THE CONTRACT, never inferred:
// no watching for a stall, no diffing token counts, no timer that guesses. If
// the event does not say a compaction is running, nothing is drawn.
//
// The three cases are the SDK's total start/terminal pair. A compaction that
// publishes a start goes on to publish exactly one of session.compacted or
// session.compaction_failed, so a latch taken here is always released — the
// early exits (cancelled context, runner.ErrNothingToCompact) happen before the
// start is published and so latch nothing at all.
//
// Totality holds over what the SDK PUBLISHES, though, not over what this client
// RECEIVES, and the difference is why sessClosedMsg clears the latch too. A
// subscription can be severed between the start and its terminal — the broker
// force-unsubscribes a subscriber it had to block on, or the broker is closed
// out from under the compaction (ordinary ctrl+c) — and in both the only signal
// is the channel closing. Without that clear, one severed subscription leaves
// "compacting context…" on screen forever, counting up, describing nothing.
func (a *App) applyCompactionEvent(ev event.Event) (armTick bool) {
	switch e := ev.(type) {
	case event.SessionCompactionStarted:
		// An explicit /compact already latched at dispatch — an EARLIER and more
		// truthful instant for the user who asked, since it covers the round trip
		// too. Keep it, and do not arm a second tick on top of the one it
		// started; two ticks would just redraw identical frames twice a second.
		if !a.compactingSince.IsZero() {
			return false
		}
		// Prefer the event's own publish time so a client attaching MID
		// compaction — the case this event exists for — counts from when the
		// work actually started rather than from when it happened to connect.
		// On the daemon path that time is the reconstructor's republish, not the
		// origin's — see internal/wirestream/reconstruct.go's note on seq/time —
		// so it understates elapsed by the transport hop and nothing more. The
		// IsZero fallback below stays as defence. Same guard as
		// internal/router/rostercache.go.
		since := e.Time()
		if since.IsZero() {
			since = time.Now()
		}
		a.compactingSince = since
		return true

	case event.SessionCompacted:
		// Terminal (success). The durable record is the itemSessionCompacted
		// block Model.Ingest appends from this same event — this only retires
		// the transient indicator that was standing in for it.
		a.compactingSince = time.Time{}

	case event.SessionCompactionFailed:
		// Terminal (failure), and the only place the TUI can learn a compaction
		// did not land. Say so rather than letting the indicator blink out as if
		// it had succeeded: a silently vanishing indicator is indistinguishable
		// from a completed compaction, and the user would be left believing their
		// context was summarized when it was not. Nothing the runner acknowledges
		// as journaled happened, so the session's context is exactly what it was.
		a.compactingSince = time.Time{}
		a.setStatus(sevDanger, uicopy.CompactionFailed(e.Err))
	}
	return false
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
		a.setStatus(sevDanger, uicopy.CompactNeedsSession)
		return a, nil
	}
	instructions := strings.Join(args, " ")
	a.compactingSince = time.Now()
	if a.scr != screenAttach {
		a.setStatus(sevOK, uicopy.CompactingNote)
	}

	sessionID, sup := sess.ID, a.sup
	return a, tea.Batch(
		func() tea.Msg {
			return compactDoneMsg{err: sup.Compact(context.Background(), sessionID, instructions)}
		},
		compactTick(),
	)
}
