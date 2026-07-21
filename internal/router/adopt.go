package router

import (
	"context"
	"errors"
	"os"
	"os/exec"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/wirestream"
)

// adoptExistingWorkers scans for still-alive DETACHED workers left by a PRIOR
// router and re-attaches to each — the second half of M6 Phase 2 (design §4),
// so a router restart re-adopts its in-flight sessions instead of orphaning
// them. It is the whole point of process isolation: the router (and CLI) can be
// restarted at will while turns keep running on their worker's binary.
//
// It reads every `<uuid>.json` endpoint file ([daemon.ListWorkerEndpoints]) and
// runs the §4 adopt/stale decision on each (see [Supervisor.adoptWorker]).
// Best-effort by contract: a scan or per-worker failure NEVER fails
// construction — a router with an empty roster is still a usable router, since
// every [Supervisor.Create] spawns a fresh worker. It runs once, from [New],
// before the router serves any request, so it needs no locking beyond
// adoptWorker's own registry insert.
func (s *Supervisor) adoptExistingWorkers() {
	entries, err := daemon.ListWorkerEndpoints()
	if err != nil {
		// A scan failure (an unreadable workers dir) is non-fatal: start empty.
		s.log.Warn("adoption scan failed; starting with an empty roster", "err", err)
		return
	}
	adopted := 0
	for _, entry := range entries {
		if s.adoptWorker(entry) {
			adopted++
		}
	}
	if len(entries) > 0 {
		s.log.Info("adoption scan complete", "endpoints", len(entries), "adopted", adopted)
	}
}

