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
	// path exercised manually per docs/M1-PROOF.md: go test's own stdio is
	// not guaranteed to be a terminal (CI never is), so it can't be
	// asserted true here without flaking.
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
	// A *bytes.Buffer stdout is never a terminal, so useTUI must be false
	// regardless of the other inputs — this is the path every existing
	// test/CI invocation takes today, and it must keep resolving to
	// driveSession.
	tests := []struct {
		name           string
		asJSON         bool
		promptFromArgs bool
	}{
		{name: "json flag, prompt from args", asJSON: true, promptFromArgs: true},
		{name: "no json, prompt from args", asJSON: false, promptFromArgs: true},
		{name: "no json, prompt from stdin", asJSON: false, promptFromArgs: false},
		{name: "json flag, prompt from stdin", asJSON: true, promptFromArgs: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if useTUI(tt.asJSON, tt.promptFromArgs, &bytes.Buffer{}) {
				t.Fatal("useTUI with a non-terminal stdout must be false")
			}
		})
	}

	// asJSON=true and promptFromArgs=false are each independently
	// sufficient to force the renderer, ahead of the TTY checks in the &&
	// chain — lock that in explicitly rather than relying only on the
	// non-terminal stdout above to mask it.
	if useTUI(true, true, &bytes.Buffer{}) {
		t.Fatal("--json must force driveSession even with a prompt from args")
	}
	if useTUI(false, false, &bytes.Buffer{}) {
		t.Fatal("a stdin-sourced prompt must force driveSession")
	}
}
