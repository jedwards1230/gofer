package daemon

// PeersForSessionCount reports how many peers are currently attached to
// sessionID in the fan-out registry. It is a test-only accessor — the daemon's
// tests live in the external package daemon_test and cannot reach
// [Daemon.sessionPeers] directly — used to assert attach-on-load/detach-on-close
// bookkeeping (see the fan-out tests).
func (d *Daemon) PeersForSessionCount(sessionID string) int {
	return len(d.peersForSession(sessionID))
}
