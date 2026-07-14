//go:build darwin

package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/loop"
)

func lookPathHit(string) (string, error) { return "/usr/bin/sandbox-exec", nil }
func lookPathMiss(string) (string, error) {
	return "", errors.New("executable file not found in $PATH")
}

func TestSeatbeltContainer_Available(t *testing.T) {
	tests := []struct {
		name     string
		lookPath func(string) (string, error)
		want     bool
	}{
		{"runtime present", lookPathHit, true},
		{"runtime absent", lookPathMiss, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newSeatbeltContainer(tt.lookPath)
			if got := c.Available(); got != tt.want {
				t.Errorf("Available() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSeatbeltContainer_CanContain(t *testing.T) {
	tests := []struct {
		name     string
		lookPath func(string) (string, error)
		call     loop.ToolCall
		want     bool
	}{
		{"available + bash", lookPathHit, loop.ToolCall{Name: "bash"}, true},
		{"available + read", lookPathHit, loop.ToolCall{Name: "read"}, true},
		{"available + unknown tool", lookPathHit, loop.ToolCall{Name: "unknown"}, false},
		{"runtime absent + bash", lookPathMiss, loop.ToolCall{Name: "bash"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newSeatbeltContainer(tt.lookPath)
			got, err := c.CanContain(context.Background(), tt.call)
			if err != nil {
				t.Fatalf("CanContain() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("CanContain() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSeatbeltContainer_WrapCommand_RuntimeAbsent(t *testing.T) {
	c := newSeatbeltContainer(lookPathMiss)
	if argv, ok := c.WrapCommand("echo hi", "/tmp/work"); ok || argv != nil {
		t.Errorf("WrapCommand() = (%v, %v), want (nil, false) when the runtime is absent", argv, ok)
	}
}

func TestSeatbeltContainer_WrapCommand_Shape(t *testing.T) {
	c := newSeatbeltContainer(lookPathHit)
	argv, ok := c.WrapCommand("echo hi", "/tmp/work")
	if !ok {
		t.Fatal("WrapCommand() ok = false, want true")
	}
	if len(argv) != 6 {
		t.Fatalf("argv length = %d, want 6: %v", len(argv), argv)
	}
	if argv[0] != "sandbox-exec" || argv[1] != "-p" {
		t.Errorf("argv[0:2] = %v, want [sandbox-exec -p]", argv[0:2])
	}
	if !strings.Contains(argv[2], "/tmp/work") {
		t.Errorf("profile missing workdir: %s", argv[2])
	}
	if !strings.Contains(argv[2], "(deny network*)") {
		t.Errorf("profile missing network denial: %s", argv[2])
	}
	if argv[3] != "/bin/sh" || argv[4] != "-c" || argv[5] != "echo hi" {
		t.Errorf("argv[3:6] = %v, want [/bin/sh -c \"echo hi\"]", argv[3:6])
	}
}

// TestSeatbeltContainer_WrapCommand_NoSecretLeak asserts the argv WrapCommand
// returns — which the loop hands straight to exec.CommandContext — never
// carries a secret sitting in the process environment.
func TestSeatbeltContainer_WrapCommand_NoSecretLeak(t *testing.T) {
	t.Setenv("GOFER_TEST_SECRET", "super-secret-token-do-not-leak")

	c := newSeatbeltContainer(lookPathHit)
	argv, ok := c.WrapCommand("echo hi", "/tmp/work")
	if !ok {
		t.Fatal("WrapCommand() ok = false, want true")
	}
	for _, a := range argv {
		if strings.Contains(a, "super-secret-token-do-not-leak") {
			t.Fatalf("argv leaked env secret: %v", argv)
		}
	}
}
