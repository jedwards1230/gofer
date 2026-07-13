package supervisor

import "errors"

// ErrNotLive indicates the requested session id is not in the supervisor's
// live roster — either it was never created/resumed in this process, or a
// prior [Supervisor.Kill]/[Supervisor.Archive] already dropped it. The
// on-disk journal, if any, is unaffected; see [Supervisor.List] to find it.
var ErrNotLive = errors.New("session not live")

// ErrRunning indicates [Supervisor.Archive] was called on a session that is
// still active — a turn in flight, or queued-but-not-yet-dispatched prompts
// (both surface as StatusWorking). Interrupt or kill it first.
var ErrRunning = errors.New("session is running or has queued work")

// ErrClosed indicates the supervisor itself has been closed; no further
// session operations are accepted.
var ErrClosed = errors.New("supervisor closed")
