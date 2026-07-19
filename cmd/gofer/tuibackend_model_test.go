package main

// tuibackend_model_test.go pins the model the roster TUI's two backends show
// AND use. Issue #147's user-visible failure was the gap between those two on
// the local backend: the header displayed a resolved model that never reached
// Create, so the first session started from the TUI died on
// `runner: unknown model ""`.

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/tui"
)

// TestSelectTUIBackend_LocalHeaderModelIsAlsoUsedForCreate is the regression
// test for the reported failure. It reproduces the user's situation
// generically — two credentialed providers (previously an outright refusal)
// plus a config.Session.Model naming which to use — and asserts BOTH halves:
// the header shows that model, and a session created with no explicit model
// actually reaches the runner with it.
//
// The Create call cannot complete without a live provider, so the assertion
// is on WHICH error comes back: supervisor.ErrNoModel means the model never
// made it through (the bug), while a credential error means it did and the
// runner got as far as looking for the provider's key.
func TestSelectTUIBackend_LocalHeaderModelIsAlsoUsedForCreate(t *testing.T) {
	root := t.TempDir()
	// Both providers credentialed: the state that used to make the local TUI
	// resolve nothing at all. The config model is what breaks the tie.
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-key")
	t.Setenv("OPENAI_API_KEY", "sk-test-key")
	writeSessionModelConfig(t, root, "claude-sonnet-5")

	df := &daemonFlags{addr: closedDaemonAddr}
	backend, err := selectTUIBackend(context.Background(), df, t.TempDir(), root, io.Discard)
	if err != nil {
		t.Fatalf("selectTUIBackend: %v", err)
	}
	defer func() { _ = backend.close() }()

	if backend.meta.Model != "claude-sonnet-5" {
		t.Errorf("header Model = %q, want claude-sonnet-5", backend.meta.Model)
	}

	_, cerr := backend.sup.Create(context.Background(), "", tui.CreateOptions{Cwd: t.TempDir()})
	if errors.Is(cerr, supervisor.ErrNoModel) {
		t.Fatalf("Create reached the supervisor with no model (%v) — the header's model never got to Create", cerr)
	}
	// Reaching the runner's credential lookup is proof the model went through:
	// resolution happens before any credential is consulted.
	if cerr != nil && !strings.Contains(cerr.Error(), "anthropic") {
		t.Errorf("Create err = %v, want it to have reached anthropic's credential lookup", cerr)
	}
}

// TestSelectTUIBackend_LocalHeaderModelEmptyWithoutResolution is the negative
// half: with nothing to resolve, the header stays empty (the TUI must still
// open — an unresolved model is a valid starting state) and Create fails with
// the actionable supervisor.ErrNoModel rather than the SDK's
// `runner: unknown model ""`.
func TestSelectTUIBackend_LocalHeaderModelEmptyWithoutResolution(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	df := &daemonFlags{addr: closedDaemonAddr}
	backend, err := selectTUIBackend(context.Background(), df, t.TempDir(), root, io.Discard)
	if err != nil {
		t.Fatalf("selectTUIBackend: %v", err)
	}
	defer func() { _ = backend.close() }()

	if backend.meta.Model != "" {
		t.Errorf("header Model = %q, want empty with nothing resolvable", backend.meta.Model)
	}

	_, cerr := backend.sup.Create(context.Background(), "", tui.CreateOptions{Cwd: t.TempDir()})
	if !errors.Is(cerr, supervisor.ErrNoModel) {
		t.Fatalf("Create err = %v, want supervisor.ErrNoModel", cerr)
	}
	// The message must carry the remedy, not just the sentinel's bare text —
	// this is the string the operator reads in the TUI.
	if got := cerr.Error(); !strings.Contains(got, "gofer login") || !strings.Contains(got, config.ConfigFileName) {
		t.Errorf("Create err = %q, want it to name both remedies (gofer login / config.json)", got)
	}
}

// TestSelectTUIBackend_DaemonHeaderShowsDaemonModel covers the header
// inconsistency: the daemon branch used to build OverviewMeta with no Model
// at all, so the same TUI showed a model locally and none over a daemon. The
// value must come from the DAEMON (gofer/hello), since only it knows what its
// own sessions will use.
func TestSelectTUIBackend_DaemonHeaderShowsDaemonModel(t *testing.T) {
	// testDaemon configures DefaultModel "faux"; a locally-recomputed model
	// could never produce that value, so seeing it proves the header read the
	// daemon's own.
	addr := testDaemon(t, "", fauxProvider)
	df := &daemonFlags{addr: addr}

	backend, err := selectTUIBackend(context.Background(), df, t.TempDir(), "", io.Discard)
	if err != nil {
		t.Fatalf("selectTUIBackend: %v", err)
	}
	defer func() { _ = backend.close() }()

	if backend.meta.Model != "faux" {
		t.Errorf("daemon-backend header Model = %q, want the daemon's own default faux", backend.meta.Model)
	}
}
