package tui

// usage_test.go covers the /usage command-panel tab (usage.go): the token +
// cost row mapping, the omit-cache-when-zero and omit-cost-breakdown-when-zero
// discipline, the dash-not-$0 rule for an unpriced session, and the honest
// empty states (no active session, and a session with no usage yet). White-box
// (package tui) because usageView is unexported — the App-level "/usage opens
// the panel" behavior is covered in command_test.go (package tui_test).

import (
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// fixtureUsageSession is the SessionInfo the populated-usage goldens render
// against: every token bucket and every cost bucket non-zero, so the full
// table (including the cache rows and the cost breakdown) renders.
func fixtureUsageSession() *SessionInfo {
	return &SessionInfo{
		ID:    "0192a1b2-use0-7000-8000-000000000001",
		Title: "wire the usage view",
		Usage: provider.Usage{
			InputTokens:      18234,
			OutputTokens:     4096,
			CacheReadTokens:  12000,
			CacheWriteTokens: 512,
		},
		Cost: provider.Cost{
			USD:           0.1120,
			InputUSD:      0.0547,
			OutputUSD:     0.0410,
			CacheReadUSD:  0.0036,
			CacheWriteUSD: 0.0127,
		},
	}
}

func renderUsage(t *testing.T, name string, v usageView) {
	t.Helper()
	testkit.AssertGolden(t, name, testkit.Render(v, testkit.Width, testkit.Height))
}

func renderUsageStyled(t *testing.T, name string, v usageView) {
	t.Helper()
	testkit.AssertGoldenStyled(t, name, testkit.Render(v, testkit.Width, testkit.Height))
}

// TestGoldenUsagePopulated covers the full table: all four token buckets and
// the whole cost breakdown present.
func TestGoldenUsagePopulated(t *testing.T) {
	v := usageView{theme: theme.Test(), sess: fixtureUsageSession()}
	renderUsage(t, "usage_populated", v)
}

// TestGoldenUsageNoSession covers the overview case: no active session
// collapses to one muted "attach to see its usage" line, invisible under the
// forced-Ascii profile — hence a styled counterpart.
func TestGoldenUsageNoSession(t *testing.T) {
	v := usageView{theme: theme.Test()}
	renderUsage(t, "usage_no_session", v)
}

// TestGoldenUsageNoSessionStyled is TestGoldenUsageNoSession's color-state
// counterpart: the empty-state line renders in MutedStyle.
func TestGoldenUsageNoSessionStyled(t *testing.T) {
	v := usageView{theme: testkit.ColorTheme()}
	renderUsageStyled(t, "usage_no_session", v)
}

// TestGoldenUsageZeroUsage covers a session that has recorded no usage yet
// (attached before the first turn finished): one honest "no usage recorded
// yet" line rather than a wall of zeros.
func TestGoldenUsageZeroUsage(t *testing.T) {
	v := usageView{theme: theme.Test(), sess: &SessionInfo{ID: "x", Title: "brand new"}}
	renderUsage(t, "usage_zero", v)
}

// TestGoldenUsageZeroUsageStyled is TestGoldenUsageZeroUsage's color-state
// counterpart: the "no usage recorded yet" line renders in MutedStyle.
func TestGoldenUsageZeroUsageStyled(t *testing.T) {
	v := usageView{theme: testkit.ColorTheme(), sess: &SessionInfo{ID: "x", Title: "brand new"}}
	renderUsageStyled(t, "usage_zero", v)
}

// TestUsageOmitsZeroCacheRows covers the omit-don't-blank-fill discipline for
// the cache rows: a session with only input/output tokens shows neither cache
// row.
func TestUsageOmitsZeroCacheRows(t *testing.T) {
	v := usageView{theme: theme.Test(), sess: &SessionInfo{
		ID:    "x",
		Usage: provider.Usage{InputTokens: 100, OutputTokens: 50},
		Cost:  provider.Cost{USD: 0.01},
	}}
	got := v.View(testkit.Width, testkit.Height)
	if strings.Contains(got, "Cache read") || strings.Contains(got, "Cache write") {
		t.Fatalf("expected no cache rows when both cache buckets are zero, got:\n%s", got)
	}
}

// TestUsageUnpricedShowsDashNotZero covers the dash-not-$0 rule: a session
// with real tokens but zero cost (an unregistered model has unknown pricing)
// shows "Cost: —", never "$0.0000", which would present unpriced usage as free.
func TestUsageUnpricedShowsDashNotZero(t *testing.T) {
	v := usageView{theme: theme.Test(), sess: &SessionInfo{
		ID:    "x",
		Usage: provider.Usage{InputTokens: 100, OutputTokens: 50},
	}}
	got := v.View(testkit.Width, testkit.Height)
	if !strings.Contains(got, "Cost: —") {
		t.Fatalf("expected an unpriced session to render Cost: —, got:\n%s", got)
	}
	if strings.Contains(got, "$0.0000") {
		t.Fatalf("expected no $0.0000 for an unpriced session, got:\n%s", got)
	}
}
