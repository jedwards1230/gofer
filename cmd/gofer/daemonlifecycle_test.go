package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// NOTE ON SAFETY: nothing in this file may signal, stop, or attach to the
// developer's own running gofer daemon. Every test here works against a
// t.TempDir() store root (never the default ~/.gofer), and every pid it
// signals belongs to a sleeper process the test itself spawned. Service-manager
// interaction goes through the newServiceManager fake — no launchctl or
// systemctl is ever executed. hermeticDaemonEnv additionally points
// $GOFER_DAEMON at a closed port so no discovery can wander onto a live daemon.

// spawnTestSleeper starts a long-lived child process this test owns and returns
// its pid — a stand-in for "a running daemon" that is safe to signal because
// the test created it.
//
// It reaps the child in the background. A signalled child that is never waited
// on becomes a zombie, and a zombie still answers the signal-0 liveness probe
// as ALIVE — so without this the stop path would wait out its full timeout on a
// process that is really dead, and the test would look like a hang.
func spawnTestSleeper(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "600")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start test sleeper: %v", err)
	}
	waited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-waited
	})
	return cmd.Process.Pid
}

// trackServeForeground swaps the serveForeground seam for a recorder and
// returns a func reporting how many times the foreground-serve path was
// entered. The replacement returns an error rather than nil so a test that
// accidentally relies on serve "succeeding" is loud about it.
func trackServeForeground(t *testing.T) func() int {
	t.Helper()
	prev := serveForeground
	var mu sync.Mutex
	calls := 0
	serveForeground = func(context.Context, []string, io.Writer, io.Writer) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		return errors.New("foreground serve path entered")
	}
	t.Cleanup(func() { serveForeground = prev })
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return calls
	}
}

// notInstalledManager returns a fake service manager whose unit file does not
// exist, i.e. "no launchd/systemd service owns this daemon" — the hand-started
// case.
func notInstalledManager(t *testing.T) *fakeServiceManager {
	t.Helper()
	return &fakeServiceManager{path: filepath.Join(t.TempDir(), "gofer.service")}
}

// TestRunDaemonStopStopsRunningDaemon covers the headline case: a daemon
// advertised at root is terminated, the success report names its pid and
// address, and the endpoint file it left behind is cleaned up so no client
// discovers a dead pid afterwards.
func TestRunDaemonStopStopsRunningDaemon(t *testing.T) {
	hermeticDaemonEnv(t)
	root := t.TempDir()
	pid := spawnTestSleeper(t)
	withFakeManager(t, notInstalledManager(t))

	const addr = "127.0.0.1:65535"
	if err := daemon.WriteEndpoint(root, daemon.Endpoint{Addr: addr, PID: pid}); err != nil {
		t.Fatalf("WriteEndpoint: %v", err)
	}

	var out, errBuf bytes.Buffer
	if err := runDaemonStop(context.Background(), []string{"--root", root}, &out, &errBuf); err != nil {
		t.Fatalf("runDaemonStop: %v (stderr=%q)", err, errBuf.String())
	}

	if pidAlive(pid) {
		t.Errorf("daemon pid %d is still alive after `daemon stop`", pid)
	}
	if !strings.Contains(out.String(), "Stopped gofer daemon") {
		t.Errorf("stdout = %q, want it to report the daemon was stopped", out.String())
	}
	if !strings.Contains(out.String(), fmt.Sprintf("pid %d", pid)) || !strings.Contains(out.String(), addr) {
		t.Errorf("stdout = %q, want it to name pid %d and %s", out.String(), pid, addr)
	}
	if _, err := daemon.ReadEndpoint(root); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("endpoint file after stop: err = %v, want os.ErrNotExist (the stale advertisement must be removed)", err)
	}
}

