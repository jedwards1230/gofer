//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// runQuiet runs name with args, honoring ctx, and returns a wrapped error that
// includes the tool's combined output on failure — enough for the operator to
// see why launchctl/systemctl rejected the request without this package having
// to parse tool-specific status text. It never logs, and the daemon token
// never appears in any command this package builds.
func runQuiet(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		trimmed := strings.TrimSpace(string(out))
		if trimmed != "" {
			return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, trimmed)
		}
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// asExit unwraps err to an *exec.ExitError, reporting whether the command ran
// but exited non-zero (as opposed to failing to start at all).
func asExit(err error, target **exec.ExitError) bool {
	return errors.As(err, target)
}
