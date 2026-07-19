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

// unload disables + stops the unit (`disable --now`), reporting whether
// anything was actually running to stop. The command layer then removes the
// unit file and calls reloadAfterRemove, so the full uninstall sequence is:
// disable --now → remove file → daemon-reload. An already-disabled/absent unit
// is tolerated so uninstall stays idempotent, but a disable that leaves the
// unit still active is a propagated error rather than a silent success.
func (m systemdManager) unload(ctx context.Context, _ string) (bool, error) {
	wasActive, err := m.running(ctx)
	if err != nil {
		return false, err
	}
	if err := runQuiet(ctx, "systemctl", "--user", "disable", "--now", systemdUnitName); err != nil {
		// disable exits non-zero for an unknown/never-enabled unit, which is a
		// clean idempotent no-op — the unit still being active is what makes it
		// a real failure.
		if stillActive, rerr := m.running(ctx); rerr != nil || stillActive {
			return false, err
		}
		return wasActive, nil
	}
	if wasActive {
		if err := waitServiceStopped(ctx, m.running); err != nil {
			return false, err
		}
	}
	return wasActive, nil
}

// stopService stops the unit without disabling it, so a service-managed daemon
// stopped by `gofer daemon stop` still comes back at the next login. systemd's
// Restart=on-failure would respawn a bare-SIGTERM'd daemon, so driving
// systemctl is what makes the stop stick.
func (m systemdManager) stopService(ctx context.Context, _ string) (bool, error) {
	active, err := m.running(ctx)
	if err != nil {
		return false, err
	}
	if !active {
		return false, nil
	}
	if err := runQuiet(ctx, "systemctl", "--user", "stop", systemdUnitName); err != nil {
		if stillActive, rerr := m.running(ctx); rerr == nil && !stillActive {
			return true, nil
		}
		return false, err
	}
	if err := waitServiceStopped(ctx, m.running); err != nil {
		return false, err
	}
	return true, nil
}

// startService starts the already-installed unit — the start half of
// `gofer daemon restart` for a service-managed daemon.
func (systemdManager) startService(ctx context.Context, _ string) error {
	return runQuiet(ctx, "systemctl", "--user", "start", systemdUnitName)
}

// reloadAfterRemove runs `systemctl --user daemon-reload` so systemd forgets the
// unit file the command layer has just deleted. Best-effort/idempotent: a reload
// with no matching unit left is a clean no-op, so a transient failure never
// blocks an otherwise-complete uninstall.
func (systemdManager) reloadAfterRemove(ctx context.Context) error {
	_ = runQuiet(ctx, "systemctl", "--user", "daemon-reload")
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
