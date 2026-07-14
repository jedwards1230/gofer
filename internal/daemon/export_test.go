package daemon

// PeersForSessionCount reports how many peers are currently attached to
// sessionID in the fan-out registry. It is a test-only accessor — the daemon's
// tests live in the external package daemon_test and cannot reach
// [Daemon.sessionPeers] directly — used to assert attach-on-load/detach-on-close
// bookkeeping (see the fan-out tests).
func (d *Daemon) PeersForSessionCount(sessionID string) int {
	return len(d.peersForSession(sessionID))
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