// TestRunDaemonStopWithNoDaemonStartsNothing is the regression test for the
// reported defect: `gofer daemon stop` with nothing running used to fall
// through to foreground serve and answer "a gofer daemon is already running —
// stop it first". It must instead say plainly that none is running, exit 0, and
// — asserted directly through the serveForeground seam — never enter serve.
func TestRunDaemonStopWithNoDaemonStartsNothing(t *testing.T) {
	hermeticDaemonEnv(t)
	root := t.TempDir()
	withFakeManager(t, notInstalledManager(t))
	serveCalls := trackServeForeground(t)

	var out, errBuf bytes.Buffer
	code := run([]string{"daemon", "stop", "--root", root}, strings.NewReader(""), &out, &errBuf)

	// Serve-entry first: it is the load-bearing assertion. The exit code alone
	// cannot tell the fix from the defect, since the old fall-through also
	// exited non-zero (via the already-running guard).
	if got := serveCalls(); got != 0 {
		t.Fatalf("`daemon stop` entered the foreground serve path %d times, want 0 — stop must never start a daemon", got)
	}
	if code != 0 {
		t.Fatalf("run(daemon stop, none running) = %d, want 0\nstdout: %s\nstderr: %s", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "No gofer daemon is running") {
		t.Errorf("stdout = %q, want it to say no daemon is running", out.String())
	}
	if strings.Contains(errBuf.String(), "already running") {
		t.Errorf("stderr = %q, want no already-running error (that was the reported defect)", errBuf.String())
	}
}

// TestRunDaemonStopClearsStaleEndpoint covers the crash-residue case: the
// endpoint file names a pid that is gone. Stop must not signal that pid (the OS
// may have reused it for an unrelated process), must report that nothing was
// running, and must clear the file.
func TestRunDaemonStopClearsStaleEndpoint(t *testing.T) {
	hermeticDaemonEnv(t)
	root := t.TempDir()
	withFakeManager(t, notInstalledManager(t))
	stale := deadPid(t)
	if err := daemon.WriteEndpoint(root, daemon.Endpoint{Addr: "127.0.0.1:65535", PID: stale}); err != nil {
		t.Fatalf("WriteEndpoint: %v", err)
	}

	var out, errBuf bytes.Buffer
	if err := runDaemonStop(context.Background(), []string{"--root", root}, &out, &errBuf); err != nil {
		t.Fatalf("runDaemonStop: %v", err)
	}
	if !strings.Contains(out.String(), "No gofer daemon is running") || !strings.Contains(out.String(), "stale") {
		t.Errorf("stdout = %q, want it to report no daemon and a removed stale endpoint file", out.String())
	}
	if _, err := daemon.ReadEndpoint(root); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale endpoint file survived stop: err = %v, want os.ErrNotExist", err)
	}
}

// TestRunDaemonStopDrivesServiceManager asserts the design decision for a
// service-managed daemon: the launchd/systemd service is stopped through its
// manager BEFORE the pid is signalled. A bare SIGTERM to a KeepAlive/Restart=
// supervised process is respawned within moments, so a stop that only signalled
// would report success and leave a daemon running under a new pid.
func TestRunDaemonStopDrivesServiceManager(t *testing.T) {
	hermeticDaemonEnv(t)
	root := t.TempDir()
	pid := spawnTestSleeper(t)

	unitPath := filepath.Join(t.TempDir(), "gofer.service")
	if err := os.WriteFile(unitPath, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("seed unit file: %v", err)
	}
	fake := &fakeServiceManager{path: unitPath, isRunning: true, stopDidWork: true}
	withFakeManager(t, fake)

	if err := daemon.WriteEndpoint(root, daemon.Endpoint{Addr: "127.0.0.1:65535", PID: pid}); err != nil {
		t.Fatalf("WriteEndpoint: %v", err)
	}

	var out, errBuf bytes.Buffer
	if err := runDaemonStop(context.Background(), []string{"--root", root}, &out, &errBuf); err != nil {
		t.Fatalf("runDaemonStop: %v", err)
	}

	if fake.stopCalls != 1 {
		t.Errorf("stopService called %d times, want 1 — a service-managed daemon must be stopped through its service manager or it respawns", fake.stopCalls)
	}
	if !strings.Contains(out.String(), "will not respawn") {
		t.Errorf("stdout = %q, want it to tell the operator the service was stopped too", out.String())
	}
	if pidAlive(pid) {
		t.Errorf("pid %d still alive: the service manager did not reap it, so stop must still signal it", pid)
	}
}

