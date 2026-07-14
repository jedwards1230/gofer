//go:build !darwin && !linux

package main

import (
	"context"
	"fmt"
	"runtime"
)

// unsupportedServiceManager is the manager on platforms with no launchd/systemd
// integration: every action returns a clean "not supported on <GOOS>" error so
// the build stays green everywhere while the feature is a no-op off macOS/Linux.
type unsupportedServiceManager struct{}

func activeServiceManager() serviceManager { return unsupportedServiceManager{} }

func unsupportedErr() error {
	return fmt.Errorf("daemon service install is not supported on %s", runtime.GOOS)
}

func (unsupportedServiceManager) label() string { return "gofer" }

func (unsupportedServiceManager) unitPath() (string, error) { return "", unsupportedErr() }

func (unsupportedServiceManager) render(_ serviceConfig) []byte { return nil }

func (unsupportedServiceManager) isInstalled() (bool, error) { return false, nil }

func (unsupportedServiceManager) load(_ context.Context, _ string) error { return unsupportedErr() }

func (unsupportedServiceManager) unload(_ context.Context, _ string) error { return unsupportedErr() }

func (unsupportedServiceManager) reloadAfterRemove(_ context.Context) error { return nil }

func (unsupportedServiceManager) running(_ context.Context) (bool, error) { return false, nil }
