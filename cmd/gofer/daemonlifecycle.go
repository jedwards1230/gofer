package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// daemonStopTimeout bounds the wait for a SIGTERM'd daemon to actually exit.
// The daemon's own graceful shutdown closes the listener and drains in-flight
// ACP work, so this is sized to outlast a normal drain rather than to be
// snappy; a daemon still alive at the end of it is wedged, not busy.
const daemonStopTimeout = 10 * time.Second

// daemonKillGrace bounds the second wait, after SIGKILL. SIGKILL is not
// catchable, so this only covers the kernel tearing the process down; anything
// longer than this means the pid is stuck in an uninterruptible state and no
// amount of further waiting will help.
const daemonKillGrace = 2 * time.Second

// daemonStopPoll is the liveness-probe interval inside both waits. Unlike the
// service-manager probe (which forks a tool), this is a bare signal-0 syscall,
// so it can afford to be tight and keep `stop` feeling immediate.
const daemonStopPoll = 20 * time.Millisecond

// daemonStartTimeout bounds how long `restart` waits for the replacement daemon
// to advertise a fresh endpoint file. A daemon that fails at startup (bad
// model, bound port, missing credential) never writes one, so this timeout is
// the failure signal — its message points at the daemon log the child's stdio
// was redirected to.
const daemonStartTimeout = 15 * time.Second

// daemonStartPoll is the interval between endpoint-file reads while waiting for
// a replacement daemon to come up.
const daemonStartPoll = 50 * time.Millisecond

// daemonProcess is the seam over OS process control that `stop`/`restart` use.
// It exists so the lifecycle logic — signal, wait, escalate to SIGKILL, give up
// — is testable without a test ever signalling a real gofer daemon: a test
// injects a fake that models a process which ignores SIGTERM, or dies on the
// third probe, and no live process is touched at all. The default
// implementation is the real one (osDaemonProcess).
type daemonProcess interface {
	// alive reports whether pid names a running process.
	alive(pid int) bool
	// signal delivers sig to pid. A pid that has already exited is reported as
	// os.ErrProcessDone (or syscall.ESRCH), which callers treat as success.
	signal(pid int, sig syscall.Signal) error
}

// osDaemonProcess is the real [daemonProcess]: signal-0 liveness (pidAlive) and
// a genuine signal delivery.
type osDaemonProcess struct{}

func (osDaemonProcess) alive(pid int) bool { return pidAlive(pid) }

func (osDaemonProcess) signal(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return fmt.Errorf("refusing to signal pid %d", pid)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(sig)
}

// newDaemonProcess is the seam tests swap to inject a fake process controller.
// Mirrors newServiceManager's pattern in service.go.
var newDaemonProcess = func() daemonProcess { return osDaemonProcess{} }

// daemonStartSpec describes the replacement daemon `restart` spawns for a
// daemon that is NOT service-managed: the store root it serves, the address it
// binds, and the bearer token it needs. All three are recovered from the
// endpoint file the stopped daemon wrote, so a restart preserves the discovery
// contract clients already hold.
//
// It deliberately carries nothing else. A manually started daemon's other flags
// (--model, --workers, --log-level) are not recorded anywhere on disk and
// cannot be recovered; a restart therefore reverts them to their defaults. A
// service-managed daemon has no such gap — its unit file carries the full argv,
// which is why restart drives the service manager for that case instead of
// spawning.
type daemonStartSpec struct {
	root   string
	listen string
	// token is a bearer token; it is passed to the child through the
	// environment, never argv (which `ps` exposes), and is never printed.
	token string
}

// startDaemonProcess is the seam tests swap to avoid spawning a real daemon.
// The default spawns a detached `gofer daemon` child.
var startDaemonProcess = spawnDetachedDaemon

