package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// deadPid returns a pid guaranteed not to be alive: it runs a real
// short-lived child process to completion and returns its pid, which the OS
// will not reuse within this test's lifetime in practice. This is the same
// technique other daemon-adjacent tests in this package use to get a
// deterministic "not running" pid without an arbitrary sleep or a hardcoded
// large number that might coincidentally be live.
func deadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/usr/bin/true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run /usr/bin/true: %v", err)
	}
	return cmd.Process.Pid
}

// TestPidAlive covers pidAlive's two ends: the current test process's own
// pid (definitely alive) and a just-exited child's pid (definitely dead).
func TestPidAlive(t *testing.T) {
	if !pidAlive(os.Getpid()) {
		t.Error("pidAlive(os.Getpid()) = false, want true")
	}
	if pidAlive(deadPid(t)) {
		t.Error("pidAlive(deadPid) = true, want false")
	}
	if pidAlive(0) || pidAlive(-1) {
		t.Error("pidAlive(0 or -1) = true, want false")
	}
}

// TestGuardLiveEndpointNoExistingFile asserts guardLiveEndpoint is a no-op
// (nothing to guard) when no endpoint file exists yet at root.
func TestGuardLiveEndpointNoExistingFile(t *testing.T) {
	root := t.TempDir()
	if err := guardLiveEndpoint(context.Background(), root, "127.0.0.1:7333"); err != nil {
		t.Errorf("guardLiveEndpoint with no file: %v, want nil", err)
	}
}

// TestGuardLiveEndpointStaleDeadPID asserts an endpoint file naming a dead
// pid is treated as stale (no error, and no dial is even attempted — the
// recorded addr here is deliberately garbage to prove that) regardless of
// what listenAddr is passed.
func TestGuardLiveEndpointStaleDeadPID(t *testing.T) {
	root := t.TempDir()
	if err := daemon.WriteEndpoint(root, daemon.Endpoint{Addr: "not-a-real-address", PID: deadPid(t)}); err != nil {
		t.Fatalf("WriteEndpoint: %v", err)
	}
	if err := guardLiveEndpoint(context.Background(), root, "not-a-real-address"); err != nil {
		t.Errorf("guardLiveEndpoint with a dead-pid file: %v, want nil (stale, ignored)", err)
	}
}

// TestGuardLiveEndpointStaleUndialableAddr asserts an endpoint file naming a
// LIVE pid but an address that refuses the dial is also treated as stale
// (e.g. the process is alive but is no longer the daemon, or the daemon
// process died leaving an unrelated pid reused by something else — the dial
// is the authoritative check, pid liveness alone is not enough to call it
// live).
func TestGuardLiveEndpointStaleUndialableAddr(t *testing.T) {
	root := t.TempDir()
	if err := daemon.WriteEndpoint(root, daemon.Endpoint{Addr: "127.0.0.1:1", PID: os.Getpid()}); err != nil {
		t.Fatalf("WriteEndpoint: %v", err)
	}
	if err := guardLiveEndpoint(context.Background(), root, "127.0.0.1:1"); err != nil {
		t.Errorf("guardLiveEndpoint with an undialable addr: %v, want nil (stale, ignored)", err)
	}
}

// TestGuardLiveEndpointLiveSameAddrIsHardError covers the actual guard: a
// live pid (this test process's own) whose endpoint dials successfully at
// the SAME address we're about to bind is a hard "already running" error
// naming both the address and the pid, and is never silently overwritten.
func TestGuardLiveEndpointLiveSameAddrIsHardError(t *testing.T) {
	addr := testDaemon(t, "", fauxProvider)
	root := t.TempDir()
	if err := daemon.WriteEndpoint(root, daemon.Endpoint{Addr: addr, PID: os.Getpid()}); err != nil {
		t.Fatalf("WriteEndpoint: %v", err)
	}

	err := guardLiveEndpoint(context.Background(), root, addr)
	if err == nil {
		t.Fatal("guardLiveEndpoint with a live daemon at the same addr: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "already running") || !strings.Contains(err.Error(), addr) {
		t.Errorf("err = %v, want it to name \"already running\" and the address %q", err, addr)
	}
}

// TestGuardLiveEndpointLiveDifferentAddrIsNotBlocked covers the case where a
// live, dialable daemon is recorded but at a DIFFERENT address than the one
// we're about to bind: guardLiveEndpoint lets startup proceed (a deliberate
// second instance over the same root), rather than blocking on a daemon
// that isn't actually in our way.
func TestGuardLiveEndpointLiveDifferentAddrIsNotBlocked(t *testing.T) {
	addr := testDaemon(t, "", fauxProvider)
	root := t.TempDir()
	if err := daemon.WriteEndpoint(root, daemon.Endpoint{Addr: addr, PID: os.Getpid()}); err != nil {
		t.Fatalf("WriteEndpoint: %v", err)
	}

	if err := guardLiveEndpoint(context.Background(), root, "127.0.0.1:59999"); err != nil {
		t.Errorf("guardLiveEndpoint with a live daemon at a different addr: %v, want nil", err)
	}
}

// TestRemoveOwnEndpointGuarded covers removeOwnEndpoint's ownership check: it
// leaves a file whose recorded pid differs from the caller's untouched, and
// only removes one that still names the caller's own pid — the mechanism
// that keeps a delayed/late shutdown from one process clobbering a later
// daemon that has since taken over the same root's endpoint file.
func TestRemoveOwnEndpointGuarded(t *testing.T) {
	root := t.TempDir()
	otherPID := deadPid(t) // any pid distinct from ours; doesn't need to be dead here, just different
	if err := daemon.WriteEndpoint(root, daemon.Endpoint{Addr: "127.0.0.1:7333", PID: otherPID}); err != nil {
		t.Fatalf("WriteEndpoint: %v", err)
	}

	// A different pid than the file's owner: must NOT remove.
	if err := removeOwnEndpoint(root, os.Getpid()); err != nil {
		t.Fatalf("removeOwnEndpoint (mismatched pid): %v", err)
	}
	if _, err := daemon.ReadEndpoint(root); err != nil {
		t.Fatalf("endpoint file was removed despite a pid mismatch: ReadEndpoint: %v", err)
	}

	// The matching pid: must remove.
	if err := removeOwnEndpoint(root, otherPID); err != nil {
		t.Fatalf("removeOwnEndpoint (matching pid): %v", err)
	}
	if _, err := daemon.ReadEndpoint(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadEndpoint after removeOwnEndpoint: err = %v, want os.ErrNotExist", err)
	}
}

// TestRemoveOwnEndpointMissingFileIsNoop asserts removeOwnEndpoint on a root
// with no endpoint file at all is not an error — the common case for a
// daemon that fails before ever reaching the write.
func TestRemoveOwnEndpointMissingFileIsNoop(t *testing.T) {
	root := t.TempDir()
	if err := removeOwnEndpoint(root, os.Getpid()); err != nil {
		t.Errorf("removeOwnEndpoint on a missing file: %v, want nil", err)
	}
}