// TestRunDaemonStopSkipsServiceManagerWhenNotInstalled is the counterpart: with
// no unit file installed, stop must not touch the service manager at all.
func TestRunDaemonStopSkipsServiceManagerWhenNotInstalled(t *testing.T) {
	hermeticDaemonEnv(t)
	root := t.TempDir()
	pid := spawnTestSleeper(t)
	fake := notInstalledManager(t)
	withFakeManager(t, fake)
	if err := daemon.WriteEndpoint(root, daemon.Endpoint{Addr: "127.0.0.1:65535", PID: pid}); err != nil {
		t.Fatalf("WriteEndpoint: %v", err)
	}

	var out, errBuf bytes.Buffer
	if err := runDaemonStop(context.Background(), []string{"--root", root}, &out, &errBuf); err != nil {
		t.Fatalf("runDaemonStop: %v", err)
	}
	if fake.stopCalls != 0 {
		t.Errorf("stopService called %d times with no unit installed, want 0", fake.stopCalls)
	}
	if strings.Contains(out.String(), "will not respawn") {
		t.Errorf("stdout = %q, want no service note for a hand-started daemon", out.String())
	}
}

// TestRunDaemonRestartLeavesOneDaemonAtNewPID covers restart end to end: the old
// daemon is stopped, exactly one replacement is started, and the endpoint file
// ends up naming the NEW pid. It also asserts the no-dead-pid-window property
// from inside the start seam: at the instant the replacement is launched, the
// endpoint file must be absent rather than still advertising the corpse.
func TestRunDaemonRestartLeavesOneDaemonAtNewPID(t *testing.T) {
	hermeticDaemonEnv(t)
	root := t.TempDir()
	oldPID := spawnTestSleeper(t)
	withFakeManager(t, notInstalledManager(t))

	const addr = "127.0.0.1:65535"
	const token = "restart-token"
	if err := daemon.WriteEndpoint(root, daemon.Endpoint{Addr: addr, PID: oldPID, Token: token}); err != nil {
		t.Fatalf("WriteEndpoint: %v", err)
	}

	var (
		newPID       int
		gotSpec      daemonStartSpec
		startCalls   int
		endpointGone bool
	)
	prevStart := startDaemonProcess
	startDaemonProcess = func(_ context.Context, spec daemonStartSpec) (int, error) {
		startCalls++
		gotSpec = spec
		// The window assertion: between stop and start there must be NO endpoint
		// file naming the pid we just killed.
		if _, err := daemon.ReadEndpoint(root); errors.Is(err, os.ErrNotExist) {
			endpointGone = true
		}
		// Stand in for a real daemon coming up: a live process of our own, plus
		// the same self-advertisement runDaemon performs at startup.
		newPID = spawnTestSleeper(t)
		if err := daemon.WriteEndpoint(root, daemon.Endpoint{Addr: spec.listen, PID: newPID, Token: spec.token}); err != nil {
			return 0, err
		}
		return newPID, nil
	}
	t.Cleanup(func() { startDaemonProcess = prevStart })

	var out, errBuf bytes.Buffer
	if err := runDaemonRestart(context.Background(), []string{"--root", root}, &out, &errBuf); err != nil {
		t.Fatalf("runDaemonRestart: %v (stderr=%q)", err, errBuf.String())
	}

	if startCalls != 1 {
		t.Errorf("restart started %d daemons, want exactly 1", startCalls)
	}
	if !endpointGone {
		t.Error("the endpoint file still existed when the replacement was started — restart must leave no window where it names a dead pid")
	}
	if pidAlive(oldPID) {
		t.Errorf("restart left the old daemon (pid %d) running", oldPID)
	}
	if !pidAlive(newPID) {
		t.Errorf("the replacement daemon (pid %d) is not running", newPID)
	}
	ep, err := daemon.ReadEndpoint(root)
	if err != nil {
		t.Fatalf("ReadEndpoint after restart: %v", err)
	}
	if ep.PID != newPID {
		t.Errorf("endpoint file names pid %d, want the replacement's %d", ep.PID, newPID)
	}
	// The replacement must inherit the discovery contract clients already hold.
	if gotSpec.listen != addr {
		t.Errorf("replacement listen = %q, want the stopped daemon's %q", gotSpec.listen, addr)
	}
	if gotSpec.token != token {
		t.Error("replacement did not inherit the stopped daemon's bearer token")
	}
	// The token must never be printed.
	if strings.Contains(out.String(), token) || strings.Contains(errBuf.String(), token) {
		t.Errorf("restart leaked the bearer token to output: stdout=%q stderr=%q", out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), fmt.Sprintf("Started gofer daemon (pid %d)", newPID)) {
		t.Errorf("stdout = %q, want it to report the new pid", out.String())
	}
}