// spawnDetachedDaemon starts a replacement `gofer daemon` as a detached child
// (new session, no controlling terminal) so it outlives this short-lived CLI
// process, with its stdio appended to <root>/logs/daemon.out.log. It returns
// the child's pid.
//
// The child is reaped in the background: until something waits on it, a child
// that dies immediately (a bad model, a bound port) lingers as a zombie, and a
// zombie answers signal-0 — so the caller's liveness check would report a
// crashed daemon as running. Reaping keeps that check honest.
func spawnDetachedDaemon(_ context.Context, spec daemonStartSpec) (int, error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("resolve gofer executable: %w", err)
	}
	resolvedRoot, err := supervisor.ResolveRoot(spec.root)
	if err != nil {
		return 0, err
	}
	logDir := filepath.Join(resolvedRoot, "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return 0, fmt.Errorf("create daemon logs directory: %w", err)
	}

	args := []string{"daemon", "--root", resolvedRoot}
	if spec.listen != "" {
		args = append(args, "--listen", spec.listen)
	}
	// exec.Command, NOT exec.CommandContext: binding the child's lifetime to
	// this CLI invocation's context would kill the daemon we just started the
	// moment `gofer daemon restart` returns.
	cmd := exec.Command(exe, args...)
	cmd.Env = os.Environ()
	if spec.token != "" {
		// Environment, never argv: the daemon reads $GOFER_TOKEN (see runDaemon),
		// and argv is world-visible through `ps`.
		cmd.Env = append(cmd.Env, "GOFER_TOKEN="+spec.token)
	}

	pid, err := daemon.SpawnDetached(cmd, filepath.Join(logDir, "daemon.out.log"))
	if err != nil {
		return 0, err
	}
	reaped := daemon.Reap(cmd)
	go func() { <-reaped }()
	return pid, nil
}

// stopOutcome describes what stopDaemon did, so the calling command can report
// it precisely instead of a blanket "ok".
type stopOutcome struct {
	// stopped is true only when a live daemon process was actually terminated.
	stopped bool
	// pid is the endpoint file's recorded pid, set whenever a file was found
	// (whether it named a live daemon or a stale one).
	pid int
	// addr is the stopped daemon's listen address, for the report and for the
	// replacement `restart` starts.
	addr string
	// token is the stopped daemon's bearer token, carried so restart can hand it
	// to the replacement. Never printed, never logged.
	token string
	// staleCleaned is true when no daemon was running but a stale endpoint file
	// naming a dead pid was found and removed.
	staleCleaned bool
	// serviceManaged is true when the stop went through the launchd/systemd
	// service manager because a unit was installed and loaded. It drives both
	// the user-facing note and restart's choice of start mechanism.
	serviceManaged bool
}

// stopDaemon stops the daemon advertised at root and returns what it did.
//
// The order matters. When a service unit is installed and loaded, the service
// manager is driven FIRST: launchd's KeepAlive=true and systemd's
// Restart=on-failure both respawn a daemon that merely received a SIGTERM, so
// signalling first would produce a `stop` that reports success and leaves a
// daemon running under a new pid. Only once the supervisor has been told to
// stand down is the recorded pid signalled — and only if it is still alive,
// since the service manager has usually already reaped it.
//
// A stale endpoint file (dead pid) is not an error: it is the residue of a
// crash, so it is removed and reported as "nothing was running" rather than
// signalling a pid the OS may since have reused for an unrelated process.
func stopDaemon(ctx context.Context, root string) (stopOutcome, error) {
	ep, err := daemon.ReadEndpoint(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// No daemon has advertised here. Nothing to stop — and, critically,
			// nothing to start either (the defect this command exists to fix).
			return stopOutcome{}, nil
		}
		return stopOutcome{}, err
	}

	proc := newDaemonProcess()
	if !proc.alive(ep.PID) {
		if rerr := removeOwnEndpoint(root, ep.PID); rerr != nil {
			return stopOutcome{}, rerr
		}
		return stopOutcome{pid: ep.PID, addr: ep.Addr, staleCleaned: true}, nil
	}

	out := stopOutcome{pid: ep.PID, addr: ep.Addr, token: ep.Token}

	mgr := newServiceManager()
	installed, err := mgr.isInstalled()
	if err != nil {
		return stopOutcome{}, err
	}
	if installed {
		path, perr := mgr.unitPath()
		if perr != nil {
			return stopOutcome{}, perr
		}
		stopped, serr := mgr.stopService(ctx, path)
		if serr != nil {
			return stopOutcome{}, fmt.Errorf("stop service %s: %w", mgr.label(), serr)
		}
		out.serviceManaged = stopped
	}

	// The service manager usually took the process down already; signal only
	// what is still standing. This also covers the mixed case: a service is
	// installed, but the daemon advertised at THIS root was started by hand.
	if proc.alive(ep.PID) {
		if err := terminateDaemonPID(ctx, proc, ep.PID, daemonStopTimeout, daemonKillGrace); err != nil {
			return stopOutcome{}, err
		}
	}

	// Guarded on ownership (removeOwnEndpoint) so a daemon that has since taken
	// over this root's advertisement keeps it.
	if err := removeOwnEndpoint(root, ep.PID); err != nil {
		return stopOutcome{}, err
	}
	out.stopped = true
	return out, nil
}

