//go:build linux

package sandbox

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/loop"
)

func bwrapLookPathHit(string) (string, error) { return "/usr/bin/bwrap", nil }
func bwrapLookPathMiss(string) (string, error) {
	return "", errors.New("executable file not found in $PATH")
}

func TestBwrapContainer_Available(t *testing.T) {
	tests := []struct {
		name     string
		lookPath func(string) (string, error)
		want     bool
	}{
		{"runtime present", bwrapLookPathHit, true},
		{"runtime absent", bwrapLookPathMiss, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newBwrapContainer(tt.lookPath)
			if got := c.Available(); got != tt.want {
				t.Errorf("Available() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBwrapContainer_CanContain(t *testing.T) {
	tests := []struct {
		name     string
		lookPath func(string) (string, error)
		call     loop.ToolCall
		want     bool
	}{
		{"available + bash", bwrapLookPathHit, loop.ToolCall{Name: "bash"}, true},
		{"available + read", bwrapLookPathHit, loop.ToolCall{Name: "read"}, true},
		{"available + ask_user", bwrapLookPathHit, loop.ToolCall{Name: "ask_user"}, true},
		{"available + unknown tool", bwrapLookPathHit, loop.ToolCall{Name: "unknown"}, false},
		{"runtime absent + bash", bwrapLookPathMiss, loop.ToolCall{Name: "bash"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newBwrapContainer(tt.lookPath)
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

func TestBwrapContainer_WrapCommand_RuntimeAbsent(t *testing.T) {
	c := newBwrapContainer(bwrapLookPathMiss)
	if argv, ok := c.WrapCommand("echo hi", "/tmp/work"); ok || argv != nil {
		t.Errorf("WrapCommand() = (%v, %v), want (nil, false) when the runtime is absent", argv, ok)
	}
}

func TestBwrapContainer_WrapCommand_Shape(t *testing.T) {
	c := newBwrapContainer(bwrapLookPathHit)
	argv, ok := c.WrapCommand("echo hi", "/tmp/work")
	if !ok {
		t.Fatal("WrapCommand() ok = false, want true")
	}
	if !slices.Contains(argv, "--unshare-net") {
		t.Errorf("argv missing --unshare-net: %v", argv)
	}
	if argv[0] != "bwrap" {
		t.Errorf("argv[0] = %q, want %q", argv[0], "bwrap")
	}
	if argv[len(argv)-1] != "echo hi" {
		t.Errorf("argv trailer = %v, want command last", argv)
	}
}

// TestBwrapContainer_WrapCommand_NoSecretLeak asserts the argv WrapCommand
// returns — which the loop hands straight to exec.CommandContext — never
// carries a secret sitting in the process environment.
func TestBwrapContainer_WrapCommand_NoSecretLeak(t *testing.T) {
	t.Setenv("GOFER_TEST_SECRET", "super-secret-token-do-not-leak")

	c := newBwrapContainer(bwrapLookPathHit)
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