// adoptWorker applies the §4 reattach/adoption decision to one worker endpoint
// entry and, on success, registers an ADOPTED handle and starts its reaper. It
// returns whether the worker was adopted (for the scan's summary count).
//
// The decision, in order (design §4 failure matrix):
//
//  1. pid gone ([daemon.ProcessAlive] false) → STALE: the worker crashed/exited
//     without cleaning up. GC its endpoint+socket residue; the session is now
//     just an offline journal.
//  2. version skew from the endpoint file's pre-dial hint (design §6) →
//     ADVISORY ONLY: no skew class refuses adoption, so the hint cannot decide
//     anything on its own; the handshake below is authoritative. It is reported
//     at step 5 only when the two DISAGREE (a stale endpoint file).
//  3. dial refused → STALE: the socket has no listener even though the pid probe
//     passed (a reused pid, or a worker mid-crash). GC and treat offline.
//  4. gofer/hello fails → LEAVE unadopted: the worker dialed but is
//     unresponsive/degraded; its pid is live, so do not GC — leave it for a
//     later reap, and do not route to it. (A FAILED handshake is not a skewed
//     one: it means the router learned nothing at all, not that it learned the
//     worker is old.) EXCEPT [daemon.ErrHelloUnsupported] — a worker predating
//     the handshake — which adopts with a zero HelloResult (⇒ skewWire, the
//     strict-but-adoptable side of the policy).
//  5. classify the authoritative handshake into a [skewClass] and ADOPT
//     regardless of the class — the class is recorded on the handle and governs
//     only what the router will later ASK of the worker: a wire mismatch
//     refuses new prompts/model changes ([ErrWorkerSkewed]) while still
//     observing, replying to permissions, and letting the turn finish; a binary
//     mismatch is fully routable (session pinning — see the package doc).
//  6. ADOPT: wrap the connection in a [wirestream.Reconstructor],
//     re-attach via [wirestream.Reconstructor.Load] FIRST — which settles
//     history and re-surfaces any still-open PermissionRequested into the broker
//     (§7) — then register the ADOPTED handle (cmd==nil; wait fires off
//     [daemon.Client.Done]; pid from the endpoint file, EXCEPT when it names the
//     router's own pid — see the self-pid guard) and start its reaper.
func (s *Supervisor) adoptWorker(entry daemon.WorkerEndpointEntry) bool {
	id := entry.UUID
	ep := entry.Endpoint

	// 1. Liveness probe (signal-0). A dead pid ⇒ stale residue.
	if !daemon.ProcessAlive(ep.PID) {
		s.log.Info("adoption: worker pid gone; GC stale artifacts", "session", id, "pid", ep.PID)
		removeWorkerArtifacts(id)
		return false
	}

	// 2. Pre-dial version hint from the endpoint file (design §6). ADVISORY
	//    ONLY: under this slice's policy no skew class refuses adoption (a wire
	//    mismatch adopts for the observe/reply/finish subset; a binary mismatch
	//    adopts fully), so the hint cannot short-circuit the decision. It is
	//    computed HERE — the pre-dial point the design describes, and the cheap
	//    signal a future policy (or a fleet router with no local endpoint file)
	//    needs — but it is only REPORTED at step 5, and then only when it
	//    disagrees with the authoritative handshake. Agreement is the normal
	//    case and stays silent; disagreement means a STALE endpoint file, which
	//    is the condition §6 actually wants an operator to be able to see.
	hint := classifySkew(s.version, ep.BinaryVersion, daemon.WireVersion, ep.WireVersion)

	// 3. Dial. A refused dial ⇒ stale socket (design §4).
	dialCtx, cancel := context.WithTimeout(context.Background(), workerDialTimeout)
	client, err := daemon.Dial(dialCtx, ep.Addr, "")
	cancel()
	if err != nil {
		s.log.Info("adoption: dial refused; GC stale artifacts", "session", id, "addr", ep.Addr, "err", err)
		removeWorkerArtifacts(id)
		return false
	}

	// 4. Authoritative version handshake (design §6). Unresponsive ⇒ leave.
	//
	//    ONE error is not "unresponsive": [daemon.ErrHelloUnsupported] — the
	//    method-not-found reply of a worker built BEFORE gofer/hello existed.
	//    That worker is healthy, it just cannot describe itself, which is
	//    precisely the case this slice adopts rather than refuses (refusing
	//    would brick every pre-handshake worker — the same reasoning that makes
	//    skewUnknown adoptable). Synthesize a zero HelloResult and continue: its
	//    absent wire version classifies as skewWire, so the session is adopted
	//    on the STRICT side of the policy — observable, replyable, and able to
	//    finish, but given no new work.
	helloCtx, cancel := wireCallCtx()
	hello, err := client.Hello(helloCtx)
	cancel()
	if err != nil {
		if !errors.Is(err, daemon.ErrHelloUnsupported) {
			s.log.Warn("adoption: gofer/hello failed; leaving worker unadopted", "session", id, "err", err)
			_ = client.Close()
			return false
		}
		s.log.Info("adoption: worker predates gofer/hello; adopting it unidentified (no new work)",
			"session", id, "err", err)
		hello = daemon.HelloResult{}
	}

	// 5. Authoritative version classification (design §6). Every class ADOPTS —
	//    the class only decides what the router will subsequently ask of the
	//    worker (see [skewClass.refusesNewWork]).
	skew := s.classifyWorker(id, hello)

	//    Report the step-2 endpoint-file hint ONLY when it disagrees with the
	//    authoritative class: that disagreement IS the stale-endpoint-file
	//    condition (design §6's "it can be stale"), and it is otherwise
	//    invisible at any log level. Agreement is the normal case and is silent.
	if hint != skew {
		s.log.Info("adoption: endpoint-file version hint disagrees with the gofer/hello handshake (stale endpoint file)",
			"session", id, "hint", hint.String(), "handshake", skew.String(),
			"hintWire", ep.WireVersion, "hintBinary", ep.BinaryVersion,
			"workerWire", hello.WireVersion, "workerBinary", hello.BinaryVersion)
	}

	// 6. Adopt. Load FIRST so history + any open permission request re-surface
	//    into the reconstructed broker (retained by event.WithReplay, §7) before
	//    any consumer subscribes; only THEN is the session considered attached.
	//    Referencing via Load (not a first-reference SubscribeLive) also means a
	//    later SubscribeLive on this session won't re-trigger a history replay
	//    onto the live stream (see wirestream's Load/SubscribeLive contract).
	// The event sink is installed at construction (see [Supervisor.eventSink] and
	// wirestream's Option contract) so the adopted session's turn — which is
	// running RIGHT NOW on the worker, with no prompt handler of ours observing
	// it — streams to this router's attached clients.
	//
	// INVARIANT — this Load must happen BEFORE [Supervisor.SetEventRelay]: the
	// sink fires for replayed history frames exactly as it does for live ones, so
	// a Load with the relay already installed would re-broadcast this session's
	// whole history to attached clients as if it were live. Adoption runs inside
	// [New], which returns before the daemon that injects the relay is even
	// constructed, so the ordering holds by construction rather than by
	// convention. The other Load — [Supervisor.Resume] spawning a worker for an
	// offline session — cannot rely on that ordering (it runs while serving), so
	// it suppresses the sink for its replay instead (see [Supervisor.eventSink]'s
	// replaySuppressed guard). Adoption needs no such guard: a nil guard here
	// leaves the sink ungated, and the relay it would reach does not yet exist.
	rec := wirestream.New(client, wirestream.WithEventSink(s.eventSink(id, nil)))
	loadCtx, cancel := wireCallCtx()
	if lerr := rec.Load(loadCtx, id); lerr != nil {
		// A load that did not settle is non-fatal — the live stream still works;
		// the session simply starts from whatever events arrive next.
		s.log.Warn("adoption: history load did not settle; adopting live-only", "session", id, "err", lerr)
	}
	cancel()

	// SELF-PID GUARD. An adopted handle carries no *exec.Cmd, so Kill/Archive
	// best-effort SIGKILL the pid the ENDPOINT FILE advertised (see
	// killHandleProcess). If that file ever names the ROUTER'S OWN pid, a single
	// `gofer kill <session>` would SIGKILL the daemon itself and take routing
	// down for EVERY session — a blast radius far worse than the pid-reuse
	// caveat below, and the exact shape a hand-written or copied endpoint file
	// produces. Record pid 0 instead: killHandleProcess's `h.pid > 0` check then
	// makes the signal a no-op, and the handle is still reconciled the normal
	// way — Kill/Archive first ask the worker to wind down over its socket, and
	// the reaper fires when that connection closes (adoptedWait).
	pid := ep.PID
	if pid == os.Getpid() {
		s.log.Warn("adoption: endpoint file advertises the router's own pid; recording pid 0 so kill cannot signal the daemon",
			"session", id, "pid", ep.PID)
		pid = 0
	}

	h := newWorkerHandle(id, nil, client, rec, pid, adoptedWait(client), hello, skew)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = rec.Close()
		return false
	}
	if _, exists := s.workers[id]; exists {
		// A duplicate endpoint for an id already adopted this scan — drop it
		// rather than clobber the live handle (should not happen: ids are unique
		// filenames, but be defensive).
		s.mu.Unlock()
		_ = rec.Close()
		return false
	}
	s.workers[id] = h
	s.reapWG.Add(1)
	s.watcherWG.Add(1)
	s.mu.Unlock()
	go s.reap(h)
	go s.watchSession(h)

	s.log.Info("adopted live worker", "session", id, "addr", ep.Addr, "pid", ep.PID, "binaryVersion", hello.BinaryVersion)
	return true
}

