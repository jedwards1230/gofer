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
// TODO: cmd/gofer/daemon.go:pidAlive (line ~622) is an exact duplicate of this
// and should be collapsed onto it. The stated blocker has cleared — the router
// adoption path landed and already consumes ProcessAlive (internal/router/
// adopt.go, methods.go) — so this is actionable now rather than deferred; it
// was only held back to keep the original PR zero-conflict with then-in-flight
// critical-path work. Tracked as jedwards1230/gofer#254.
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
