package router

import "github.com/jedwards1230/gofer/internal/daemon"

// skewClass is how a worker's advertised versions relate to the router's own —
// the M6 §6 version-skew decision, reduced to the four cases the router routes
// differently. It is computed once per worker (from the authoritative
// gofer/hello handshake) and stored on its [workerHandle].
//
// The classes are ordered by severity of the routing consequence, not by how
// different the binaries are: only [skewWire] restricts what the router will
// ask of a worker.
type skewClass int

const (
	// skewNone: the worker reports the router's own wire version AND the same
	// binary version (or both sides are unidentified). Route everything.
	skewNone skewClass = iota
	// skewBinary: the wire version MATCHES but the binary version differs — an
	// old-binary worker still speaking the current protocol. Fully routable; see
	// the package doc's session-pinning rationale.
	skewBinary
	// skewWire: the router↔worker wire-contract version differs. The protocol
	// itself cannot be trusted, so the router adopts the worker for the
	// observe / permission-reply / finish subset only and refuses to give it NEW
	// work ([ErrWorkerSkewed]) — design §6's literal compatibility window.
	skewWire
	// skewUnknown: the wire version matches but exactly one side identified its
	// binary, so the two are not comparable — most commonly a worker that
	// predates the version-reporting wiring. Routed exactly like skewBinary
	// (surface it, do not refuse): refusing here would brick every worker built
	// before this slice.
	skewUnknown
)

// String renders a class for structured logs.
func (c skewClass) String() string {
	switch c {
	case skewNone:
		return "none"
	case skewBinary:
		return "binary"
	case skewWire:
		return "wire"
	case skewUnknown:
		return "unknown"
	default:
		return "invalid"
	}
}

// refusesNewWork reports whether a worker in this class may be given NEW work —
// a prompt ([Supervisor.Send]) or a model change ([Supervisor.SetModel]). Only a
// WIRE mismatch refuses: the protocol carrying the request is itself in doubt,
// so the router restricts the connection to the additive observe/reply/finish
// subset design §6 guarantees across a version gap.
//
// A BINARY mismatch deliberately does NOT refuse — see classifySkew and the
// package doc.
func (c skewClass) refusesNewWork() bool {
	return c == skewWire
}

// classifySkew is the pure M6 §6 version decision: given the router's own build
// and wire versions and the versions a worker advertised (authoritatively, via
// gofer/hello), which [skewClass] does that worker fall into.
//
// Wire dominates binary: a wire mismatch means the router cannot trust the
// protocol regardless of what the binaries say, so it is classified first and
// the binary comparison is not even attempted.
//
// Binary comparison is EXACT, not N-1 (design §6: "N-0 full + skew-observe-only
// (Phase 3), widen if ever needed"), and it is a plain string compare — which
// intentionally treats a "-dirty" build as different from its base commit,
// because it genuinely is a different binary.
//
// The unknown case is "exactly one side identified itself". Two EMPTY versions
// are skewNone, not skewUnknown: an embedder (or a test) that configures no
// version on either tier has no evidence of skew, and classifying that as
// unknown would log a skew warning for every such worker forever.
func classifySkew(routerBinary, workerBinary string, routerWire, workerWire int) skewClass {
	if routerWire != workerWire {
		return skewWire
	}
	if routerBinary == workerBinary {
		return skewNone
	}
	if routerBinary == "" || workerBinary == "" {
		return skewUnknown
	}
	return skewBinary
}

// classifyWorker classifies a worker from its authoritative gofer/hello result
// against this router's own versions, logging any non-[skewNone] outcome once,
// at handle-creation time — the single place both handle-creation paths
// ([Supervisor.Create] and [Supervisor.adoptWorker]) record what they are about
// to route to.
//
// It logs at Warn for a wire mismatch (the router will refuse new work on that
// session, so an operator should see it) and at Info for a binary/unknown
// mismatch (expected and benign during a rolling upgrade — that IS the session
// pinning M6 sells).
func (s *Supervisor) classifyWorker(sessionID string, hello daemon.HelloResult) skewClass {
	skew := classifySkew(s.version, hello.BinaryVersion, daemon.WireVersion, hello.WireVersion)
	switch skew {
	case skewNone:
	case skewWire:
		s.log.Warn("worker wire-version skew: adopting for observe/reply/finish only; new work refused",
			"session", sessionID, "workerWire", hello.WireVersion, "routerWire", daemon.WireVersion,
			"workerBinary", hello.BinaryVersion, "routerBinary", s.version)
	default:
		s.log.Info("worker binary-version skew: session pinned to its own binary",
			"session", sessionID, "skew", skew.String(),
			"workerBinary", hello.BinaryVersion, "routerBinary", s.version)
	}
	return skew
}
