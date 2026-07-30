package supervisor_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// TestSessionGuard_WiresLSPDiagnostics proves the Supervisor's REAL
// sessionGuard wiring — not internal/lspdiag in isolation — produces a tool
// registry whose edit results carry a live gopls diagnostic, on both the
// model-facing Content and the Diagnostics metadata slot. It is the
// supervisor-level half of the M7 LSP-wiring verification; the
// package-level half is internal/lspdiag's own live test.
//
// Skipped when gopls is not on PATH (see internal/lspdiag's live test for
// the install command); CI has no LSP servers installed.
func TestSessionGuard_WiresLSPDiagnostics(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not on PATH — install with: go install golang.org/x/tools/gopls@latest")
	}

	h := newHarness(t)
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "go.mod"), []byte("module fixture\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	mainGo := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hi\")\n}\n"
	if err := os.WriteFile(filepath.Join(cwd, "main.go"), []byte(mainGo), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	ctx := context.Background()
	info, err := h.sup.Create(ctx, "", supervisor.CreateOptions{Cwd: cwd, Model: "m"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs := h.session(info.ID)
	if fs.tools == nil {
		t.Fatal("supervisor did not inject a tool registry")
	}

	editTool, ok := fs.tools.Get("edit")
	if !ok {
		t.Fatal("expected the edit tool to resolve through the supervisor-injected registry")
	}

	input, err := json.Marshal(map[string]string{
		"path":       "main.go",
		"old_string": `fmt.Println("hi")`,
		"new_string": `fmt.Println(thisIdentifierDoesNotExist)`,
	})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}

	res, err := editTool.Run(ctx, input)
	if err != nil {
		t.Fatalf("edit.Run(): %v", err)
	}
	if res.IsError {
		t.Fatalf("edit.Run() reported IsError, content: %s", res.Content)
	}
	t.Logf("supervisor-wired edit result (Content, the model-facing field):\n%s", res.Content)
	if !strings.Contains(res.Content, "thisIdentifierDoesNotExist") {
		t.Fatalf("res.Content missing gopls's diagnostic — the supervisor's real sessionGuard wiring did not reach the model-facing content:\n%s", res.Content)
	}
	if len(res.Diagnostics) == 0 {
		t.Error("res.Diagnostics is empty, want gopls's diagnostic")
	}
}
