//go:build unix

package daemon

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// reapTimeout guards every channel receive so a stuck child fails the test fast
// rather than blocking the whole suite.
const reapTimeout = 10 * time.Second

// TestSpawnDetachedSetsid proves the child runs in its OWN process group, the
// group-level half of detachment (SysProcAttr{Setsid: true}).
//
// Note this asserts a DISTINCT pgid rather than the literal "signal the
// parent's group, watch the child survive": the test process IS the parent, so
// a group-directed signal to the parent's pgid would kill the test runner
// itself. A pgid distinct from the parent's is the sound, safe proof of the
// same property — a signal aimed at the parent's process group cannot reach a
// child that is no longer in it.
func TestSpawnDetachedSetsid(t *testing.T) {
	// No t.Parallel(): this test reasons about process-group identity.
	logPath := filepath.Join(t.TempDir(), "worker.log")
	cmd := exec.Command("/bin/sh", "-c", "sleep 5")
	pid, err := SpawnDetached(cmd, logPath)
	if err != nil {
		t.Fatalf("SpawnDetached: %v", err)
	}
	// Kill the child directly (not its group) and reap it, regardless of the
	// assertions below.
	defer func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		select {
		case <-Reap(cmd):
		case <-time.After(reapTimeout):
			t.Error("Reap did not return after killing child")
		}
	}()

	childPgid, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("Getpgid(child %d): %v", pid, err)
	}
	parentPgid, err := syscall.Getpgid(os.Getpid())
	if err != nil {
		t.Fatalf("Getpgid(parent %d): %v", os.Getpid(), err)
	}
	if childPgid == parentPgid {
		t.Errorf("child pgid %d == parent pgid %d, want distinct (Setsid did not take)", childPgid, parentPgid)
	}
}

// TestSpawnDetachedLogRedirection asserts stdout/stderr land in logPath and
// that the log file is created mode 0600.
func TestSpawnDetachedLogRedirection(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "worker.log")
	const marker = "SPAWN_MARKER_123"
	cmd := exec.Command("/bin/sh", "-c", "echo "+marker)
	if _, err := SpawnDetached(cmd, logPath); err != nil {
		t.Fatalf("SpawnDetached: %v", err)
	}

	select {
	case err := <-Reap(cmd):
		if err != nil {
			t.Fatalf("child exited with error: %v", err)
		}
	case <-time.After(reapTimeout):
		t.Fatal("Reap did not return before timeout")
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log %s: %v", logPath, err)
	}
	if !strings.Contains(string(data), marker) {
		t.Errorf("log %q does not contain marker %q", data, marker)
	}

	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat log %s: %v", logPath, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("log file mode = %o, want 0600", perm)
	}
}

// TestReap covers both exit paths: a non-zero exit surfaces as a *exec.ExitError
// carrying the code, and a clean exit-0 yields a nil error.
func TestReap(t *testing.T) {
	t.Run("nonzero exit", func(t *testing.T) {
		logPath := filepath.Join(t.TempDir(), "worker.log")
		cmd := exec.Command("/bin/sh", "-c", "exit 3")
		if _, err := SpawnDetached(cmd, logPath); err != nil {
			t.Fatalf("SpawnDetached: %v", err)
		}
		select {
		case err := <-Reap(cmd):
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("Reap error = %v, want *exec.ExitError", err)
			}
			if code := exitErr.ExitCode(); code != 3 {
				t.Errorf("exit code = %d, want 3", code)
			}
		case <-time.After(reapTimeout):
			t.Fatal("Reap did not return before timeout")
		}
	})

	t.Run("clean exit", func(t *testing.T) {
		logPath := filepath.Join(t.TempDir(), "worker.log")
		cmd := exec.Command("/bin/sh", "-c", "exit 0")
		if _, err := SpawnDetached(cmd, logPath); err != nil {
			t.Fatalf("SpawnDetached: %v", err)
		}
		select {
		case err := <-Reap(cmd):
			if err != nil {
				t.Errorf("Reap error = %v, want nil for clean exit", err)
			}
		case <-time.After(reapTimeout):
			t.Fatal("Reap did not return before timeout")
		}
	})
}
