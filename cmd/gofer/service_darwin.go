//go:build darwin

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// launchdLabel is the launchd job label (reverse-DNS) for the gofer daemon
// user agent.
const launchdLabel = "com.github.jedwards1230.gofer"

// launchdManager drives a launchd user agent under ~/Library/LaunchAgents.
type launchdManager struct{}

func activeServiceManager() serviceManager { return launchdManager{} }

func (launchdManager) label() string { return launchdLabel }

func (launchdManager) unitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

func (launchdManager) render(cfg serviceConfig) []byte { return renderLaunchdPlist(cfg) }

func (m launchdManager) isInstalled() (bool, error) {
	path, err := m.unitPath()
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("stat %s: %w", path, err)
}

// load bootstraps the agent into the current GUI domain so it starts now and on
// every login. A bootout+bootstrap dance is avoided: a fresh install has
// nothing to bootout, and an already-loaded label makes bootstrap a clear
// error the caller surfaces rather than silently masking a stale definition.
func (launchdManager) load(ctx context.Context, path string) error {
	domain := "gui/" + strconv.Itoa(os.Getuid())
	if err := runQuiet(ctx, "launchctl", "bootstrap", domain, path); err != nil {
		return err
	}
	return nil
}

// unload boots the agent out of the GUI domain, reporting whether anything was
// actually unloaded. On launchd, bootout both stops the job and deregisters it,
// so unload and stopService are the same operation (see bootout).
func (m launchdManager) unload(ctx context.Context, _ string) (bool, error) {
	return m.bootout(ctx)
}

// stopService boots the agent out of the GUI domain, which is launchd's only
// way to stop a KeepAlive=true job: `launchctl kill` merely signals it, and the
// job is immediately respawned. The consequence is that the agent stays down
// until it is bootstrapped again — by the next login (RunAtLoad) or by
// `gofer daemon restart`, which re-bootstraps explicitly (startService). The
// plist is left in place either way, so the install survives.
func (m launchdManager) stopService(ctx context.Context, _ string) (bool, error) {
	return m.bootout(ctx)
}

// startService re-bootstraps the already-installed plist — the start half of
// `gofer daemon restart` after stopService booted it out.
func (m launchdManager) startService(ctx context.Context, path string) error {
	return m.load(ctx, path)
}

// bootout is the shared stop path. It probes first so an already-unloaded label
// is a clean (false, nil) rather than a bootout error, then — crucially —
// propagates a genuine bootout failure and WAITS for launchd to actually report
// the job gone. The previous implementation discarded bootout's error and
// returned immediately, which made a failed unload indistinguishable from a
// successful one and let the caller print "Uninstalled" over a still-running
// daemon.
func (m launchdManager) bootout(ctx context.Context) (bool, error) {
	loaded, err := m.running(ctx)
	if err != nil {
		return false, err
	}
	if !loaded {
		return false, nil
	}
	target := m.serviceTarget()
	if err := runQuiet(ctx, "launchctl", "bootout", target); err != nil {
		// The job may have exited between the probe and the bootout, which
		// launchctl reports as an error but is exactly the outcome we wanted.
		// Only a job that is genuinely still loaded makes this a real failure.
		if stillLoaded, rerr := m.running(ctx); rerr == nil && !stillLoaded {
			return true, nil
		}
		return false, err
	}
	if err := waitServiceStopped(ctx, m.running); err != nil {
		return false, err
	}
	return true, nil
}

// reloadAfterRemove is a no-op on launchd: bootout in unload already dropped the
// job from the domain, so removing the plist file needs no follow-up reload
// (the systemd path is the only one that must forget a deleted unit).
func (launchdManager) reloadAfterRemove(_ context.Context) error { return nil }

// serviceTarget is the launchd service target (<domain>/<label>) that bootout
// and print address.
func (m launchdManager) serviceTarget() string {
	return "gui/" + strconv.Itoa(os.Getuid()) + "/" + m.label()
}

// running reports whether launchctl knows the label in the GUI domain.
func (m launchdManager) running(ctx context.Context) (bool, error) {
	err := runQuiet(ctx, "launchctl", "print", m.serviceTarget())
	if err == nil {
		return true, nil
	}
	// `launchctl print` exits non-zero when the label is unknown — that is
	// "not running", not a tooling failure.
	var exitErr *exec.ExitError
	if asExit(err, &exitErr) {
		return false, nil
	}
	return false, err
}
