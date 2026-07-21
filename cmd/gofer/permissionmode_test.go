package main

// permissionmode_test.go covers [permissionModeResolver] — the closure every
// supervisor gofer builds reads its guardrail posture through. It is the hop
// that makes a /yolo config write reach a RUNNING process, so what matters is
// (a) it reads the file each call rather than caching, and (b) it fails toward
// asking when it can't.

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/jedwards1230/gofer/internal/config"
)

// writeConfig writes a config.json under root with the given permission mode.
func writeConfig(t *testing.T, root, mode string) {
	t.Helper()
	if err := config.Save(config.DefaultPath(root), config.Config{
		Session: config.Session{PermissionMode: mode},
	}); err != nil {
		t.Fatalf("config.Save: %v", err)
	}
}

func TestPermissionModeResolverReadsTheConfigFile(t *testing.T) {
	root := t.TempDir()
	resolve := permissionModeResolver(root)

	// No config file at all: an unconfigured gofer is contain-or-ask.
	if got := resolve(); got != config.PermissionModeAsk {
		t.Fatalf("with no config.json, resolver = %q, want %q", got, config.PermissionModeAsk)
	}

	writeConfig(t, root, "yolo")
	if got := resolve(); got != config.PermissionModeYolo {
		t.Fatalf("after writing yolo, resolver = %q, want %q", got, config.PermissionModeYolo)
	}

	// The re-read is the whole point: the same closure must observe a LATER
	// write, which is how /yolo reaches a daemon that is already running.
	writeConfig(t, root, "ask")
	if got := resolve(); got != config.PermissionModeAsk {
		t.Fatalf("after toggling back to ask, resolver = %q, want %q — the resolver is caching", got, config.PermissionModeAsk)
	}
}

func TestPermissionModeResolverFailsClosed(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(config.DefaultPath(root), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write malformed config: %v", err)
	}
	if got := permissionModeResolver(root)(); got != config.PermissionModeAsk {
		t.Fatalf("a malformed config resolved to %q, want %q — an unreadable config must never leave guardrails off",
			got, config.PermissionModeAsk)
	}
}

// TestPermissionModeResolverIsWiredToEveryBackend is a source-level guard: a
// supervisor built without the PermissionMode seam silently ignores /yolo, and
// that failure is invisible at runtime (sessions just keep asking, which looks
// like normal operation). gofer builds exactly three supervisors; each one must
// pass the resolver.
// wiredResolver matches the supervisor.Config field assignment, tolerating the
// gofmt key alignment that varies with whatever the LONGEST field name in that
// literal happens to be — a sibling adding a field re-pads every line, and this
// guard must not fail on that.
var wiredResolver = regexp.MustCompile(`PermissionMode:\s+permissionModeResolver\(`)

func TestPermissionModeResolverIsWiredToEveryBackend(t *testing.T) {
	files := []string{"daemon.go", "session_worker.go", "tui_app.go"}
	for _, name := range files {
		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join(".", name))
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			if !wiredResolver.MatchString(string(src)) {
				t.Errorf("%s builds a supervisor without PermissionMode: permissionModeResolver(...) — "+
					"/yolo would be a no-op on that backend", name)
			}
		})
	}
}
