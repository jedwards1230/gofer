package lspdiag_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/loop"
	"github.com/jedwards1230/agent-sdk-go/tool"

	"github.com/jedwards1230/gofer/internal/lspdiag"
)

// TestLive_GoplsDiagnosticsReachToolContent is the M7 LSP-wiring
// verification artifact: it starts a REAL gopls against a real, minimal Go
// module, drives it through the exact loop.ToolRegistry.Get("edit").Run
// path sessionGuard wires into every gofer session (internal/supervisor),
// edits a valid file into one carrying a real compile error, and asserts
// gopls's own diagnostic text ends up on res.Content — the field the SDK's
// loop feeds back to the model on its next turn (see loop.runOneTool's
// provider.ToolResultBlock(call.ID, modelContent, ...) in agent-sdk-go's
// loop/loop.go), not merely the client/UI-facing Diagnostics metadata slot.
//
// Skipped when gopls is not on PATH: `go install
// golang.org/x/tools/gopls@latest` is the prerequisite this test (and this
// package's real wiring) needs to produce a live signal at all. CI has no
// LSP servers installed (matching the SDK's own lsp package — see its doc),
// so this test is expected to skip there; it is the local/manual
// verification run for this workstream.
func TestLive_GoplsDiagnosticsReachToolContent(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH — install with: go install golang.org/x/tools/gopls@latest")
	}

	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "go.mod"), "module lspdiagfixture\n\ngo 1.25\n")
	mainGo := `package main

import "fmt"

func main() {
	fmt.Println("hello")
}
`
	mustWriteFile(t, filepath.Join(dir, "main.go"), mainGo)

	// The exact registry construction sessionGuard uses (internal/sandbox
	// wraps this further for bash/askUser; neither affects edit/write).
	base := loop.FromRegistry(tool.NewRegistry(tool.Builtins(dir)...))
	mgr := lspdiag.NewManager()
	t.Cleanup(func() {
		if err := mgr.Close(); err != nil {
			t.Errorf("Manager.Close() = %v", err)
		}
	})
	reg := lspdiag.Wrap(base, mgr, dir, lspdiag.Options{
		Enabled:        true,
		Timeout:        20 * time.Second, // a cold gopls workspace load can be slow
		MaxDiagnostics: 10,
	})

	editTool, ok := reg.Get("edit")
	if !ok {
		t.Fatal("expected the edit tool to resolve")
	}

	// Introduce a real, unambiguous compile error: reference an undefined
	// identifier — exactly the kind of mistake an editing model can make.
	input, err := json.Marshal(map[string]string{
		"path":       "main.go",
		"old_string": `fmt.Println("hello")`,
		"new_string": `fmt.Println(thisIdentifierDoesNotExist)`,
	})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}

	res, err := editTool.Run(context.Background(), input)
	if err != nil {
		t.Fatalf("edit.Run() error = %v", err)
	}
	if res.IsError {
		t.Fatalf("edit.Run() reported IsError, content: %s", res.Content)
	}

	t.Logf("tool result Content (this is what the SDK loop feeds back to the model — see loop.runOneTool):\n%s", res.Content)
	t.Logf("tool result Diagnostics (event.ToolCallFinished.Diagnostics, the client/UI-facing metadata copy):\n%s", strings.Join(res.Diagnostics, "\n"))

	if !strings.Contains(res.Content, "thisIdentifierDoesNotExist") {
		t.Fatalf("res.Content does not carry gopls's diagnostic for the undefined identifier — got:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "[lsp diagnostics: main.go]") {
		t.Errorf("res.Content missing the diagnostics header, got:\n%s", res.Content)
	}
	if len(res.Diagnostics) == 0 {
		t.Error("res.Diagnostics is empty, want at least gopls's undefined-identifier diagnostic")
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
