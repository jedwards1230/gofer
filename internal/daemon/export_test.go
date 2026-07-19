package daemon

// PeersForSessionCount reports how many peers are currently attached to
// sessionID in the fan-out registry. It is a test-only accessor — the daemon's
// tests live in the external package daemon_test and cannot reach
// [Daemon.sessionPeers] directly — used to assert attach-on-load/detach-on-close
// bookkeeping (see the fan-out tests).
func (d *Daemon) PeersForSessionCount(sessionID string) int {
	return len(d.peersForSession(sessionID))
}

// BeginPromptHandler / EndPromptHandler / PromptHandlerActive expose the M6
// event relay's DOUBLE-DELIVERY GUARD to the external test package. The guard is
// otherwise only observable through a real in-flight session/prompt, which makes
// the interesting case — a relayed broadcast arriving WHILE a prompt handler is
// running — awkward to hit deterministically. Exposing the marker lets a test
// drive the guard's two states directly and assert delivery/suppression with no
// sleeps. See [Daemon.BroadcastRawEvent]'s guard doc.
func (d *Daemon) BeginPromptHandler(sessionID string) { d.beginPromptHandler(sessionID) }

// EndPromptHandler releases one [Daemon.BeginPromptHandler] mark.
func (d *Daemon) EndPromptHandler(sessionID string) { d.endPromptHandler(sessionID) }

// PromptHandlerActive reports whether the relay is currently standing down for
// sessionID.
func (d *Daemon) PromptHandlerActive(sessionID string) bool {
	return d.promptHandlerActive(sessionID)
}

// OutstandingPermissionRequestCount reports how many permission call ids still
// have a live session/request_permission fan-out (their cancel func has not yet
// fired). It is a test-only accessor used to assert that resolving a permission
// by any path retracts the outstanding requests at every other peer, leaving no
// daemon-side waiter dangling.
func (d *Daemon) OutstandingPermissionRequestCount() int {
	d.permReqMu.Lock()
	defer d.permReqMu.Unlock()
	return len(d.permReqCancels)
}
