package mcpconn_test

// dial_leak_test.go is the ONE test in this package that spawns a real OS
// subprocess — deliberately, since a leaked process/fd is exactly what
// -race cannot see (see internal/mcpconn's package doc and the M7 plan's
// gate requirements). Everything else in this package is hermetic
// (fake_server_internal_test.go, an in-memory io.Pipe transport). This test
// spawns `sh`, a real MCP server, but never speaks the protocol to it — it
// only proves [mcpconn.Dial] cleans up the process it started when the
// handshake never completes.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/mcpconn"
)

// TestDial_StdioReapsProcessOnHandshakeTimeout proves mcpconn.Dial never
// leaks the subprocess it spawns: a shell script that writes its own pid
// then hangs (never sending a valid "initialize" response) forces
// Client.Initialize to fail on its connect timeout, and Dial must have
// fully reaped that process by the time it returns the error.
func TestDial_StdioReapsProcessOnHandshakeTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pid-liveness check below is POSIX-specific")
	}

	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	// $$ is the shell's own pid; `sleep 30` never replies to anything on
	// stdin, so the MCP "initialize" round trip can only time out.
	script := "echo $$ > " + pidFile + "; sleep 30"
	srv := config.MCPServer{Name: "hang", Command: "sh", Args: []string{"-c", script}}

	start := time.Now()
	_, err := mcpconn.Dial(context.Background(), srv, 100*time.Millisecond, time.Second)
	if err == nil {
		t.Fatal("Dial: want an error — the fake server never speaks MCP")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Dial took %s to fail, want it bounded near the 100ms connect timeout", elapsed)
	}

	pid := readPIDWithRetry(t, pidFile)

	// Give the OS a brief moment to finish reaping after Dial's own
	// cmd.Wait() (called from Client.Close, invoked by Dial on failure)
	// returns — Wait() returning already means it reaped, but the pid table
	// entry can lag by a hair on a loaded CI box.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if processGone(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("mcpconn.Dial leaked subprocess pid %d (still alive after handshake timeout + cleanup)", pid)
}

func readPIDWithRetry(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil && len(strings.TrimSpace(string(b))) > 0 {
			pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
			if perr == nil {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid file %s was never written", path)
	return 0
}

// processGone reports whether pid no longer identifies a live process, via
// the POSIX kill(pid, 0) idiom (send no signal, just check for ESRCH).
func processGone(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	return proc.Signal(syscall.Signal(0)) != nil
}
