package tui

// stats_test.go covers the /stats command-panel tab (stats.go): the session
// lifecycle rows (age/last-active/status/model) against a deterministic
// reference time, the roster rollup (session count + summed tokens/cost), the
// overview case (no lifecycle rows, rollup only), and the omit-unset-timestamp
// discipline. White-box (package tui) because statsView is unexported — the
// App-level "/stats opens the panel" behavior is covered in command_test.go.

import (
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// fixtureStatsSession is the active session the attached-case goldens render:
// created 15m before and last active 2m before GoldenNow, so the age and
// last-active rows are deterministic.
func fixtureStatsSession() *SessionInfo {
	return &SessionInfo{
		ID:      "0192a1b2-stat-7000-8000-000000000001",
		Title:   "wire the stats view",
		Status:  StatusWorking,
		Model:   "claude-sonnet-5",
		Created: GoldenNow.Add(-15 * time.Minute),
		Updated: GoldenNow.Add(-2 * time.Minute),
	}
}

func renderStats(t *testing.T, name string, v statsView) {
	t.Helper()
	testkit.AssertGolden(t, name, testkit.Render(v, testkit.Width, testkit.Height))
}

// TestGoldenStatsAttached covers the attached case: the session lifecycle rows
// above the roster rollup, aged against GoldenNow.
func TestGoldenStatsAttached(t *testing.T) {
	v := statsView{
		theme:  theme.Test(),
		sess:   fixtureStatsSession(),
		now:    GoldenNow,
		roster: GoldenRoster(),
	}
	renderStats(t, "stats_attached", v)
}

// TestGoldenStatsOverview covers the overview case: no active session, so only
// the roster rollup renders (no lifecycle rows).
func TestGoldenStatsOverview(t *testing.T) {
	v := statsView{theme: theme.Test(), now: GoldenNow, roster: GoldenRoster()}
	renderStats(t, "stats_overview", v)
}

// TestGoldenStatsEmptyRoster covers the zero-session rollup: "Sessions: 0",
// zero tokens, and a dash for cost.
func TestGoldenStatsEmptyRoster(t *testing.T) {
	v := statsView{theme: theme.Test(), now: GoldenNow}
	renderStats(t, "stats_empty_roster", v)
}

// TestStatsRollupSumsTokensAndCost covers the rollup arithmetic: tokens sum
// every normalized bucket across all rows and cost sums each row's USD.
func TestStatsRollupSumsTokensAndCost(t *testing.T) {
	roster := []SessionInfo{
		{Usage: provider.Usage{InputTokens: 100, OutputTokens: 50, CacheReadTokens: 10, CacheWriteTokens: 5}, Cost: provider.Cost{USD: 0.10}},
		{Usage: provider.Usage{InputTokens: 200, OutputTokens: 60}, Cost: provider.Cost{USD: 0.25}},
	}
	v := statsView{theme: theme.Test(), now: GoldenNow, roster: roster}
	got := v.View(testkit.Width, testkit.Height)
	if !strings.Contains(got, "Total tokens: 425") { // 100+50+10+5 + 200+60
		t.Fatalf("expected summed tokens 425, got:\n%s", got)
	}
	if !strings.Contains(got, "Total cost: $0.3500") {
		t.Fatalf("expected summed cost $0.3500, got:\n%s", got)
	}
	if !strings.Contains(got, "Sessions: 2") {
		t.Fatalf("expected Sessions: 2, got:\n%s", got)
	}
}

// TestStatsOmitsAgeWhenTimestampUnset covers the omit-don't-age-against-zero
// discipline: a session with no Created time renders no Age row rather than a
// nonsensical multi-decade duration.
func TestStatsOmitsAgeWhenTimestampUnset(t *testing.T) {
	v := statsView{theme: theme.Test(), now: GoldenNow, sess: &SessionInfo{ID: "x", Title: "no timestamps"}}
	got := v.View(testkit.Width, testkit.Height)
	if strings.Contains(got, "Age:") {
		t.Fatalf("expected no Age row when Created is unset, got:\n%s", got)
	}
	if strings.Contains(got, "Last active:") {
		t.Fatalf("expected no Last active row when Updated is unset, got:\n%s", got)
	}
}
