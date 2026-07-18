//go:build unix

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// SpawnDetached starts cmd as a detached process that outlives its parent
// (design §3): a new session + process group with no controlling terminal
// (SysProcAttr{Setsid: true}), so when the router exits the worker reparents to
// init/launchd rather than dying with it. Both stdout and stderr are redirected
// to logPath (opened append, mode 0600, created if absent); stdin is /dev/null.
// It returns the started child's pid.
//
// The caller owns cmd's Path/Args/Dir/Env — SpawnDetached only applies the
// detachment and stdio redirection, never overriding what the caller set. The
// log file is closed in the parent after a successful Start (the child has its
// own inherited fd), so SpawnDetached leaks no descriptor.
//
// The caller MUST pair a successful SpawnDetached with a [Reap] on the same cmd
// while the parent lives: until the child exits and is waited on, a child that
// terminates becomes a zombie held by this (parent) process. Reap is that wait.
func SpawnDetached(cmd *exec.Cmd, logPath string) (int, error) {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, fmt.Errorf("daemon: spawn detached: open log %s: %w", logPath, err)
	}

	cmd.Stdout = f
	cmd.Stderr = f
	// Leave cmd.Stdin nil: os/exec routes a nil Stdin to /dev/null, which is
	// exactly what a detached process (no controlling terminal) wants.
	cmd.Stdin = nil

	// Merge Setsid into any SysProcAttr the caller already set rather than
	// clobbering it; a fresh one is fine when the caller supplied none.
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	} else {
		cmd.SysProcAttr.Setsid = true
	}

	if err := cmd.Start(); err != nil {
		_ = f.Close()
		return 0, fmt.Errorf("daemon: spawn detached: %w", err)
	}
	// The child inherited its own fd for the log; the parent's copy is no
	// longer needed and must not leak.
	_ = f.Close()
	return cmd.Process.Pid, nil
}

// Reap waits for a spawned child to exit and delivers the result on the
// returned channel, so the router can avoid zombies while it lives without
// blocking a goroutine of its own on cmd.Wait (design §3's per-worker reaper).
// The channel is buffered (cap 1): if the router has stopped caring (its own
// shutdown — it simply abandons its workers, which then reparent to init and
// are reaped there), the Wait goroutine still delivers into the buffer and
// exits cleanly rather than leaking blocked. Exactly one value is ever sent:
// cmd.Wait's error (nil on a clean exit-0, a *exec.ExitError otherwise).
//
// Precondition — Reap is the SOLE owner of cmd.Wait: it must be called at most
// once per cmd, on a cmd already started (by [SpawnDetached]), and the caller
// must not call cmd.Wait itself. os/exec forbids a second Wait, so a duplicate
// Reap (or a stray Wait) on the same cmd yields a spurious error — a live trap
// for a future kill-then-reap path that must route the terminated worker
// through this one reaper, not a fresh Wait.
func Reap(cmd *exec.Cmd) <-chan error {
	ch := make(chan error, 1)
	go func() {
		ch <- cmd.Wait()
	}()
	return ch
}
