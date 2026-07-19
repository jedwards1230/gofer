//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// serviceStopTimeout bounds how long a stop/unload waits for the service
// manager to report the job actually gone. Both `launchctl bootout` and
// `systemctl stop` return before the process has necessarily finished exiting,
// so returning immediately would let a caller print success — or start a
// replacement — while the old daemon still holds the listen port.
const serviceStopTimeout = 10 * time.Second

// serviceStopPoll is the interval between liveness probes inside that wait.
// Each probe shells out to launchctl/systemctl, so this is deliberately coarse
// enough not to spin on the service manager.
const serviceStopPoll = 100 * time.Millisecond

// waitServiceStopped polls running until it reports the service down, the
// deadline expires, or ctx is cancelled. A probe error aborts the wait rather
// than being retried: if the tool cannot answer "is it running", waiting longer
// will not make the answer appear, and a silent timeout would be a worse
// diagnostic than the tool's own error.
func waitServiceStopped(ctx context.Context, running func(context.Context) (bool, error)) error {
	deadline := time.Now().Add(serviceStopTimeout)
	for {
		active, err := running(ctx)
		if err != nil {
			return err
		}
		if !active {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("service still running %s after the stop request", serviceStopTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(serviceStopPoll):
		}
	}
}

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
