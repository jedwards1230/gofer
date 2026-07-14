// Package sandbox provides gofer's OS-specific containment backends for the
// SDK's permission guard. A [Container] both answers the SDK's
// [loop.Container] decision predicate (can this call be contained on this
// host?) and supplies the contained-exec primitive gofer wraps the bash tool
// with, so an allow-matched call the sandbox can hold runs inside it rather
// than escalating to a human.
//
// The concrete backends are seatbelt (macOS `sandbox-exec`) and bwrap+seccomp
// (Linux), selected by build tag with a no-op fallback on every other host.
// The SDK owns only the [loop.Container] interface — never add a backend to the
// SDK (see loop/guard.go).
package sandbox

import (
	"context"

	"github.com/jedwards1230/agent-sdk-go/loop"
)

// Container is gofer's sandbox backend: the SDK [loop.Container] decision
// predicate (CanContain) plus the contained-exec primitive used to wrap the
// bash tool. The exported surface of this package is a stable contract other
// packages (supervisor wiring, daemon) build against — keep it frozen.
type Container interface {
	loop.Container // CanContain(ctx context.Context, call loop.ToolCall) (bool, error)

	// Available reports whether this host has a usable sandbox runtime (the
	// binary is present and detection succeeded). A false here means every
	// otherwise-containable call escalates to a human (fail-closed to ask).
	Available() bool

	// WrapCommand returns the argv that runs command contained in workdir
	// (e.g. `sandbox-exec -p <profile> /bin/sh -c <command>` or a bwrap
	// invocation), and ok=false when this host cannot contain it — in which
	// case the caller MUST NOT execute the command uncontained.
	WrapCommand(command, workdir string) (argv []string, ok bool)
}

// New returns the OS-appropriate [Container], detecting the sandbox runtime at
// construction. On an unsupported host (or when the runtime binary is missing)
// it returns a backend whose CanContain is always false, so the guard asks a
// human for every call rather than running one uncontained.
func New() Container { return newContainer() }

// ToolTarget extracts the permission-specifier match string from a tool call:
// the shell command for bash, the file path for the file tools
// (read/write/edit/ls/glob/grep), and "" for anything else. The SDK's
// [permission.Engine] matches a rule's Specifier against this string, so it is
// also what a remember-grant pins.
func ToolTarget(call loop.ToolCall) string { return toolTarget(call) }

// WrapRegistry is implemented in registry.go (it needs no OS-specific
// wiring — it only calls the [Container] interface — so it lives in its own
// non-tagged file alongside the contained-bash tool it builds).

// noopContainer is the fallback backend: it can contain nothing, so every call
// the guard would otherwise allow escalates to a human. The real seatbelt and
// bwrap backends live in container_darwin.go / container_linux.go.
type noopContainer struct{}

func (noopContainer) CanContain(context.Context, loop.ToolCall) (bool, error) { return false, nil }
func (noopContainer) Available() bool                                         { return false }
func (noopContainer) WrapCommand(string, string) ([]string, bool)             { return nil, false }
