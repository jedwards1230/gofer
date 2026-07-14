//go:build darwin

package sandbox

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// TestSeatbeltContainer_Smoke drives real commands through the production
// contained-bash path (containedBash.Run -> Container.WrapCommand ->
// sandbox-exec -> /bin/sh -c) to prove the seatbelt profile actually permits
// ordinary shell startup, pipes, workdir file I/O, and system-temp writes —
// not just that the generated SBPL text contains the right substrings.
func TestSeatbeltContainer_Smoke(t *testing.T) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not available")
	}

	c := newSeatbeltContainer(exec.LookPath)
	if !c.Available() {
		t.Skip("seatbelt container not available")
	}

	workdir := t.TempDir()
	b := &containedBash{container: c, workdir: workdir}

	tests := []struct {
		name       string
		command    string
		wantSubstr string
	}{
		{name: "date", command: "date"},
		{name: "ls", command: "ls"},
		{name: "pipe", command: "echo hi | cat", wantSubstr: "hi"},
		{
			name:       "workdir tempfile write+read",
			command:    "printf 'smoke-body' > out.txt && cat out.txt",
			wantSubstr: "smoke-body",
		},
		{name: "mktemp", command: "mktemp"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			input, err := json.Marshal(containedBashInput{Command: tt.command})
			if err != nil {
				t.Fatalf("marshal input: %v", err)
			}

			res, err := b.Run(context.Background(), json.RawMessage(input))
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if res.IsError {
				t.Fatalf("Run() IsError = true, content:\n%s", res.Content)
			}
			if strings.Contains(res.Content, "Operation not permitted") {
				t.Fatalf("Run() content contains %q:\n%s", "Operation not permitted", res.Content)
			}
			if strings.Contains(res.Content, "/private/var/select") {
				t.Fatalf("Run() content contains %q:\n%s", "/private/var/select", res.Content)
			}
			if tt.wantSubstr != "" && !strings.Contains(res.Content, tt.wantSubstr) {
				t.Fatalf("Run() content missing %q:\n%s", tt.wantSubstr, res.Content)
			}
		})
	}

	// Confinement guard: a write to the shared, world-writable /tmp must be
	// DENIED. /private/tmp is deliberately kept out of the profile's write set
	// (only the per-user /private/var/folders is granted), so a contained tool
	// cannot drop files other processes on the host can see. This locks in that
	// the temp grant stays scoped to per-user scratch — re-adding /private/tmp
	// write would flip this to a success and fail the test.
	t.Run("shared /tmp write denied", func(t *testing.T) {
		input, err := json.Marshal(containedBashInput{
			Command: "echo confinement-probe > /tmp/gofer-confinement-probe",
		})
		if err != nil {
			t.Fatalf("marshal input: %v", err)
		}
		res, err := b.Run(context.Background(), json.RawMessage(input))
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
		if !res.IsError {
			t.Fatalf("write to shared /tmp unexpectedly succeeded — confinement hole:\n%s", res.Content)
		}
	})
}
