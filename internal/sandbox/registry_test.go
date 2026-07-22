package sandbox

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/loop"
	"github.com/jedwards1230/agent-sdk-go/tool"
)

// fakeContainer is a Container test double: it lets tests control
// Available()/CanContain() and observe/redirect WrapCommand() without
// depending on a real sandbox runtime being installed.
type fakeContainer struct {
	available  bool
	canContain bool
	wrapFn     func(command, workdir string) ([]string, bool)
	wrapCalled bool
}

func (f *fakeContainer) Available() bool { return f.available }

func (f *fakeContainer) CanContain(context.Context, loop.ToolCall) (bool, error) {
	return f.canContain, nil
}

func (f *fakeContainer) WrapCommand(command, workdir string) ([]string, bool) {
	f.wrapCalled = true
	if f.wrapFn == nil {
		return nil, false
	}
	return f.wrapFn(command, workdir)
}

func bashInput(t *testing.T, command string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]string{"command": command})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	return b
}

func TestWrapRegistry_Unavailable_ReturnsPlainBuiltins(t *testing.T) {
	dir := t.TempDir()
	fc := &fakeContainer{available: false}
	reg := WrapRegistry(dir, fc)

	bash, ok := reg.Get("bash")
	if !ok {
		t.Fatal("expected a bash tool")
	}
	res, err := bash.Run(context.Background(), bashInput(t, "echo hi"))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.Contains(res.Content, "hi") {
		t.Errorf("Content = %q, want to contain %q", res.Content, "hi")
	}
	if fc.wrapCalled {
		t.Error("WrapCommand should not be called when the Container is unavailable")
	}
}

func TestWrapRegistry_NilContainer_ReturnsPlainBuiltins(t *testing.T) {
	dir := t.TempDir()
	reg := WrapRegistry(dir, nil)
	if _, ok := reg.Get("bash"); !ok {
		t.Fatal("expected a bash tool")
	}
}

func TestWrapRegistry_Available_ContainsBash(t *testing.T) {
	dir := t.TempDir()
	var gotCommand, gotWorkdir string
	fc := &fakeContainer{
		available: true,
		wrapFn: func(command, workdir string) ([]string, bool) {
			gotCommand, gotWorkdir = command, workdir
			return []string{"/bin/sh", "-c", "echo contained:" + command}, true
		},
	}
	reg := WrapRegistry(dir, fc)

	bash, ok := reg.Get("bash")
	if !ok {
		t.Fatal("expected a bash tool")
	}
	res, err := bash.Run(context.Background(), bashInput(t, "echo hi"))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !fc.wrapCalled {
		t.Error("expected WrapCommand to be called")
	}
	if gotCommand != "echo hi" {
		t.Errorf("WrapCommand command = %q, want %q", gotCommand, "echo hi")
	}
	if gotWorkdir != dir {
		t.Errorf("WrapCommand workdir = %q, want %q", gotWorkdir, dir)
	}
	if !strings.Contains(res.Content, "contained:echo hi") {
		t.Errorf("Content = %q, want to contain %q", res.Content, "contained:echo hi")
	}
}

func TestWrapRegistry_Available_OtherToolsUnwrapped(t *testing.T) {
	dir := t.TempDir()
	fc := &fakeContainer{available: true}
	reg := WrapRegistry(dir, fc)

	if _, ok := reg.Get("read"); !ok {
		t.Fatal("expected the read tool to still be present")
	}
	if fc.wrapCalled {
		t.Error("WrapCommand should not be called while merely fetching a non-bash tool")
	}
}

func TestWrapRegistry_ContainedBash_WrapFails(t *testing.T) {
	dir := t.TempDir()
	fc := &fakeContainer{
		available: true,
		wrapFn:    func(string, string) ([]string, bool) { return nil, false },
	}
	reg := WrapRegistry(dir, fc)

	bash, _ := reg.Get("bash")
	res, err := bash.Run(context.Background(), bashInput(t, "echo hi"))
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !res.IsError {
		t.Error("expected IsError when WrapCommand reports the call cannot be contained")
	}
}

