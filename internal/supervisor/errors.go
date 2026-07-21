package supervisor

import "errors"

// ErrNotLive indicates the requested session id is not in the supervisor's
// live roster — either it was never created/resumed in this process, or a
// prior [Supervisor.Kill]/[Supervisor.Archive] already dropped it. The
// on-disk journal, if any, is unaffected; see [Supervisor.List] to find it.
var ErrNotLive = errors.New("session not live")

// ErrNoPendingPermission indicates [Supervisor.ExplainPermission] was asked
// about a permission call id that is not outstanding on the named session —
// it already resolved (by any client, or by an interrupt), it belongs to a
// different session, or it never existed. It is deliberately distinct from an
// empty rationale: "that request is no longer pending" and "that request was
// gated for no stated reason" are different answers, and a client showing the
// second when the first is true would be telling a user their prompt is still
// live when it is not.
var ErrNoPendingPermission = errors.New("no pending permission request with that id")

// ErrRunning indicates [Supervisor.Archive] was called on a session that is
// still active — a turn in flight, or queued-but-not-yet-dispatched prompts
// (both surface as StatusWorking). Interrupt or kill it first.
var ErrRunning = errors.New("session is running or has queued work")

// ErrClosed indicates the supervisor itself has been closed; no further
// session operations are accepted.
var ErrClosed = errors.New("supervisor closed")

// ErrEmptyModel indicates [Supervisor.SetModel] was called with an empty
// model string — there is no sensible "unset the model" operation, so an
// empty target is rejected outright. Its create/resume counterpart is
// [ErrNoModel].
var ErrEmptyModel = errors.New("model must not be empty")

// ErrNoModel indicates [Supervisor.Create]/[Supervisor.Resume] was reached
// with no model at all. The supervisor resolves no default of its own — every
// caller owns that decision before it gets here (`gofer run`'s -m or
// resolveRunModel, the daemon's Config.DefaultModel, the TUI bridge's
// defaultModel) — so an empty model means the caller's own resolution came up
// empty and quietly passed the gap along.
//
// It exists because that gap used to reach the SDK instead, where it
// surfaced as `runner: unknown model ""` — an error naming neither the cause
// nor a remedy, and the one users actually saw (issue #147). Callers wrap it
// with the remedy that applies to them.
var ErrNoModel = errors.New("no model configured")

// ErrInvalidEffort indicates [Supervisor.SetEffort] was called with a level
// outside the SDK's unified reasoning-effort vocabulary ([provider.ValidEffort]:
// "low", "medium", "high", or "" to clear back to the provider's default).
//
// Note the asymmetry with [ErrEmptyModel], and that it is deliberate: an empty
// MODEL is meaningless, whereas an empty EFFORT is the documented way to clear
// a previously-set level, so it is accepted rather than rejected. The sentinel
// exists so a caller can branch on "you asked for a level that does not exist"
// without string-matching; the offending value is named in the wrapping message.
var ErrInvalidEffort = errors.New("unknown reasoning effort")

// ErrCrossProvider indicates a SetModel call asked to change a session to a
// model served by a DIFFERENT provider than its current one. A runner's
// provider client is bound to one backend at session creation, so a live
// model swap can only move within the same provider (e.g. claude-sonnet-5 ->
// claude-opus-4-8); switching providers requires a new session. Callers that
// branch on this (e.g. "set the new-session default only") should compare
// provider.Resolve(current).Provider vs provider.Resolve(target).Provider
// themselves before calling — the concrete error type is not carried across
// the daemon wire (see internal/daemonbridge). Resolve, not Lookup: an
// unregistered id is still runnable and still has a knowable provider, and
// Lookup would report neither.
var ErrCrossProvider = errors.New("cannot change model across providers")
