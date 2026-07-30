package tui

// context_test.go covers the /context command-panel tab (context.go,
// gofer#177): the fill-bar + row rendering, the honest empty states (no
// active session, no settled turn yet), the unknown-context-window
// counts-only fallback, and the configured-threshold line. White-box
// (package tui) because contextView is unexported — the App-level "/context
// opens the panel" behavior is covered in context_select_test.go (package
// tui_test).

import (
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// fixtureContextSession is the SessionInfo the populated-context golden
// renders against: a registered model with a real window and a measured
// last-turn usage comfortably inside it.
func fixtureContextSession() *SessionInfo {
	return &SessionInfo{
		ID:            "0192a1b2-ctx0-7000-8000-000000000001",
		Model:         "claude-sonnet-5",
		LastUsage:     provider.Usage{InputTokens: 40000, CacheReadTokens: 10000, OutputTokens: 500},
		ContextWindow: 200000,
	}
}

func renderContext(t *testing.T, name string, v contextView) {
	t.Helper()
	testkit.AssertGolden(t, name, testkit.Render(v, testkit.Width, testkit.Height))
}

// TestGoldenContextPopulated covers the full row set: model, window, fill
// bar + percentage, in-use/free counts, and the auto-compaction threshold
// line (read through a Config closure reporting the default policy).
func TestGoldenContextPopulated(t *testing.T) {
	v := contextView{theme: theme.Test(), sess: fixtureContextSession(), env: GoldenCommandEnv()}
	renderContext(t, "context_populated", v)
}

// TestGoldenContextNoSession covers the overview case: no active session
// collapses to one muted line, matching usage.go/stats.go's empty-state
// convention.
func TestGoldenContextNoSession(t *testing.T) {
	v := contextView{theme: theme.Test()}
	renderContext(t, "context_no_session", v)
}

// TestContextNoSettledTurn asserts a session with no measured usage yet
// (attached before the first turn finished) renders one honest line, not a
// 0-token bar or a divide-by-zero percentage.
func TestContextNoSettledTurn(t *testing.T) {
	v := contextView{theme: theme.Test(), sess: &SessionInfo{Model: "claude-sonnet-5", ContextWindow: 200000}}
	got := v.View(testkit.Width, testkit.Height)
	if !strings.Contains(got, "No turn has completed yet") {
		t.Errorf("expected the no-settled-turn empty state, got:\n%s", got)
	}
	if strings.Contains(got, "%") {
		t.Errorf("must not render a percentage with no measured usage, got:\n%s", got)
	}
}

// TestContextUnknownWindowCountsOnly asserts an unregistered model
// (ContextWindow == 0, meaning UNKNOWN per house rule — never "no window")
// renders counts only: no percentage, no fill bar, no free-space figure,
// since all three would require dividing by the unknown window.
func TestContextUnknownWindowCountsOnly(t *testing.T) {
	v := contextView{theme: theme.Test(), sess: &SessionInfo{
		Model:     "totally-unregistered-model",
		LastUsage: provider.Usage{InputTokens: 5000},
		// ContextWindow left zero: unknown.
	}}
	got := v.View(testkit.Width, testkit.Height)
	if !strings.Contains(got, "unknown for this model") {
		t.Errorf("expected the unknown-window line, got:\n%s", got)
	}
	if !strings.Contains(got, "5000 tokens") {
		t.Errorf("expected the counts-only measured figure, got:\n%s", got)
	}
	if strings.Contains(got, "%") || strings.Contains(got, "█") {
		t.Errorf("must not render a percentage or fill bar with an unknown window, got:\n%s", got)
	}
}

// TestContextThresholdLineReflectsConfig asserts the auto-compaction
// threshold row reads the LIVE configured value (not gofer's built-in
// default) through the CommandEnv.Config seam, and that Disabled renders as
// "off" rather than a percentage that would misstate the actual policy.
func TestContextThresholdLineReflectsConfig(t *testing.T) {
	half := 0.5
	env := GoldenCommandEnv()
	env.Config = func() (config.Config, error) {
		return config.Config{Compaction: config.Compaction{ThresholdFraction: &half}}, nil
	}
	v := contextView{theme: theme.Test(), sess: fixtureContextSession(), env: env}
	got := v.View(testkit.Width, testkit.Height)
	if !strings.Contains(got, "Auto-compacts at: 50%") {
		t.Errorf("expected the configured 50%% threshold, got:\n%s", got)
	}

	env.Config = func() (config.Config, error) {
		return config.Config{Compaction: config.Compaction{Disabled: true}}, nil
	}
	v = contextView{theme: theme.Test(), sess: fixtureContextSession(), env: env}
	got = v.View(testkit.Width, testkit.Height)
	if !strings.Contains(got, "Auto-compaction: off") {
		t.Errorf("expected the disabled line, got:\n%s", got)
	}
}

// TestContextBar pins the fill-bar cell math directly: 25%/50%/100% of a
// 20-cell bar, plus the over-100% clamp (a provider call can exceed the
// registry's static capacity, e.g. a temporarily larger context beta).
func TestContextBar(t *testing.T) {
	tests := []struct {
		name         string
		used, window int
		want         string
	}{
		{"quarter full", 50, 200, "[█████░░░░░░░░░░░░░░░]"},
		{"half full", 100, 200, "[██████████░░░░░░░░░░]"},
		{"full", 200, 200, "[████████████████████]"},
		{"over 100% clamps", 400, 200, "[████████████████████]"},
		{"zero window never divides", 100, 0, "[░░░░░░░░░░░░░░░░░░░░]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := contextBar(tt.used, tt.window); got != tt.want {
				t.Errorf("contextBar(%d, %d) = %q, want %q", tt.used, tt.window, got, tt.want)
			}
		})
	}
}
