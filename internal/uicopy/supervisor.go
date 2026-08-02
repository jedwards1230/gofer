package uicopy

// Copy for the roster's session-status labels
// (internal/tui/supervisor.go's SessionStatus.String).

// SessionStatusWorking labels a session with a turn in flight.
const SessionStatusWorking = "Working"

// SessionStatusNeedsInput labels a session awaiting the user. It is
// deliberately distinct from an idle session's label: a reloaded or
// just-resumed row must not read as a real prompt waiting to be answered.
const SessionStatusNeedsInput = "Needs input"

// SessionStatusFinished labels a terminal session — completed, killed, or
// archived. Its journal is retained either way, so the word describes the turn
// loop rather than promising the session is gone.
const SessionStatusFinished = "Finished"

// SessionStatusIdle labels a session at rest that is NOT awaiting the user: a
// reloaded offline row, or one resumed from disk and not yet prompted.
const SessionStatusIdle = "Idle"

// SessionStatusUnknown labels a status outside the enum, which only a version
// skew between client and daemon can produce. It says so rather than guessing
// at one of the four real groupings.
const SessionStatusUnknown = "Unknown"
