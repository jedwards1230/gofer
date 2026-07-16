package main

import (
	"bytes"
	"os"
	"testing"
)

// TestIsTerminal exercises the real filesystem case a fake io.Writer can't:
// a regular file is never a terminal, regardless of how it's typed.
func TestIsTerminal(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "tui-isterminal")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer func() { _ = f.Close() }()

	if isTerminal(f) {
		t.Fatal("isTerminal(regular file) = true, want false")
	}
}

func TestInteractiveTTY(t *testing.T) {
	if interactiveTTY(&bytes.Buffer{}) {
		t.Fatal("interactiveTTY(*bytes.Buffer) = true, want false — it is never a terminal")
	}

	f, err := os.CreateTemp(t.TempDir(), "tui-interactivetty")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer func() { _ = f.Close() }()

	if interactiveTTY(f) {
		t.Fatal("interactiveTTY(regular *os.File) = true, want false — it is not a char device")
	}

	// The true case — an *os.File that IS a char device — is the real-TTY
	// path, exercised manually: go test's own stdio is not guaranteed to be
	// a terminal (CI never is), so it can't be asserted true here without
	// flaking.
}

// TestStdinIsTTY documents that stdinIsTTY delegates to isTerminal(os.Stdin)
// rather than duplicating its logic; it can't assert a fixed true/false
// result since the test runner's stdin varies (a real terminal locally, a
// pipe/redirect in CI).
func TestStdinIsTTY(t *testing.T) {
	if got, want := stdinIsTTY(), isTerminal(os.Stdin); got != want {
		t.Fatalf("stdinIsTTY() = %v, want isTerminal(os.Stdin) = %v", got, want)
	}
}

func TestUseTUI(t *testing.T) {
	tests := []struct {
		name      string
		asJSON    bool
		stdinTTY  bool
		stdoutTTY bool
		want      bool
	}{
		// The key case: both stdio are terminals and --json is off → TUI,
		// regardless of whether the prompt came from args or the interactive
		// `prompt>` read (prompt source is no longer a factor).
		{name: "both TTY, no json", asJSON: false, stdinTTY: true, stdoutTTY: true, want: true},
		{name: "json forces the renderer", asJSON: true, stdinTTY: true, stdoutTTY: true, want: false},
		{name: "piped stdin stays line-rendered", asJSON: false, stdinTTY: false, stdoutTTY: true, want: false},
		{name: "redirected stdout stays line-rendered", asJSON: false, stdinTTY: true, stdoutTTY: false, want: false},
		{name: "neither TTY", asJSON: false, stdinTTY: false, stdoutTTY: false, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := useTUI(tt.asJSON, tt.stdinTTY, tt.stdoutTTY); got != tt.want {
				t.Errorf("useTUI(asJSON=%v, stdinTTY=%v, stdoutTTY=%v) = %v, want %v",
					tt.asJSON, tt.stdinTTY, tt.stdoutTTY, got, tt.want)
			}
		})
	}
}
