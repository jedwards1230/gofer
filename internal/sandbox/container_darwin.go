//go:build darwin

package sandbox

import (
	"context"
	"os/exec"

	"github.com/jedwards1230/agent-sdk-go/loop"
)

// seatbeltContainer is the macOS sandbox backend: `sandbox-exec` with a
// generated SBPL profile (profile_seatbelt.go) that denies everything by
// default, allows read of the base system, allows read+write only inside the
// session workdir, and denies network access outright.
type seatbeltContainer struct {
	available bool
}

// newContainer detects sandbox-exec on PATH and returns the seatbelt backend.
func newContainer() Container { return newSeatbeltContainer(exec.LookPath) }

// newSeatbeltContainer builds a seatbeltContainer using lookPath to detect
// the sandbox-exec runtime. lookPath is injected (rather than calling
// exec.LookPath directly) so detection is testable without depending on the
// host's real PATH.
func newSeatbeltContainer(lookPath func(string) (string, error)) *seatbeltContainer {
	_, err := lookPath("sandbox-exec")
	return &seatbeltContainer{available: err == nil}
}

// Available implements Container.
func (c *seatbeltContainer) Available() bool { return c.available }

// CanContain implements loop.Container: true once sandbox-exec is present
// and the call is one of the containable builtins (see capability.go).
func (c *seatbeltContainer) CanContain(_ context.Context, call loop.ToolCall) (bool, error) {
	return c.available && containableTool(call.Name), nil
}

// WrapCommand implements Container.
func (c *seatbeltContainer) WrapCommand(command, workdir string) ([]string, bool) {
	if !c.available {
		return nil, false
	}
	profile := seatbeltProfile(workdir)
	return []string{"sandbox-exec", "-p", profile, "/bin/sh", "-c", command}, true
}
