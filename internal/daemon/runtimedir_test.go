package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestWorkersDir(t *testing.T) {
	suffix := filepath.Join("gofer-"+strconv.Itoa(os.Getuid()), "workers")

	t.Run("roots at XDG_RUNTIME_DIR when set", func(t *testing.T) {
		runtime := t.TempDir()
		t.Setenv("XDG_RUNTIME_DIR", runtime)

		dir, err := WorkersDir()
		if err != nil {
			t.Fatalf("WorkersDir: %v", err)
		}
		if want := filepath.Join(runtime, suffix); dir != want {
			t.Errorf("WorkersDir = %q, want %q", dir, want)
		}
	})

	t.Run("falls back to os.TempDir when XDG unset", func(t *testing.T) {
		// Empty (not just unset) exercises the "set but empty" guard too.
		t.Setenv("XDG_RUNTIME_DIR", "")

		dir, err := WorkersDir()
		if err != nil {
			t.Fatalf("WorkersDir: %v", err)
		}
		if want := filepath.Join(os.TempDir(), suffix); dir != want {
			t.Errorf("WorkersDir = %q, want %q", dir, want)
		}
	})
}

func TestWorkerPaths(t *testing.T) {
	// A short, synthetic runtime root: these path helpers are pure string
	// composers (no filesystem access), and a deep real t.TempDir() on macOS
	// (`/var/folders/...`) would trip WorkerSocketPath's length guard — which
	// is the point of the guard, but not what this test is exercising.
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")

	dir, err := WorkersDir()
	if err != nil {
		t.Fatalf("WorkersDir: %v", err)
	}

	const uuid = "sess-abc123"
	// The endpoint and lock carry the full uuid; the socket carries a short
	// hash of it (see WorkerSocketPath — the full uuid would overflow the
	// unix-socket path budget on macOS).
	sum := sha256.Sum256([]byte(uuid))
	sockBase := hex.EncodeToString(sum[:])[:workerSocketHashLen] + ".sock"
	tests := []struct {
		name string
		got  func(string) (string, error)
		base string
	}{
		{"endpoint", WorkerEndpointPath, uuid + ".json"},
		{"socket", WorkerSocketPath, sockBase},
		{"lock", WorkerLockPath, uuid + ".lock"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.got(uuid)
			if err != nil {
				t.Fatalf("%s path: %v", tt.name, err)
			}
			if want := filepath.Join(dir, tt.base); got != want {
				t.Errorf("path = %q, want %q", got, want)
			}
		})
	}
}

// TestWorkerSocketPathWithinMacOSBudget is the regression guard for the macOS
// sun_path overflow: with a real 36-char UUIDv7 under a macOS-representative
// $TMPDIR (`/var/folders/xx/<28>/T`, ~47 bytes — the exact fallback that broke
// the naive `<uuid>.sock` scheme), the composed socket path must stay within
// the 103-byte budget. Simulated via XDG_RUNTIME_DIR so it holds on any host,
// not just macOS.
func TestWorkerSocketPathWithinMacOSBudget(t *testing.T) {
	// A realistic macOS per-user DARWIN_USER_TEMP_DIR shape and length.
	t.Setenv("XDG_RUNTIME_DIR", "/var/folders/q7/8xk3mn5d1qz9b2wr0vfh6yc00000gn/T")
	const uuidv7 = "0190c3a1-7b2e-7cde-89ab-0123456789ab" // 36 chars

	got, err := WorkerSocketPath(uuidv7)
	if err != nil {
		t.Fatalf("WorkerSocketPath: %v", err)
	}
	if len(got) > maxUnixSocketPath {
		t.Errorf("socket path is %d bytes (%q), exceeds the %d-byte budget", len(got), got, maxUnixSocketPath)
	}
	// It must NOT embed the full uuid (that is the overflow the hash avoids),
	// while the endpoint/lock still do.
	if strings.Contains(filepath.Base(got), uuidv7) {
		t.Errorf("socket basename %q unexpectedly contains the full uuid", filepath.Base(got))
	}
	ep, err := WorkerEndpointPath(uuidv7)
	if err != nil {
		t.Fatalf("WorkerEndpointPath: %v", err)
	}
	if !strings.Contains(filepath.Base(ep), uuidv7) {
		t.Errorf("endpoint basename %q should carry the full uuid", filepath.Base(ep))
	}
}

// TestWorkerSocketPathOverBudget asserts the length guard earns its keep: a
// pathologically long runtime root makes WorkerSocketPath return an error
// rather than a socket path that would silently fail bind().
func TestWorkerSocketPathOverBudget(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/"+strings.Repeat("x", maxUnixSocketPath))

	if _, err := WorkerSocketPath("0190c3a1-7b2e-7cde-89ab-0123456789ab"); err == nil {
		t.Error("WorkerSocketPath over budget: err = nil, want an error")
	}
}
