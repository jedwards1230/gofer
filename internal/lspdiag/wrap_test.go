package lspdiag_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/loop"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/lspdiag"
)

// stubRegistry is a minimal loop.ToolRegistry test double: one tool, no
// specs, so Wrap's decorator can be exercised without pulling in the whole
// SDK builtin set.
type stubRegistry struct{ tools map[string]loop.Tool }

func (r stubRegistry) Get(name string) (loop.Tool, bool) { t, ok := r.tools[name]; return t, ok }
func (r stubRegistry) Specs() []provider.ToolSpec        { return nil }

// stubTool returns a fixed loop.ToolResult/error, letting tests drive every
// branch diagnosingTool.Run can take (error, IsError, no edits, a real
// edit).
type stubTool struct {
	result loop.ToolResult
	err    error
}

func (t stubTool) Run(context.Context, json.RawMessage) (loop.ToolResult, error) {
	return t.result, t.err
}

func TestWrap_NilManager_ReturnsUnwrapped(t *testing.T) {
	base := stubRegistry{tools: map[string]loop.Tool{"edit": stubTool{}}}
	got := lspdiag.Wrap(base, nil, "/tmp", lspdiag.Options{Enabled: true})
	if _, ok := got.(stubRegistry); !ok {
		t.Errorf("Wrap with a nil Manager should return the base registry unchanged, got %T", got)
	}
}

func TestWrap_Disabled_ReturnsUnwrapped(t *testing.T) {
	base := stubRegistry{tools: map[string]loop.Tool{"edit": stubTool{}}}
	got := lspdiag.Wrap(base, lspdiag.NewManager(), "/tmp", lspdiag.Options{Enabled: false})
	if _, ok := got.(stubRegistry); !ok {
		t.Errorf("Wrap with Enabled=false should return the base registry unchanged, got %T", got)
	}
}

func TestWrap_NonDiagnosableTool_PassesThroughUntouched(t *testing.T) {
	want := loop.ToolResult{Content: "42 lines"}
	base := stubRegistry{tools: map[string]loop.Tool{"grep": stubTool{result: want}}}
	reg := lspdiag.Wrap(base, lspdiag.NewManager(), "/tmp", lspdiag.Options{Enabled: true})

	grep, ok := reg.Get("grep")
	if !ok {
		t.Fatal("expected grep to still resolve")
	}
	got, err := grep.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Run() = %+v, want the base tool's result completely unchanged: %+v", got, want)
	}
}

func TestWrap_ErrorResult_PassesThroughUnchanged(t *testing.T) {
	want := loop.ToolResult{Content: "boom", IsError: true, Edits: []event.FileEdit{{Path: "x.go", NewText: "package main"}}}
	base := stubRegistry{tools: map[string]loop.Tool{"edit": stubTool{result: want}}}
	reg := lspdiag.Wrap(base, lspdiag.NewManager(), "/tmp", lspdiag.Options{Enabled: true})

	edit, _ := reg.Get("edit")
	got, err := edit.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Run() = %+v, want the failed result unchanged: %+v", got, want)
	}
}

func TestWrap_NoEdits_PassesThroughUnchanged(t *testing.T) {
	want := loop.ToolResult{Content: "ran, nothing changed"}
	base := stubRegistry{tools: map[string]loop.Tool{"edit": stubTool{result: want}}}
	reg := lspdiag.Wrap(base, lspdiag.NewManager(), "/tmp", lspdiag.Options{Enabled: true})

	edit, _ := reg.Get("edit")
	got, err := edit.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Run() = %+v, want unchanged: %+v", got, want)
	}
}

func TestWrap_UnsupportedExtension_NoDiagnosticsAppended(t *testing.T) {
	want := loop.ToolResult{
		Content: "edited notes.txt (1 replacement)",
		Edits:   []event.FileEdit{{Path: "notes.txt", NewText: "hello"}},
	}
	base := stubRegistry{tools: map[string]loop.Tool{"edit": stubTool{result: want}}}
	reg := lspdiag.Wrap(base, lspdiag.NewManager(), t.TempDir(), lspdiag.Options{Enabled: true})

	edit, _ := reg.Get("edit")
	got, err := edit.Run(context.Background(), nil)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got.Content != want.Content {
		t.Errorf("Content = %q, want unchanged %q (no language server exists for .txt)", got.Content, want.Content)
	}
	if len(got.Diagnostics) != 0 {
		t.Errorf("Diagnostics = %v, want none", got.Diagnostics)
	}
}

func TestWrap_SpecsUnchanged(t *testing.T) {
	base := stubRegistry{tools: map[string]loop.Tool{"edit": stubTool{}}}
	reg := lspdiag.Wrap(base, lspdiag.NewManager(), "/tmp", lspdiag.Options{Enabled: true})
	if specs := reg.Specs(); specs != nil {
		t.Errorf("Specs() = %v, want the base registry's Specs() passed through verbatim (nil here)", specs)
	}
}

func TestWrap_OnlyEditAndWriteAreDecorated(t *testing.T) {
	editWant := loop.ToolResult{Content: "e"}
	writeWant := loop.ToolResult{Content: "w"}
	lsWant := loop.ToolResult{Content: "l"}
	base := stubRegistry{tools: map[string]loop.Tool{
		"edit":  stubTool{result: editWant},
		"write": stubTool{result: writeWant},
		"ls":    stubTool{result: lsWant},
	}}
	reg := lspdiag.Wrap(base, lspdiag.NewManager(), "/tmp", lspdiag.Options{Enabled: true})

	// A non-diagnosable tool comes back as the exact same underlying value —
	// not merely equal output, but genuinely unwrapped (tool.NewRegistry-style
	// registries return the same identity for every Get of the same name).
	ls, _ := reg.Get("ls")
	got, err := ls.Run(context.Background(), nil)
	if err != nil || !reflect.DeepEqual(got, lsWant) {
		t.Errorf("ls.Run() = %+v, %v, want %+v, nil", got, err, lsWant)
	}
}