// TestRunDaemonRestartWithNoneRunningStartsOne asserts restart is not a no-op
// when nothing is running: it starts one and says so.
func TestRunDaemonRestartWithNoneRunningStartsOne(t *testing.T) {
	hermeticDaemonEnv(t)
	root := t.TempDir()
	withFakeManager(t, notInstalledManager(t))

	prevStart := startDaemonProcess
	startCalls := 0
	startDaemonProcess = func(_ context.Context, spec daemonStartSpec) (int, error) {
		startCalls++
		pid := spawnTestSleeper(t)
		return pid, daemon.WriteEndpoint(root, daemon.Endpoint{Addr: spec.listen, PID: pid})
	}
	t.Cleanup(func() { startDaemonProcess = prevStart })

	var out, errBuf bytes.Buffer
	if err := runDaemonRestart(context.Background(), []string{"--root", root}, &out, &errBuf); err != nil {
		t.Fatalf("runDaemonRestart: %v (stderr=%q)", err, errBuf.String())
	}
	if startCalls != 1 {
		t.Errorf("restart started %d daemons, want 1", startCalls)
	}
	if !strings.Contains(out.String(), "No gofer daemon was running; starting one.") {
		t.Errorf("stdout = %q, want it to say none was running", out.String())
	}
}

// TestRunDaemonRestartUsesServiceManagerWhenServiceManaged asserts restart's
// start half mirrors how the daemon was being run: a service-managed daemon
// comes back through its service manager (inheriting the unit's full argv),
// never through a hand-rolled spawn that would silently drop --model.
func TestRunDaemonRestartUsesServiceManagerWhenServiceManaged(t *testing.T) {
	hermeticDaemonEnv(t)
	root := t.TempDir()
	oldPID := spawnTestSleeper(t)

	unitPath := filepath.Join(t.TempDir(), "gofer.service")
	if err := os.WriteFile(unitPath, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("seed unit file: %v", err)
	}
	fake := &fakeServiceManager{path: unitPath, isRunning: true, stopDidWork: true}
	withFakeManager(t, fake)
	if err := daemon.WriteEndpoint(root, daemon.Endpoint{Addr: "127.0.0.1:65535", PID: oldPID}); err != nil {
		t.Fatalf("WriteEndpoint: %v", err)
	}

	spawned := 0
	prevStart := startDaemonProcess
	startDaemonProcess = func(context.Context, daemonStartSpec) (int, error) {
		spawned++
		return 0, errors.New("a service-managed restart must not spawn a daemon directly")
	}
	t.Cleanup(func() { startDaemonProcess = prevStart })

	// The "service" comes back asynchronously, exactly as launchd/systemd would:
	// advertise a fresh endpoint shortly after startService is called.
	newPID := spawnTestSleeper(t)
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = daemon.WriteEndpoint(root, daemon.Endpoint{Addr: "127.0.0.1:65535", PID: newPID})
	}()

	var out, errBuf bytes.Buffer
	if err := runDaemonRestart(context.Background(), []string{"--root", root}, &out, &errBuf); err != nil {
		t.Fatalf("runDaemonRestart: %v (stderr=%q)", err, errBuf.String())
	}
	if fake.startCalls != 1 {
		t.Errorf("startService called %d times, want 1", fake.startCalls)
	}
	if spawned != 0 {
		t.Errorf("restart spawned a daemon directly %d times for a service-managed daemon, want 0", spawned)
	}
}

// TestDaemonUnknownSubVerbNeverReachesServe is the acceptance test for the
// default arm. It asserts through the serveForeground seam that an unrecognized
// positional starts NOTHING — an exit-code check alone could not distinguish the
// fix from the bug, since the old fall-through also exited non-zero (via the
// already-running guard).
func TestDaemonUnknownSubVerbNeverReachesServe(t *testing.T) {
	for _, verb := range []string{"staus", "stopp", "bogus", "start"} {
		t.Run(verb, func(t *testing.T) {
			hermeticDaemonEnv(t)
			serveCalls := trackServeForeground(t)

			var out, errBuf bytes.Buffer
			code := run([]string{"daemon", verb}, strings.NewReader(""), &out, &errBuf)

			if got := serveCalls(); got != 0 {
				t.Fatalf("`gofer daemon %s` entered the foreground serve path %d times, want 0 — an unknown sub-verb must start nothing", verb, got)
			}
			if code != 2 {
				t.Errorf("run(daemon %s) = %d, want 2 (usage error)\nstderr: %s", verb, code, errBuf.String())
			}
			if !strings.Contains(errBuf.String(), "unknown sub-verb") {
				t.Errorf("stderr = %q, want it to name the unknown sub-verb", errBuf.String())
			}
			for _, valid := range []string{"install", "uninstall", "status", "stop", "restart"} {
				if !strings.Contains(errBuf.String(), valid) {
					t.Errorf("stderr = %q, want it to list the valid sub-verb %q", errBuf.String(), valid)
				}
			}
		})
	}
}

