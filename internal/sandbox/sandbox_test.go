package sandbox

import (
	"context"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/loop"
)

func TestNoopContainer(t *testing.T) {
	c := noopContainer{}

	if c.Available() {
		t.Error("Available() = true, want false")
	}
	ok, err := c.CanContain(context.Background(), loop.ToolCall{Name: "bash"})
	if ok || err != nil {
		t.Errorf("CanContain() = (%v, %v), want (false, nil)", ok, err)
	}
	argv, ok := c.WrapCommand("echo hi", "/tmp")
	if argv != nil || ok {
		t.Errorf("WrapCommand() = (%v, %v), want (nil, false)", argv, ok)
	}
}

// TestNew_ReturnsUsableContainer exercises New() on whatever host runs the
// test — it must always return a non-nil, non-panicking Container. Available()
// legitimately varies by whether the local host has the runtime installed;
// runtime-detection behavior itself is covered per-OS in
// container_darwin_test.go / container_linux_test.go.
func TestNew_ReturnsUsableContainer(t *testing.T) {
	c := New()
	if c == nil {
		t.Fatal("New() returned nil")
		return
	}
	_ = c.Available()
	if _, ok := c.WrapCommand("true", t.TempDir()); !c.Available() && ok {
		t.Error("WrapCommand() ok=true while Available()=false")
	}
}
