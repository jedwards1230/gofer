package tui

// fixtures_test.go holds the small set of golden-test fixtures shared across
// both this package's internal tests (app_internal_test.go, dialog_color_test.go)
// and the black-box tui_test package (app_test.go, overview_test.go,
// color_layout_test.go) — the standard export_test pattern: this file is only
// compiled for tests, but its exported names are reachable from tui_test the
// same as any other exported package identifier.

import (
	"time"

	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/config"
)

// GoldenNow is the fixed reference instant every golden-test fixture ages its
// sessions against, so relative-age output (humanAge/humanDuration) is
// deterministic across machines and CI.
var GoldenNow = time.Date(2026, 7, 12, 18, 0, 0, 0, time.UTC)

// GoldenMeta returns the shared OverviewMeta the App/Overview golden tests
// build through.
func GoldenMeta() OverviewMeta {
	return OverviewMeta{App: "gofer", Version: "0.3.0", Model: "claude-sonnet-5", Cwd: "~/orchestration", Now: GoldenNow}
}

// GoldenCommandEnv returns the shared [CommandEnv] the App/command-panel
// golden tests build through: fixed version/cwd/root and the
// auth-independence default (zero providers authenticated, no persisted
// config) — the state every panel view must open cleanly in. SaveConfig is a
// no-op (never touches disk) — it exists so /config's edit paths exercise a
// non-nil closure in golden tests without leaving files behind; tests that
// need to observe what was written supply their own CommandEnv (see
// config_view_test.go).
func GoldenCommandEnv() CommandEnv {
	return CommandEnv{
		Version:    "0.3.0",
		Cwd:        "~/orchestration",
		Root:       "~/.gofer",
		Auth:       func() ([]ProviderAuth, error) { return nil, nil },
		Config:     func() (config.Config, error) { return config.Config{}, nil },
		SaveConfig: func(config.Config) error { return nil },
	}
}

// GoldenRoster returns the two-session fixture the App golden and behavioral
// tests navigate: a working session (selected first — most recently active)
// and an idle one awaiting input.
func GoldenRoster() []SessionInfo {
	return []SessionInfo{
		{
			ID:      "0192a1b2-app0-7000-8000-000000000001",
			Title:   "wire the app root",
			Summary: "overview <-> peek <-> attach nav",
			Status:  StatusWorking,
			Cost:    provider.Cost{USD: 0.1120},
			// Usage is populated (row 2 leaves it zero-valued) so the /usage and
			// /stats panel goldens render real token numbers, and the Stats
			// rollup sums a mixed populated/zero roster.
			Usage:   provider.Usage{InputTokens: 18234, OutputTokens: 4096, CacheReadTokens: 12000, CacheWriteTokens: 512},
			Created: GoldenNow.Add(-15 * time.Minute),
			Updated: GoldenNow.Add(-2 * time.Minute),
		},
		{
			ID:      "0192a1b2-app0-7000-8000-000000000002",
			Title:   "review the supervisor contract",
			Summary: "turn finished — awaiting the next prompt",
			Status:  StatusNeedsInput,
			Cost:    provider.Cost{USD: 0.0450},
			Created: GoldenNow.Add(-30 * time.Minute),
			Updated: GoldenNow.Add(-5 * time.Minute),
		},
	}
}