// terminateDaemonPID signals pid and does not return until the process is
// verifiably gone (or the escalation is exhausted). SIGTERM first — the daemon
// installs an interrupt handler and shuts down gracefully — then SIGKILL after
// termGrace, then a hard error naming the pid. It never reports success on a
// process it has not observed exit; a fire-and-forget signal is exactly the
// silent no-op this change exists to remove.
//
// The two grace periods are parameters rather than the consts read directly so
// the escalation ladder is unit-testable in milliseconds against an injected
// [daemonProcess] — a test for "SIGTERM was ignored, so SIGKILL followed"
// should not have to wait out the production 10s.
func terminateDaemonPID(ctx context.Context, proc daemonProcess, pid int, termGrace, killGrace time.Duration) error {
	if err := proc.signal(pid, syscall.SIGTERM); err != nil && !processGone(err) {
		return fmt.Errorf("signal daemon pid %d: %w", pid, err)
	}
	if waitProcessExit(ctx, proc, pid, termGrace) {
		return nil
	}
	if err := proc.signal(pid, syscall.SIGKILL); err != nil && !processGone(err) {
		return fmt.Errorf("force-kill daemon pid %d after it ignored SIGTERM for %s: %w", pid, termGrace, err)
	}
	if waitProcessExit(ctx, proc, pid, killGrace) {
		return nil
	}
	return fmt.Errorf("daemon pid %d is still running after SIGTERM (%s) and SIGKILL (%s)", pid, termGrace, killGrace)
}

// processGone reports whether a signal error means the target had already
// exited — which, for a stop, is success rather than failure.
func processGone(err error) bool {
	return errors.Is(err, os.ErrProcessDone) || errors.Is(err, syscall.ESRCH)
}

// waitProcessExit polls pid's liveness until it is gone or timeout elapses,
// reporting whether it exited. A cancelled ctx aborts the wait but still
// answers with a final liveness probe, so a cancelled stop reports the truth
// about the process rather than a guess.
func waitProcessExit(ctx context.Context, proc daemonProcess, pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !proc.alive(pid) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return !proc.alive(pid)
		case <-time.After(daemonStopPoll):
		}
	}
}

// removeStaleEndpoint removes root's endpoint file if — and only if — it names
// a pid that is no longer running, reporting whether it removed one. A live
// daemon's advertisement is never touched.
func removeStaleEndpoint(root string) (bool, error) {
	ep, err := daemon.ReadEndpoint(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		// An unreadable or corrupt file is as good as stale: it advertises
		// nothing usable, so removing it is strictly an improvement.
		if rerr := daemon.RemoveEndpoint(root); rerr != nil {
			return false, rerr
		}
		return true, nil
	}
	if newDaemonProcess().alive(ep.PID) {
		return false, nil
	}
	if err := removeOwnEndpoint(root, ep.PID); err != nil {
		return false, err
	}
	return true, nil
}

// runDaemonStop implements `gofer daemon stop [--root dir]`: it resolves the
// running daemon from root's endpoint file, stops it (driving the service
// manager first when one owns it), waits for the process to actually exit, and
// removes the now-stale endpoint file.
//
// With no daemon running it SAYS SO and starts nothing — the whole point of the
// sub-verb existing, since `stop` previously fell through runDaemon's switch to
// the foreground-serve path and tried to start one.
func runDaemonStop(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newDaemonServiceFlagSet("daemon stop", stderr)
	root := fs.String("root", "", "session store root whose daemon to stop (default ~/.gofer)")
	if help, err := parseFlags(fs, args); err != nil {
		return err
	} else if help {
		return nil
	}

	out, err := stopDaemon(ctx, *root)
	if err != nil {
		return err
	}
	reportStop(out, stdout)
	return nil
}

