package router

import "errors"

// ErrNotLive is returned by the router's per-session methods (Send, SubscribeLive,
// Interrupt, SetModel, Reply, Kill) when no live worker owns the session id — it
// crashed, was killed, was archived, or never existed. It mirrors
// [github.com/jedwards1230/gofer/internal/supervisor.ErrNotLive]'s role for the
// in-process supervisor; the daemon surfaces either as a plain JSON-RPC
// application error, so the concrete type need not match.
var ErrNotLive = errors.New("router: session not live")

// ErrAtCapacity is the typed error [Supervisor.Create] returns when the router
// already hosts its [Config.MaxWorkers] limit of live workers (counting in-flight
// spawns): the request is REFUSED before anything is forked, dialed, or written
// to disk, so a capacity refusal leaves no artifact and no half-started process.
// The wrapping error carries the observed count and the configured cap.
//
// The daemon's session/new handler wraps any Create error with its application
// error code (see internal/daemon's handleSessionNew → appError), so a client
// sees a clean JSON-RPC application error it can surface and retry after a
// session ends — not a transport failure or a dropped connection.
var ErrAtCapacity = errors.New("router: worker capacity reached")

// ErrResumeUnsupported is the typed error [Supervisor.Resume] returns: spawning a
// fresh worker for an offline (or old-binary) session is Phase 4 of M6, not this
// slice. The daemon's session/load handler surfaces it as a clean application
// error rather than panicking (see the package doc's Phase-1 cuts).
var ErrResumeUnsupported = errors.New("router: resume not yet supported in worker mode")

// ErrEmitConfigUnsupported is the typed error [Supervisor.EmitConfigOptions]
// returns: there is no wire method for a client to make a worker emit an
// arbitrary config-options snapshot, and it is off the crash-isolation critical
// path. The daemon's advertiseModelChange treats this as a non-fatal, Debug-logged
// outcome, and the real config_option_update a model swap produces still reaches
// clients via the worker's own emit (see [Supervisor.SetModel]).
var ErrEmitConfigUnsupported = errors.New("router: emit config options not supported in worker mode")

// ErrWorkerSkewed is the typed error [Supervisor.Send] and [Supervisor.SetModel]
// return when the owning worker speaks a DIFFERENT router↔worker wire version
// than this router: the protocol carrying the request cannot be trusted, so the
// router forwards only the additive observe / permission-reply / finish subset
// design §6 guarantees across a version gap and refuses to give that worker NEW
// work. The session stays live, keeps streaming, keeps answering permissions, and
// its in-flight turn finishes normally — it simply accepts no further prompts.
//
// It is deliberately NOT returned for a BINARY-version mismatch on a matching
// wire: prompting an older worker merely runs another turn on that binary, which
// is session pinning rather than a hazard (see the package doc).
//
// Like [ErrResumeUnsupported], the daemon surfaces it as a plain JSON-RPC
// application error — no distinct error code, and no first-class TUI state.
var ErrWorkerSkewed = errors.New("router: worker wire version skewed")
