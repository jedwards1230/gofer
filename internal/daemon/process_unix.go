//go:build unix

package daemon

import (
	"errors"
	"os"
	"syscall"
)

// ProcessAlive reports whether a process with the given pid is currently
// running — the signal-0 liveness probe the M6 router's adoption scan uses to
// decide whether a worker's endpoint file is live or stale (design §4). A nil
// error, or EPERM (the process exists, it just is not ours to signal), both
// mean alive; anything else (typically ESRCH / os.ErrProcessDone) means gone.
// Unix-only: os.FindProcess always succeeds on Unix regardless of liveness, so
// signal 0 is the portable probe.
//
// cmd/gofer's daemon lifecycle (daemon.go, daemonlifecycle.go) and the router
// adoption path (internal/router/adopt.go, methods.go) all consume this
// directly; jedwards1230/gofer#254 collapsed cmd/gofer's former duplicate
// (pidAlive) onto it.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
