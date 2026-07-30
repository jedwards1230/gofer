package supervisor_test

// lspdiag_config_test.go proves config.LSP actually reaches sessionGuard's
// lspdiag.Wrap call — not just that lspdiag.Wrap itself behaves correctly
// (that is internal/lspdiag's own wrap_test.go), and not just that the
// supervisor's wiring produces real diagnostics when LSP defaults to ON
// (that is lspdiag_wiring_test.go's live gopls test). Those two prove the
// pipe exists; this proves a config VALUE actually flows through it — a test
// that only checked the defaults would keep passing even if the config read
// at internal/supervisor/supervisor.go's sessionGuard were deleted and
// lspOpts went back to a hardcoded literal.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/runner"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// lspToolType returns the %T of the "edit" tool the supervisor injected for
// sess — the observable signal of whether lspdiag.Wrap decorated the
// registry: enabled, Get("edit") resolves through lspdiag's own (unexported)
// diagnosingTool, so its qualified type name contains "lspdiag"; disabled,
// Wrap returns the base registry completely unchanged (see [lspdiag.Wrap]'s
// doc) and the type name does not.
func lspToolType(t *testing.T, sess *fakeSession) string {
	t.Helper()
	if sess.tools == nil {
		t.Fatal("supervisor did not inject a tool registry")
	}
	tool, ok := sess.tools.Get("edit")
	if !ok {
		t.Fatal("expected the edit tool to resolve through the supervisor-injected registry")
	}
	return fmt.Sprintf("%T", tool)
}

// TestSessionGuard_LSPConfigDisablesWrapping proves an explicit
// `lsp.enabled: false` reaches sessionGuard's lspdiag.Wrap call and turns
// wrapping off, on BOTH permission-mode paths (sessionGuard's yolo branch and
// its guarded/ask branch each build their own lspOpts — see
// supervisor.go:277-294). A supervisor built with the hardcoded
// `Enabled: true` this replaced would wrap the registry regardless of what
// this test's config.LSP says, failing both subtests below.
func TestSessionGuard_LSPConfigDisablesWrapping(t *testing.T) {
	disabled := false
	for _, mode := range []config.PermissionMode{config.PermissionModeAsk, config.PermissionModeYolo} {
		t.Run(string(mode), func(t *testing.T) {
			h := newLSPHarness(t, func() config.LSP { return config.LSP{Enabled: &disabled} }, mode)

			info, err := h.sup.Create(context.Background(), "", supervisor.CreateOptions{Cwd: t.TempDir(), Model: "m"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			if got := lspToolType(t, h.session(info.ID)); strings.Contains(got, "lspdiag") {
				t.Fatalf("edit tool type = %s, want an UNwrapped registry (lsp.enabled: false did not reach sessionGuard)", got)
			}
		})
	}
}

// TestSessionGuard_LSPConfigEnablesWrapping is
// TestSessionGuard_LSPConfigDisablesWrapping's positive counterpart: an
// explicit `lsp.enabled: true` (a non-default config.LSP value in the sense
// that it is EXPLICIT, not merely the nil-defaults-to-true zero value) wraps
// the registry on both permission-mode paths.
func TestSessionGuard_LSPConfigEnablesWrapping(t *testing.T) {
	enabled := true
	for _, mode := range []config.PermissionMode{config.PermissionModeAsk, config.PermissionModeYolo} {
		t.Run(string(mode), func(t *testing.T) {
			h := newLSPHarness(t, func() config.LSP { return config.LSP{Enabled: &enabled} }, mode)

			info, err := h.sup.Create(context.Background(), "", supervisor.CreateOptions{Cwd: t.TempDir(), Model: "m"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			if got := lspToolType(t, h.session(info.ID)); !strings.Contains(got, "lspdiag") {
				t.Fatalf("edit tool type = %s, want lspdiag's wrapped registry (lsp.enabled: true did not reach sessionGuard)", got)
			}
		})
	}
}

// newLSPHarness is [newHarness] (helpers_test.go) with an explicit LSP
// resolver and permission mode injected — newHarness itself builds a Config
// with neither set, so it cannot exercise either knob.
func newLSPHarness(t *testing.T, lsp func() config.LSP, mode config.PermissionMode) *harness {
	t.Helper()
	h := &harness{t: t, root: t.TempDir(), sessions: make(map[string]*fakeSession)}

	var nextID int64
	cfg := supervisor.Config{
		Root:           h.root,
		LSP:            lsp,
		PermissionMode: func() config.PermissionMode { return mode },
		NewSession: func(_ context.Context, opts runner.Options) (supervisor.Session, error) {
			id := "sess-" + strconv.FormatInt(atomic.AddInt64(&nextID, 1), 10)
			fs := h.register(id, opts.Cwd)
			fs.approver = opts.Approver
			fs.tools = opts.Tools
			return fs, nil
		},
	}

	sup, err := supervisor.New(cfg)
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	h.sup = sup
	t.Cleanup(func() { _ = sup.Close() })
	return h
}
