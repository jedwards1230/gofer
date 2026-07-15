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

// ErrEmptyModel indicates [Supervisor.SetModel] was called with an empty
// model string — there is no sensible "unset the model" operation, unlike an
// empty [CreateOptions.Model]/[ResumeOptions.Model], which resolves to a
// credential-driven default at session construction. A live model swap has
// no such default to fall back to, so an empty target is rejected outright.
var ErrEmptyModel = errors.New("model must not be empty")

// ErrCrossProvider indicates a SetModel call asked to change a session to a
// model served by a DIFFERENT provider than its current one. A runner's
// provider client is bound to one backend at session creation, so a live
// model swap can only move within the same provider (e.g. claude-sonnet-5 ->
// claude-opus-4-8); switching providers requires a new session. Callers that
// branch on this (e.g. "set the new-session default only") should compare
// provider.Lookup(current).Provider vs provider.Lookup(target).Provider
// themselves before calling — the concrete error type is not carried across
// the daemon wire (see internal/daemonbridge).
var ErrCrossProvider = errors.New("cannot change model across providers")
