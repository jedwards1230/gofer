package uicopy

// Copy for `/compact` (internal/tui/compact.go).

// CompactingNote is the status line a `/compact` dispatched from OFF the
// attach screen leaves behind, where the transcript-tail indicator is not on
// screen to be seen.
//
// Its exact text is load-bearing: the compactDoneMsg handler clears the status
// only when it still holds this string, so a note some other event set in the
// meantime is newer and wins. Both the writer and that comparison read it from
// here, so they cannot drift apart.
const CompactingNote = "Compacting context…"

// CompactNeedsSession is `/compact`'s refusal with nothing attached — unlike
// /new or /resume there is no "apply to the default" reading for a command
// that summarizes a specific session's own history.
const CompactNeedsSession = "/compact needs an attached session"

// CompactionFailed reports a session.compaction_failed event's reason. The
// TUI says so rather than letting the indicator blink out as if the
// compaction had succeeded.
func CompactionFailed(reason string) string { return "compaction failed: " + reason }
