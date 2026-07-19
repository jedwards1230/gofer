//go:build unix

package daemon

import (
	"os"
	"os/exec"
	"testing"
)

// deadPid returns a pid guaranteed not to be alive: it runs a real short-lived
// child to completion and returns its pid, which the OS will not reuse within
// this test's lifetime in practice. Mirrors cmd/gofer's deadPid helper — a
// deterministic "not running" pid without a sleep or a hardcoded large number
// that might coincidentally be live.
func deadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start /bin/sh: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait /bin/sh: %v", err)
	}
	return cmd.Process.Pid
}

// TestProcessAlive covers ProcessAlive's branches: the current process (alive),
// invalid pids (dead), a just-exited child (dead via ESRCH), and pid 1
// (alive via the nil-or-EPERM branch).
func TestProcessAlive(t *testing.T) {
	if !ProcessAlive(os.Getpid()) {
		t.Error("ProcessAlive(os.Getpid()) = false, want true")
	}
	if ProcessAlive(0) {
		t.Error("ProcessAlive(0) = true, want false")
	}
	if ProcessAlive(-1) {
		t.Error("ProcessAlive(-1) = true, want false")
	}
	if ProcessAlive(deadPid(t)) {
		t.Error("ProcessAlive(deadPid) = true, want false")
	}
	// pid 1 (init/launchd) always exists. Under an unprivileged runner,
	// signal 0 to pid 1 returns EPERM (exists, not ours to signal); under
	// root it returns nil. Either way ProcessAlive must report alive —
	// this exercises the "alive via nil-OR-EPERM" branch specifically.
	if !ProcessAlive(1) {
		t.Error("ProcessAlive(1) = false, want true (pid 1 always exists)")
	}
}