// TestDaemonFlagArgsStillReachServe guards the other side of the default arm:
// rejecting unknown positionals must not reject serve FLAGS. `gofer daemon
// --listen ...` and a bare `gofer daemon` still go to foreground serve.
func TestDaemonFlagArgsStillReachServe(t *testing.T) {
	for name, args := range map[string][]string{
		"bare":        {"daemon"},
		"listen flag": {"daemon", "--listen", "127.0.0.1:65535"},
		"serve alias": {"serve", "--log-level", "debug"},
	} {
		t.Run(name, func(t *testing.T) {
			hermeticDaemonEnv(t)
			serveCalls := trackServeForeground(t)

			var out, errBuf bytes.Buffer
			run(args, strings.NewReader(""), &out, &errBuf)

			if got := serveCalls(); got != 1 {
				t.Errorf("run(%v) entered the foreground serve path %d times, want 1", args, got)
			}
		})
	}
}

// fakeDaemonProcess models a process for the escalation ladder without any real
// process existing: it reports alive until the configured signal arrives (and,
// for diesOnTerm == false, ignores SIGTERM entirely, which is what forces the
// SIGKILL escalation).
type fakeDaemonProcess struct {
	mu          sync.Mutex
	dead        bool
	diesOnTerm  bool
	diesOnKill  bool
	signals     []syscall.Signal
	signalErr   error
	neverDiesAt bool // when true, even SIGKILL leaves it alive (the wedged case)
}

func (f *fakeDaemonProcess) alive(int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.dead
}

func (f *fakeDaemonProcess) signal(_ int, sig syscall.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, sig)
	if f.signalErr != nil {
		return f.signalErr
	}
	if f.neverDiesAt {
		return nil
	}
	if (sig == syscall.SIGTERM && f.diesOnTerm) || (sig == syscall.SIGKILL && f.diesOnKill) {
		f.dead = true
	}
	return nil
}

func (f *fakeDaemonProcess) sent() []syscall.Signal {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]syscall.Signal(nil), f.signals...)
}

// TestTerminateDaemonPIDEscalation covers the escalation ladder against an
// injected process: a graceful daemon gets only SIGTERM; one that ignores
// SIGTERM is escalated to SIGKILL; one that survives both is a hard error rather
// than a false success.
func TestTerminateDaemonPIDEscalation(t *testing.T) {
	const (
		termGrace = 30 * time.Millisecond
		killGrace = 30 * time.Millisecond
	)
	tests := []struct {
		name    string
		proc    *fakeDaemonProcess
		wantSig []syscall.Signal
		wantErr bool
	}{
		{
			name:    "exits on SIGTERM",
			proc:    &fakeDaemonProcess{diesOnTerm: true, diesOnKill: true},
			wantSig: []syscall.Signal{syscall.SIGTERM},
		},
		{
			name:    "ignores SIGTERM, escalates to SIGKILL",
			proc:    &fakeDaemonProcess{diesOnKill: true},
			wantSig: []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL},
		},
		{
			name:    "survives both: a hard error, never a false success",
			proc:    &fakeDaemonProcess{neverDiesAt: true},
			wantSig: []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := terminateDaemonPID(context.Background(), tt.proc, 4242, termGrace, killGrace)
			if tt.wantErr {
				if err == nil {
					t.Fatal("terminateDaemonPID = nil, want an error naming the still-running pid")
				}
				if !strings.Contains(err.Error(), "4242") {
					t.Errorf("err = %v, want it to name the pid", err)
				}
			} else if err != nil {
				t.Fatalf("terminateDaemonPID: %v", err)
			}
			got := tt.proc.sent()
			if len(got) != len(tt.wantSig) {
				t.Fatalf("signals sent = %v, want %v", got, tt.wantSig)
			}
			for i := range got {
				if got[i] != tt.wantSig[i] {
					t.Fatalf("signals sent = %v, want %v", got, tt.wantSig)
				}
			}
		})
	}
}