// SetPermissionRelay wires the OUTER daemon's permission fan-out into the router
// (design §7, F1). It must be called once, after the daemon that hosts this
// router is constructed and BEFORE it serves — the daemon does not exist yet at
// [New] time (it takes this router as its Supervisor), so the bridge is injected
// here rather than in the constructor.
//
// The gap it closes: a session whose turn runs on a worker has no [daemon]
// session/prompt handler in THIS process observing its event stream to record
// permission routes and broadcast requests. Without a standing watcher a
// re-surfaced (or newly asked) permission on such a session would never reach the
// daemon's route table or its attached clients — invisible and unanswerable. The
// per-session watcher ([Supervisor.watchSession]) drives the relay so the daemon
// records the call→session route (making handlePermissionReply resolve) and fans
// the request out exactly as the prompt handler would.
//
// # Contract change in slice 3b: no longer adopted-sessions-only
//
// This method used to START the watchers, and only for sessions adopted before
// it ran — its documented "adopted sessions only" invariant. Watchers now start
// from BOTH paths a handle comes into existence, [Supervisor.Create] as well as
// adopt, because the same subscription also maintains the pushed roster cache
// (see rostercache.go), which every live session needs. So this method now only
// PUBLISHES the relay; the watchers were already running.
//
// The widening is safe: a CREATED session's turns are usually driven by a
// client's session/prompt, whose handler ALSO observes the same permission
// events, so the daemon sees two observers. Its fan-out is already
// first-observer-gated for exactly this reason ([daemon.Daemon.recordPermRoute] /
// clearPendingPerm), so the request is still routed and broadcast exactly once.
//
// Because the watchers predate the relay, requests observed BEFORE this call
// would otherwise be lost — precisely the §7 case of a permission re-surfaced by
// adoption's replay. So this also FLUSHES each handle's still-open requests to
// the newly installed relay. The flush and a concurrent watcher cannot lose a
// request between them: the watcher records into the open set BEFORE it reads the
// relay, so any request the flush's snapshot misses is one whose watcher must
// then observe the already-published relay. It can deliver a request TWICE
// (flush + watcher), which the first-observer gate absorbs. A nil relay, or a
// closed router, is a no-op.
func (s *Supervisor) SetPermissionRelay(relay daemon.PermissionRelay) {
	s.mu.Lock()
	if relay == nil || s.closed {
		s.mu.Unlock()
		return
	}
	s.permRelay = relay
	handles := make([]*workerHandle, 0, len(s.workers))
	for _, h := range s.workers {
		handles = append(handles, h)
	}
	s.mu.Unlock()

	// Flush outside mu: RequestPermission broadcasts to peers (socket writes).
	for _, h := range handles {
		for _, pe := range h.openPermissions() {
			relay.RequestPermission(h.id, pe)
		}
	}
}