// reportStop prints stopDaemon's outcome in the operator's terms.
func reportStop(out stopOutcome, stdout io.Writer) {
	switch {
	case out.stopped:
		_, _ = fmt.Fprintf(stdout, "Stopped gofer daemon (pid %d) at %s.\n", out.pid, out.addr)
		if out.serviceManaged {
			_, _ = fmt.Fprintln(stdout, "Its service was stopped too, so it will not respawn; it starts again at the next login (or run `gofer daemon restart`).")
		}
	case out.staleCleaned:
		_, _ = fmt.Fprintf(stdout, "No gofer daemon is running (removed a stale endpoint file naming pid %d).\n", out.pid)
	default:
		_, _ = fmt.Fprintln(stdout, "No gofer daemon is running.")
	}
}

// runDaemonRestart implements `gofer daemon restart [--root dir]`: stop, then
// start, then confirm the replacement advertised itself.
//
// There is no window in which the endpoint file names a dead pid: stopDaemon
// removes the file as part of stopping (and removes a stale one outright), so
// between the two halves there is simply no advertisement, and the next one a
// client can read is the new daemon's own. A client that reads in the gap falls
// back to its normal no-endpoint discovery rather than dialling a corpse.
//
// The start half mirrors how the daemon was being run: a service-managed daemon
// is restarted through its service manager, so it comes back with the unit's
// full argv; a hand-started one is respawned with the root/listen/token
// recovered from its endpoint file (see daemonStartSpec for what that cannot
// recover).
func runDaemonRestart(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newDaemonServiceFlagSet("daemon restart", stderr)
	root := fs.String("root", "", "session store root whose daemon to restart (default ~/.gofer)")
	if help, err := parseFlags(fs, args); err != nil {
		return err
	} else if help {
		return nil
	}

	out, err := stopDaemon(ctx, *root)
	if err != nil {
		return err
	}
	switch {
	case out.stopped:
		_, _ = fmt.Fprintf(stdout, "Stopped gofer daemon (pid %d) at %s.\n", out.pid, out.addr)
	case out.staleCleaned:
		_, _ = fmt.Fprintf(stdout, "No gofer daemon was running (cleared a stale endpoint file naming pid %d); starting one.\n", out.pid)
	default:
		_, _ = fmt.Fprintln(stdout, "No gofer daemon was running; starting one.")
	}

	if out.serviceManaged {
		mgr := newServiceManager()
		path, perr := mgr.unitPath()
		if perr != nil {
			return perr
		}
		if err := mgr.startService(ctx, path); err != nil {
			return fmt.Errorf("start service %s: %w", mgr.label(), err)
		}
	} else if _, err := startDaemonProcess(ctx, daemonStartSpec{
		root:   *root,
		listen: out.addr,
		token:  out.token,
	}); err != nil {
		return err
	}

	// The spawned pid is not the confirmation — the endpoint file is. A daemon
	// that starts and then fails a startup check (model, port, credential) never
	// writes one, and reporting the pid alone would call that a successful
	// restart. Waiting for a fresh advertisement is what makes "exactly one
	// daemon running, endpoint pointing at the NEW pid" an assertion rather than
	// a hope.
	newPID, err := waitEndpointLive(ctx, *root, out.pid, daemonStartTimeout)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "Started gofer daemon (pid %d).\n", newPID)
	return nil
}

// waitEndpointLive polls root's endpoint file until it names a live pid that is
// not prevPID, returning that pid. Excluding prevPID is what makes this a
// restart check rather than a "was anything ever here" check: a leftover file
// from the daemon we just stopped must never be mistaken for the replacement's
// advertisement.
func waitEndpointLive(ctx context.Context, root string, prevPID int, timeout time.Duration) (int, error) {
	proc := newDaemonProcess()
	deadline := time.Now().Add(timeout)
	for {
		ep, err := daemon.ReadEndpoint(root)
		if err == nil && ep.PID != prevPID && proc.alive(ep.PID) {
			return ep.PID, nil
		}
		if !time.Now().Before(deadline) {
			return 0, fmt.Errorf("the restarted daemon did not advertise an endpoint within %s — check the daemon log under the store root's logs/ directory", timeout)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(daemonStartPoll):
		}
	}
}
