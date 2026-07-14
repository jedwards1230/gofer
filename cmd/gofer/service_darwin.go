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

// unload boots the agent out of the GUI domain. An already-unloaded label is
// tolerated so uninstall stays idempotent.
func (m launchdManager) unload(ctx context.Context, path string) error {
	domain := "gui/" + strconv.Itoa(os.Getuid())
	target := domain + "/" + m.label()
	// bootout by service target; ignore "not loaded" so uninstall is idempotent.
	_ = runQuiet(ctx, "launchctl", "bootout", target)
	return nil
}

// reloadAfterRemove is a no-op on launchd: bootout in unload already dropped the
// job from the domain, so removing the plist file needs no follow-up reload
// (the systemd path is the only one that must forget a deleted unit).
func (launchdManager) reloadAfterRemove(_ context.Context) error { return nil }

// running reports whether launchctl knows the label in the GUI domain.
func (m launchdManager) running(ctx context.Context) (bool, error) {
	domain := "gui/" + strconv.Itoa(os.Getuid())
	target := domain + "/" + m.label()
	err := runQuiet(ctx, "launchctl", "print", target)
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