// TestTerminateDaemonPIDAlreadyGone asserts a signal error meaning "the process
// already exited" is treated as success, not failure: that is the outcome stop
// wanted.
func TestTerminateDaemonPIDAlreadyGone(t *testing.T) {
	proc := &fakeDaemonProcess{dead: true, signalErr: os.ErrProcessDone}
	if err := terminateDaemonPID(context.Background(), proc, 4242, time.Millisecond, time.Millisecond); err != nil {
		t.Errorf("terminateDaemonPID on an already-exited process: %v, want nil", err)
	}
}

// TestStopDaemonNeverSignalsAStalePID asserts stop does not signal a dead pid at
// all — the OS may have reused it for an unrelated process, so signalling it
// would be dangerous rather than merely useless.
func TestStopDaemonNeverSignalsAStalePID(t *testing.T) {
	hermeticDaemonEnv(t)
	root := t.TempDir()
	withFakeManager(t, notInstalledManager(t))
	if err := daemon.WriteEndpoint(root, daemon.Endpoint{Addr: "127.0.0.1:65535", PID: deadPid(t)}); err != nil {
		t.Fatalf("WriteEndpoint: %v", err)
	}

	proc := &fakeDaemonProcess{dead: true}
	prev := newDaemonProcess
	newDaemonProcess = func() daemonProcess { return proc }
	t.Cleanup(func() { newDaemonProcess = prev })

	out, err := stopDaemon(context.Background(), root)
	if err != nil {
		t.Fatalf("stopDaemon: %v", err)
	}
	if out.stopped {
		t.Error("stopDaemon reported stopping a daemon that was already dead")
	}
	if !out.staleCleaned {
		t.Error("stopDaemon did not report clearing the stale endpoint file")
	}
	if got := proc.sent(); len(got) != 0 {
		t.Errorf("stopDaemon signalled a stale pid (%v); it must never signal a pid the OS may have reused", got)
	}
}

// TestRunDaemonUninstallReportsNothingUnloaded is the uninstall half of the fix:
// with the unit file present but nothing actually loaded (the manually started
// daemon case, and the case launchd's discarded bootout error used to hide), the
// command must NOT print "Uninstalled".
func TestRunDaemonUninstallReportsNothingUnloaded(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "gofer.service")
	if err := os.WriteFile(path, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("seed unit file: %v", err)
	}
	fake := &fakeServiceManager{path: path, unloadDidWork: false}
	withFakeManager(t, fake)

	var out, errBuf bytes.Buffer
	if err := runDaemonUninstall(context.Background(), nil, &out, &errBuf); err != nil {
		t.Fatalf("runDaemonUninstall: %v", err)
	}
	if strings.Contains(out.String(), "Uninstalled") {
		t.Errorf("stdout = %q, want no success claim when nothing was unloaded", out.String())
	}
	if !strings.Contains(out.String(), "nothing was loaded") {
		t.Errorf("stdout = %q, want it to say nothing was loaded", out.String())
	}
}

// TestRunDaemonUninstallPropagatesUnloadError asserts a failed unload is a
// command error rather than a discarded one followed by a success message —
// the launchd `_ = runQuiet(..., "bootout", ...)` defect.
func TestRunDaemonUninstallPropagatesUnloadError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "gofer.service")
	if err := os.WriteFile(path, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("seed unit file: %v", err)
	}
	fake := &fakeServiceManager{path: path, unloadErr: errors.New("bootout: Operation not permitted")}
	withFakeManager(t, fake)

	var out, errBuf bytes.Buffer
	err := runDaemonUninstall(context.Background(), nil, &out, &errBuf)
	if err == nil {
		t.Fatal("runDaemonUninstall = nil, want the unload error propagated")
	}
	if !strings.Contains(err.Error(), "Operation not permitted") {
		t.Errorf("err = %v, want it to carry the unload failure", err)
	}
	if strings.Contains(out.String(), "Uninstalled") {
		t.Errorf("stdout = %q, want no success message after a failed unload", out.String())
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("a failed unload must leave the unit file in place: %v", statErr)
	}
}