// permissionRelay returns the installed [daemon.PermissionRelay], or nil.
func (s *Supervisor) permissionRelay() daemon.PermissionRelay {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.permRelay
}

// watchSession is the standing per-session watcher, started for EVERY live
// handle (spawned or adopted) right after it is registered. It subscribes to h's
// reconstructed broker WITH replay and drives the two pieces of router state
// that are projections of a session's event stream:
//
//   - the PUSHED ROSTER CACHE (rostercache.go) — this goroutine is its single
//     writer, which is what lets Roster/List serve snapshots lock-free and
//     RPC-free;
//   - the PERMISSION RELAY (design §7) — the outer daemon's route table and
//     client fan-out, so a permission asked (or re-surfaced) on a session no
//     prompt handler here is driving is still visible and answerable.
//
// It exits when the broker closes (the handle was reaped, or the router shut
// down and closed every rec) or reaperStop fires — so it never outlives its
// session and is joined by [Supervisor.Close] via watcherWG.
//
// Using the replaying Subscribe is deliberate: a request re-surfaced by
// adoption's Load is retained on the broker, and Subscribe replays it so the
// watcher sees it even though it was published before the watcher existed. A
// retained request that ALSO has a retained resolution replays as
// requested-then-resolved, netting to a no-op — only a still-OPEN request stays
// routed, which is exactly the mid-approval state §7 must survive.
//
// The roster seed runs AFTER the subscription is live (see [Supervisor.seedRosterCache]),
// so nothing published during the seeding RPC is lost.
func (s *Supervisor) watchSession(h *workerHandle) {
	defer s.watcherWG.Done()
	sub, err := h.rec.Subscribe(context.Background(), h.id)
	if err != nil {
		// Reconstructor already closed (handle reaped before the watcher started).
		// Mark the seed settled anyway so nothing waits on a cache that will never
		// be filled.
		h.markSeeded()
		return
	}
	defer sub.Close()

	s.seedRosterCache(h)

	for {
		select {
		case ev, ok := <-sub.C:
			if !ok {
				return // broker closed: session gone or router shutting down
			}
			s.observeSessionEvent(h, ev)
		case <-s.reaperStop:
			return
		}
	}
}

