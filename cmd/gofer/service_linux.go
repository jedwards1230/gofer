//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// systemdUnitName is the systemd user unit name for the gofer daemon.
const systemdUnitName = "gofer.service"

// systemdManager drives a systemd --user unit under
// ~/.config/systemd/user/gofer.service.
type systemdManager struct{}

func activeServiceManager() serviceManager { return systemdManager{} }

func (systemdManager) label() string { return systemdUnitName }

func (systemdManager) unitPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "systemd", "user", systemdUnitName), nil
}

func (systemdManager) render(cfg serviceConfig) []byte { return renderSystemdUnit(cfg) }

func (m systemdManager) isInstalled() (bool, error) {
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

// load reloads systemd's user manager to pick up the freshly written unit, then
// enables + starts it so it runs now and on every login.
func (systemdManager) load(ctx context.Context, _ string) error {
	if err := runQuiet(ctx, "systemctl", "--user", "daemon-reload"); err != nil {
		return err
	}
	return runQuiet(ctx, "systemctl", "--user", "enable", "--now", systemdUnitName)
}

// unload disables + stops the unit, then reloads the manager. The command layer
// removes the unit file (see runDaemonUninstall); a subsequent daemon-reload
// here lets systemd forget the removed unit cleanly. Both steps tolerate an
// already-disabled unit so uninstall stays idempotent.
func (systemdManager) unload(ctx context.Context, _ string) error {
	_ = runQuiet(ctx, "systemctl", "--user", "disable", "--now", systemdUnitName)
	return nil
}

// running reports whether `systemctl --user is-active` says the unit is active.
func (systemdManager) running(ctx context.Context) (bool, error) {
	err := runQuiet(ctx, "systemctl", "--user", "is-active", systemdUnitName)
	if err == nil {
		return true, nil
	}
	// is-active exits non-zero for an inactive/unknown unit — that is "not
	// running", not a tooling failure.
	var exitErr *exec.ExitError
	if asExit(err, &exitErr) {
		return false, nil
	}
	return false, err
}