// TestRunDaemonUninstallClearsStaleEndpoint asserts uninstall cleans up the
// endpoint file the unloaded daemon left behind, so clients stop discovering a
// dead pid after the service is gone.
func TestRunDaemonUninstallClearsStaleEndpoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "gofer.service")
	if err := os.WriteFile(path, []byte("[Unit]\n"), 0o644); err != nil {
		t.Fatalf("seed unit file: %v", err)
	}
	withFakeManager(t, &fakeServiceManager{path: path, unloadDidWork: true})
	if err := daemon.WriteEndpoint(root, daemon.Endpoint{Addr: "127.0.0.1:65535", PID: deadPid(t)}); err != nil {
		t.Fatalf("WriteEndpoint: %v", err)
	}

	var out, errBuf bytes.Buffer
	if err := runDaemonUninstall(context.Background(), []string{"--root", root}, &out, &errBuf); err != nil {
		t.Fatalf("runDaemonUninstall: %v", err)
	}
	if !strings.Contains(out.String(), "Uninstalled") {
		t.Errorf("stdout = %q, want the success message when a loaded service was unloaded", out.String())
	}
	if _, err := daemon.ReadEndpoint(root); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("uninstall left a stale endpoint file behind: err = %v, want os.ErrNotExist", err)
	}
}

// TestWaitEndpointLiveIgnoresThePreviousDaemon pins the guard that makes
// restart's confirmation a real check: the PREVIOUS daemon's still-present,
// still-live advertisement must never be mistaken for the replacement's. Without
// the prevPID exclusion, a restart whose replacement never came up would report
// success by reading the old file back.
func TestWaitEndpointLiveIgnoresThePreviousDaemon(t *testing.T) {
	root := t.TempDir()
	prevPID := spawnTestSleeper(t)
	if err := daemon.WriteEndpoint(root, daemon.Endpoint{Addr: "127.0.0.1:65535", PID: prevPID}); err != nil {
		t.Fatalf("WriteEndpoint: %v", err)
	}

	// Only the old daemon is advertised: this must NOT be accepted.
	if got, err := waitEndpointLive(context.Background(), root, prevPID, 50*time.Millisecond); err == nil {
		t.Fatalf("waitEndpointLive accepted the previous daemon's endpoint (pid %d); it must wait for a fresh one", got)
	}

	// Now the replacement advertises itself: that one IS accepted.
	newPID := spawnTestSleeper(t)
	if err := daemon.WriteEndpoint(root, daemon.Endpoint{Addr: "127.0.0.1:65535", PID: newPID}); err != nil {
		t.Fatalf("WriteEndpoint: %v", err)
	}
	got, err := waitEndpointLive(context.Background(), root, prevPID, time.Second)
	if err != nil {
		t.Fatalf("waitEndpointLive: %v", err)
	}
	if got != newPID {
		t.Errorf("waitEndpointLive = %d, want the replacement's pid %d", got, newPID)
	}
}

// TestWaitEndpointLiveRejectsADeadAdvertisement asserts an endpoint file naming
// a pid that is not running is not accepted as a live replacement — a daemon
// that wrote its endpoint and then died is a failed restart, not a success.
func TestWaitEndpointLiveRejectsADeadAdvertisement(t *testing.T) {
	root := t.TempDir()
	if err := daemon.WriteEndpoint(root, daemon.Endpoint{Addr: "127.0.0.1:65535", PID: deadPid(t)}); err != nil {
		t.Fatalf("WriteEndpoint: %v", err)
	}
	if got, err := waitEndpointLive(context.Background(), root, 1, 50*time.Millisecond); err == nil {
		t.Fatalf("waitEndpointLive accepted a dead pid (%d) as the restarted daemon", got)
	}
}

// TestRemoveStaleEndpointSparesALiveDaemon asserts the cleanup is guarded on
// liveness: a still-running daemon's advertisement is never removed.
func TestRemoveStaleEndpointSparesALiveDaemon(t *testing.T) {
	root := t.TempDir()
	pid := spawnTestSleeper(t)
	if err := daemon.WriteEndpoint(root, daemon.Endpoint{Addr: "127.0.0.1:65535", PID: pid}); err != nil {
		t.Fatalf("WriteEndpoint: %v", err)
	}
	removed, err := removeStaleEndpoint(root)
	if err != nil {
		t.Fatalf("removeStaleEndpoint: %v", err)
	}
	if removed {
		t.Error("removeStaleEndpoint removed a LIVE daemon's endpoint file")
	}
	if _, err := daemon.ReadEndpoint(root); err != nil {
		t.Errorf("live endpoint file was removed: %v", err)
	}
}