// observeSessionEvent applies one event to the watcher's two responsibilities.
// The permission open-set is updated BEFORE the relay call — see
// [Supervisor.SetPermissionRelay] for why that order is what makes a relay
// installed concurrently unable to miss an open request — and the cached roster
// row is folded last, so its Pending count reads the already-updated set.
func (s *Supervisor) observeSessionEvent(h *workerHandle, ev event.Event) {
	switch pe := ev.(type) {
	case event.PermissionRequested:
		h.trackPermission(pe)
		if relay := s.permissionRelay(); relay != nil {
			// Idempotent under duplicate delivery: even if the replay re-emits a
			// request a concurrent prompt handler also observed, the relay's
			// route-first / pending-presence gates broadcast it exactly once, and a
			// client answers by call id regardless.
			relay.RequestPermission(h.id, pe)
		}
	case event.PermissionResolved:
		h.forgetPermission(pe.ID)
		if relay := s.permissionRelay(); relay != nil {
			relay.ResolvePermission(h.id, pe)
		}
	}
	h.applyRosterEvent(ev)
	// Wake any AwaitSettled waiter to re-check the freshly-folded status — the
	// idle transition that means the turn is journaled (see [Supervisor.AwaitSettled]).
	h.pokeSettle()
}

// pokeSettle wakes an [Supervisor.AwaitSettled] waiter on h, non-blocking and
// coalescing: a poke already pending in the buffered-1 settleCh is enough, since
// the waiter re-reads the authoritative status on every wake.
func (h *workerHandle) pokeSettle() {
	select {
	case h.settleCh <- struct{}{}:
	default:
	}
}

// adoptedWait returns the exit-signal channel for an ADOPTED worker: since the
// router did not spawn it there is no *exec.Cmd to [daemon.Reap], so the only
// crash signal is the client connection closing — its read loop exiting on the
// worker's process death (or an explicit [daemon.Client.Close] during teardown).
// The returned channel mirrors daemon.Reap's contract ([Supervisor.reap] selects
// on it identically for spawned and adopted handles): buffered cap 1 so the send
// completes even after the router has stopped caring at shutdown, exactly one
// value ever sent.
//
// F4 — goroutine lifetime: the spawned goroutine blocks only on client.Done,
// which the [daemon.Client] read loop closes exactly once and unconditionally
// when the connection ends — either the worker died, or teardown ([Supervisor.reap]
// or [Supervisor.Close]) called rec.Close, which closes the client. So it always
// terminates; it is deliberately NOT joined by a WaitGroup (unlike the permission
// watchers) because its single blocking receive on an always-closed channel
// cannot outlive the handle's client, and the buffered send never blocks — there
// is nothing for a join to wait on that closing the client does not already
// guarantee.
func adoptedWait(client *daemon.Client) <-chan error {
	ch := make(chan error, 1)
	go func() {
		<-client.Done()
		ch <- nil
	}()
	return ch
}

