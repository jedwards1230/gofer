//go:build linux

package sandbox

import (
	"context"
	"os/exec"

	"github.com/jedwards1230/agent-sdk-go/loop"
)

// bwrapContainer is the Linux sandbox backend: `bwrap` (bubblewrap) with a
// generated argv (profile_bwrap.go) that bind-mounts the root filesystem
// read-only except the session workdir (read+write) and unshares the
// network namespace so the contained process has no network access.
type bwrapContainer struct {
	available bool
}

// newContainer detects bwrap on PATH and returns the bubblewrap backend.
func newContainer() Container { return newBwrapContainer(exec.LookPath) }

// newBwrapContainer builds a bwrapContainer using lookPath to detect the
// bwrap runtime. lookPath is injected (rather than calling exec.LookPath
// directly) so detection is testable without depending on the host's real
// PATH.
func newBwrapContainer(lookPath func(string) (string, error)) *bwrapContainer {
	_, err := lookPath("bwrap")
	return &bwrapContainer{available: err == nil}
}

// Available implements Container.
func (c *bwrapContainer) Available() bool { return c.available }

// CanContain implements loop.Container: true once bwrap is present and the
// call is one of the containable builtins (see capability.go).
func (c *bwrapContainer) CanContain(_ context.Context, call loop.ToolCall) (bool, error) {
	return c.available && containableTool(call.Name), nil
}

// WrapCommand implements Container.
func (c *bwrapContainer) WrapCommand(command, workdir string) ([]string, bool) {
	if !c.available {
		return nil, false
	}
	return bwrapArgv(command, workdir), true
}
