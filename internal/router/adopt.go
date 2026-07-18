package router

import (
	"context"
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
//  2. wire-version skew from the endpoint file's pre-dial hint (design §6) →
//     LEAVE unadopted: this slice supports only the router's own wire version
//     (N-0 full; skew routing is Phase 3), so a skewed-but-live worker is left
//     running detached for a future skew-aware router — NOT GC'd (it holds a
//     live session) and NOT killed.
//  3. dial refused → STALE: the socket has no listener even though the pid probe
//     passed (a reused pid, or a worker mid-crash). GC and treat offline.
//  4. gofer/hello fails → LEAVE unadopted: the worker dialed but is
//     unresponsive/degraded; its pid is live, so do not GC — leave it for a
//     later reap, and do not route to it.
//  5. wire-version skew from the authoritative handshake → LEAVE unadopted (as
//     step 2, but post-dial).
//  6. otherwise → ADOPT: wrap the connection in a [wirestream.Reconstructor],
//     re-attach via [wirestream.Reconstructor.Load] FIRST — which settles
//     history and re-surfaces any still-open PermissionRequested into the broker
//     (§7) — then register the ADOPTED handle (cmd==nil; wait fires off
//     [daemon.Client.Done]; pid from the endpoint file) and start its reaper.
func (s *Supervisor) adoptWorker(entry daemon.WorkerEndpointEntry) bool {
	id := entry.UUID
	ep := entry.Endpoint

	// 1. Liveness probe (signal-0). A dead pid ⇒ stale residue.
	if !daemon.ProcessAlive(ep.PID) {
		s.log.Info("adoption: worker pid gone; GC stale artifacts", "session", id, "pid", ep.PID)
		removeWorkerArtifacts(id)
		return false
	}

	// 2. Pre-dial wire-version hint from the endpoint file (design §6).
	if !wireVersionCompatible(ep.WireVersion) {
		s.log.Warn("adoption: wire-version skew (endpoint hint); leaving worker unadopted",
			"session", id, "workerWire", ep.WireVersion, "routerWire", daemon.WireVersion)
		return false
	}

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
	helloCtx, cancel := context.WithTimeout(context.Background(), wireCallTimeout)
	hello, err := client.Hello(helloCtx)
	cancel()
	if err != nil {
		s.log.Warn("adoption: gofer/hello failed; leaving worker unadopted", "session", id, "err", err)
		_ = client.Close()
		return false
	}

	// 5. Authoritative wire-version skew ⇒ leave.
	if !wireVersionCompatible(hello.WireVersion) {
		s.log.Warn("adoption: wire-version skew (handshake); leaving worker unadopted",
			"session", id, "workerWire", hello.WireVersion, "routerWire", daemon.WireVersion)
		_ = client.Close()
		return false
	}

	// 6. Adopt. Load FIRST so history + any open permission request re-surface
	//    into the reconstructed broker (retained by event.WithReplay, §7) before
	//    any consumer subscribes; only THEN is the session considered attached.
	//    Referencing via Load (not a first-reference SubscribeLive) also means a
	//    later SubscribeLive on this session won't re-trigger a history replay
	//    onto the live stream (see wirestream's Load/SubscribeLive contract).
	rec := wirestream.New(client)
	loadCtx, cancel := context.WithTimeout(context.Background(), wireCallTimeout)
	if lerr := rec.Load(loadCtx, id); lerr != nil {
		// A load that did not settle is non-fatal — the live stream still works;
		// the session simply starts from whatever events arrive next.
		s.log.Warn("adoption: history load did not settle; adopting live-only", "session", id, "err", lerr)
	}
	cancel()

	h := &workerHandle{id: id, cmd: nil, client: client, rec: rec, pid: ep.PID, wait: adoptedWait(client)}

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
	s.mu.Unlock()
	go s.reap(h)

	s.log.Info("adopted live worker", "session", id, "addr", ep.Addr, "pid", ep.PID, "binaryVersion", hello.BinaryVersion)
	return true
}

// SetPermissionRelay wires the OUTER daemon's permission fan-out into the router
// and starts a STANDING permission watcher for every already-adopted session
// (design §7, F1). It must be called once, after the daemon that hosts this
// router is constructed and BEFORE it serves — the daemon does not exist yet at
// [New] time (it takes this router as its Supervisor), so the bridge is injected
// here rather than in the constructor.
//
// The gap it closes: an adopted session's turn runs on its worker, so no
// [daemon] session/prompt handler is observing its event stream to record
// permission routes and broadcast requests. Without a standing watcher a
// re-surfaced (or newly asked) permission on an adopted session would never
// reach the daemon's route table or its attached clients — invisible and
// unanswerable. Each watcher subscribes to its session's reconstructed broker
// WITH replay (so a still-open request re-surfaced by adoption's
// [wirestream.Reconstructor.Load] is delivered) and drives the relay, so the
// daemon records the call→session route (making handlePermissionReply resolve
// for adopted sessions) and fans the request out exactly as the prompt handler
// would.
//
// Only ADOPTED sessions get a watcher: a session this router CREATES has its
// turns driven by a client's session/prompt, whose handler already performs the
// fan-out. At the one call site (right after daemon construction, before serve)
// every live handle is an adopted one, so iterating the roster here covers
// exactly them. A nil relay, or a closed router, is a no-op.
func (s *Supervisor) SetPermissionRelay(relay daemon.PermissionRelay) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if relay == nil || s.closed {
		return
	}
	s.permRelay = relay
	for _, h := range s.workers {
		s.watcherWG.Add(1)
		go s.watchPermissions(h, relay)
	}
}