func TestWrapRegistry_ContainedBash_RequiresCommand(t *testing.T) {
	dir := t.TempDir()
	fc := &fakeContainer{available: true}
	reg := WrapRegistry(dir, fc)

	bash, _ := reg.Get("bash")
	if _, err := bash.Run(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Error("expected an error for a missing command")
	}
}

func TestWrapRegistry_SpecsUnchanged(t *testing.T) {
	dir := t.TempDir()
	plain := loop.FromRegistry(tool.NewRegistry(tool.Builtins(dir)...))
	fc := &fakeContainer{
		available: true,
		wrapFn:    func(string, string) ([]string, bool) { return []string{"true"}, true },
	}
	wrapped := WrapRegistry(dir, fc)

	plainSpecs, wrappedSpecs := plain.Specs(), wrapped.Specs()
	if len(wrappedSpecs) != len(plainSpecs) {
		t.Fatalf("Specs() length = %d, want %d", len(wrappedSpecs), len(plainSpecs))
	}
	for i := range plainSpecs {
		if wrappedSpecs[i].Name != plainSpecs[i].Name {
			t.Errorf("Specs()[%d].Name = %q, want %q", i, wrappedSpecs[i].Name, plainSpecs[i].Name)
		}
		if wrappedSpecs[i].Description != plainSpecs[i].Description {
			t.Errorf("Specs()[%d].Description changed for %q", i, plainSpecs[i].Name)
		}
		if string(wrappedSpecs[i].InputSchema) != string(plainSpecs[i].InputSchema) {
			t.Errorf("Specs()[%d].InputSchema changed for %q", i, plainSpecs[i].Name)
		}
	}
}

// extraTool is a stand-in for a gofer-authored tool (the real one is
// internal/decision's ask_user). It lives here rather than importing that
// package so these tests keep proving the REGISTRY seam, not one tool.
type extraTool struct{ ran bool }

func (*extraTool) Name() string        { return "extra_tool" }
func (*extraTool) Description() string { return "a gofer-authored tool" }
func (*extraTool) Spec() tool.Schema {
	return tool.ObjectSchema([]string{"why"}, map[string]tool.Property{
		"why": {Type: "string", Description: "why"},
	})
}

func (e *extraTool) Run(context.Context, json.RawMessage) (tool.Result, error) {
	e.ran = true
	return tool.Result{Content: "ran"}, nil
}

func TestWrapRegistry_ExtraToolsAreRegistered(t *testing.T) {
	dir := t.TempDir()
	extra := &extraTool{}
	reg := WrapRegistry(dir, nil, extra)

	got, ok := reg.Get("extra_tool")
	if !ok {
		t.Fatal("extra tool not resolvable through Get")
	}
	res, err := got.Run(context.Background(), json.RawMessage(`{"why":"because"}`))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Content != "ran" || !extra.ran {
		t.Errorf("Run = %+v (ran=%v), want the extra tool itself to have run", res, extra.ran)
	}
	// It must also be ADVERTISED — a tool the model cannot see is not
	// registered in any useful sense.
	var advertised bool
	for _, s := range reg.Specs() {
		if s.Name == "extra_tool" {
			advertised = true
			if s.Description != extra.Description() {
				t.Errorf("spec description = %q, want %q", s.Description, extra.Description())
			}
			if !strings.Contains(string(s.InputSchema), `"why"`) {
				t.Errorf("spec schema = %s, want the tool's own schema", s.InputSchema)
			}
		}
	}
	if !advertised {
		t.Error("extra tool missing from Specs()")
	}
	// And the builtins are still there alongside it.
	if _, ok := reg.Get("bash"); !ok {
		t.Error("bash missing after adding an extra tool")
	}
}

func TestWrapRegistry_ExtraToolsSurviveContainerWrapping(t *testing.T) {
	dir := t.TempDir()
	fc := &fakeContainer{
		available: true,
		wrapFn:    func(string, string) ([]string, bool) { return []string{"true"}, true },
	}
	reg := WrapRegistry(dir, fc, &extraTool{})

	// The contained-registry wrapper delegates everything but bash, so an
	// extra tool must resolve identically with a live Container.
	if _, ok := reg.Get("extra_tool"); !ok {
		t.Fatal("extra tool not resolvable through the contained registry")
	}
	if _, ok := reg.Get("bash"); !ok {
		t.Error("bash missing from the contained registry")
	}
}