// killHandleProcess terminates a worker's process, branching on how the router
// holds it (see [workerHandle]'s SPAWNED-vs-ADOPTED doc): a SPAWNED worker is
// signalled through its *exec.Cmd (cmd.Process), while an ADOPTED worker (cmd
// nil — a process this router did not start) is best-effort SIGKILLed by the pid
// its endpoint file advertised. Both paths are best-effort: a worker that
// already exited is not an error (the reaper reconciles the handle regardless).
// The single kill seam both [Supervisor.Kill] and [Supervisor.Archive] use so
// neither nil-derefs an adopted handle's absent cmd.
//
// F3 — pid-reuse caveat: the adopted path signals a pid the router did not
// spawn, so if that worker already exited and the OS recycled its pid, the
// SIGKILL could land on an unrelated process. This is the inherent, accepted
// hazard of holding a worker only by its advertised pid; it is bounded in
// practice (the router still holds a live socket to the worker, and Kill/Archive
// first ask the worker over that socket to wind down, so a recycled pid means
// the socket was already dead and the handle about to be reaped anyway).
//
// RE-EVALUATED in slice 3a, when adoption widened: before 3a a version-skewed
// worker was never adopted, so most endpoint files never produced a handle and
// this path was largely unreachable. Now EVERY live, dialable worker is adopted
// whatever its versions, so the pid on the endpoint file is genuinely signalled.
// The caveat was deliberately RE-AFFIRMED as "document, don't gate" for the
// general case, and the reasoning is worth not re-deriving:
//
//   - The endpoint file is written mode 0600 inside the 0700 per-uid runtime
//     directory, so its content crosses NO privilege boundary — only the user
//     the router already runs as can write a pid into it, and that user can
//     signal their own processes directly anyway.
//   - Adoption is not credulous: reaching this handle at all required a live pid
//     ([daemon.ProcessAlive]), a successful dial to the advertised socket, AND a
//     gofer/hello reply on it. A recycled pid that also happens to be serving a
//     gofer worker socket for this exact session uuid is not a realistic state.
//   - A peer-credential check (SO_PEERCRED / LOCAL_PEERPID on the connection,
//     which would make the pid authoritative rather than advertised) was
//     considered and REJECTED for this slice: it is per-OS plumbing that buys
//     nothing against the two bullets above.
//
// The ONE case that was NOT left as documentation is the router's own pid — see
// the self-pid guard in [Supervisor.adoptWorker]. That one is not "some
// unrelated process" but the daemon itself, so it is gated at adoption time by
// recording pid 0, which the `h.pid > 0` check below turns into a no-op.
func killHandleProcess(h *workerHandle) {
	if h.cmd != nil {
		if h.cmd.Process != nil {
			_ = h.cmd.Process.Kill()
		}
		return
	}
	// Adopted: no *exec.Cmd — SIGKILL the endpoint-advertised pid. os.FindProcess
	// never fails on unix; Process.Kill sends SIGKILL and tolerates an
	// already-dead process (os.ErrProcessDone), so no error handling is needed.
	if h.pid > 0 {
		if proc, err := os.FindProcess(h.pid); err == nil {
			_ = proc.Kill()
		}
	}
}

// removeWorkerArtifacts best-effort garbage-collects a worker's on-disk runtime
// residue — its `<uuid>.json` endpoint file and `<uuid>.sock` socket — after the
// worker is gone (a failed spawn, a worker that exited before serving, or a
// STALE entry found during the adoption scan). The `<uuid>.lock` is deliberately
// NOT removed: unlinking a lock file races any concurrent holder (a fresh worker
// could open a new inode at the same path), and a dead worker's advisory flock
// is auto-released by the kernel on process exit anyway (see [daemon.LockWorker]).
// Every step is best-effort — a missing file is the expected common case, and a
// GC failure is never worth failing a create or a scan over.
func removeWorkerArtifacts(sessionID string) {
	_ = daemon.RemoveWorkerEndpoint(sessionID)
	if sockPath, err := daemon.WorkerSocketPath(sessionID); err == nil {
		if rmErr := os.Remove(sockPath); rmErr != nil && !os.IsNotExist(rmErr) {
			// Best-effort only: a leftover socket is harmless (a fresh worker
			// removes a stale one before binding), so a removal error is dropped.
			_ = rmErr
		}
	}
}

// cleanupSpawnedWorker tears down a SPAWNED worker whose registration failed
// before a reaper goroutine took ownership: it [terminate]s the process (killing
// it and draining its sole [daemon.Reap] result so it is waited exactly once)
// and then sweeps its endpoint/socket artifacts. It replaces a bare terminate at
// [Supervisor.Create]'s error paths so a create that spawned a worker but then
// failed (dial/session-new/id-mismatch) leaves no stale `<uuid>.json`/socket for
// a later adoption scan to trip over. Only for a worker still owned by the
// caller (not yet handed to [Supervisor.reap]); a worker that already exited must
// use [removeWorkerArtifacts] directly, since terminate's drain would block on a
// wait result already consumed.
func cleanupSpawnedWorker(sessionID string, cmd *exec.Cmd, waitCh <-chan error) {
	terminate(cmd, waitCh)
	removeWorkerArtifacts(sessionID)
}