// watchPermissions is the standing per-adopted-session permission watcher
// SetPermissionRelay starts. It subscribes to h's reconstructed broker WITH
// replay and, for every permission event, drives the relay so the outer daemon's
// route table and client fan-out track the worker's live gate state. It exits
// when the broker closes (the handle was reaped, or the router shut down and
// closed every rec) or reaperStop fires — so it never outlives its session and
// is joined by [Supervisor.Close] via watcherWG.
//
// Using the replaying Subscribe is deliberate: a request re-surfaced by
// adoption's Load is retained on the broker, and Subscribe replays it so the
// watcher relays it even though it was published before the watcher existed. A
// retained request that ALSO has a retained resolution replays as
// requested-then-resolved, netting to a no-op (recorded then cleared) — only a
// still-OPEN request stays routed, which is exactly the mid-approval state §7
// must survive.
func (s *Supervisor) watchPermissions(h *workerHandle, relay daemon.PermissionRelay) {
	defer s.watcherWG.Done()
	sub, err := h.rec.Subscribe(context.Background(), h.id)
	if err != nil {
		return // reconstructor already closed (handle reaped before the watcher started)
	}
	defer sub.Close()
	for {
		select {
		case ev, ok := <-sub.C:
			if !ok {
				return // broker closed: session gone or router shutting down
			}
			switch pe := ev.(type) {
			case event.PermissionRequested:
				// Idempotent under duplicate delivery: even if the replay re-emits
				// a request a concurrent prompt handler also observed, the relay's
				// route-first / pending-presence gates broadcast it exactly once,
				// and a client answers by call id regardless — a double delivery is
				// harmless.
				relay.RequestPermission(h.id, pe)
			case event.PermissionResolved:
				relay.ResolvePermission(h.id, pe)
			}
		case <-s.reaperStop:
			return
		}
	}
}

// wireVersionCompatible reports whether the router can adopt a worker
// advertising router↔worker wire version workerWire. This slice supports only
// the router's OWN wire version ([daemon.WireVersion]) — N-0 full; the
// observe/reply/finish subset across a version gap is Phase 3 (design §6) — so
// an equal version adopts and any skew is left for a future skew-aware router.
func wireVersionCompatible(workerWire int) bool {
	return workerWire == daemon.WireVersion
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
