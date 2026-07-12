package main

import (
	"context"
	"testing"

	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/theme"
	"github.com/jedwards1230/gofer/internal/tuibridge"
)

// TestResolveOverviewModel covers the none/one/many resolution outcomes the
// roster TUI's header uses: unlike resolveRunModel, none of them is an
// error — the overview always opens, with "" standing in for "no single
// obvious model to show yet".
func TestResolveOverviewModel(t *testing.T) {
	t.Run("no credentials", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv("ANTHROPIC_API_KEY", "")
		t.Setenv("OPENAI_API_KEY", "")

		if got := resolveOverviewModel(context.Background(), root); got != "" {
			t.Errorf("resolveOverviewModel() = %q, want \"\"", got)
		}
	})

	t.Run("one credential", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv("ANTHROPIC_API_KEY", "sk-test-key")
		t.Setenv("OPENAI_API_KEY", "")

		if got, want := resolveOverviewModel(context.Background(), root), "claude-sonnet-5"; got != want {
			t.Errorf("resolveOverviewModel() = %q, want %q", got, want)
		}
	})

	t.Run("multiple credentials", func(t *testing.T) {
		root := t.TempDir()
		t.Setenv("ANTHROPIC_API_KEY", "sk-test-key")
		t.Setenv("OPENAI_API_KEY", "sk-test-key")

		if got := resolveOverviewModel(context.Background(), root); got != "" {
			t.Errorf("resolveOverviewModel() = %q, want \"\" (never ambiguous — the TUI just shows no model yet)", got)
		}
	})
}

// TestRunTUI_ConstructionOnly exercises the non-terminal half of runTUI — a
// supervisor and [tui.App] build cleanly against a fresh store root and the
// app's Init produces the expected first command — without ever calling
// tea.Program.Run, which needs a real terminal (docs/TESTING.md: no PTY).
func TestRunTUI_ConstructionOnly(t *testing.T) {
	root := t.TempDir()
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	sup, err := supervisor.New(supervisor.Config{Root: root})
	if err != nil {
		t.Fatalf("supervisor.New: %v", err)
	}
	defer func() { _ = sup.Close() }()

	app := tui.NewApp(theme.Default(), tuibridge.New(sup), tui.OverviewMeta{
		App:     "gofer",
		Version: version,
		Model:   resolveOverviewModel(context.Background(), root),
		Cwd:     "/tmp/example",
	})

	if cmd := app.Init(); cmd == nil {
		t.Fatal("app.Init() = nil, want a roster-fetch command")
	}
}
