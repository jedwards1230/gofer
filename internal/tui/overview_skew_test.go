package tui_test

import (
	"strings"
	"testing"

	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// skewMeta is GoldenMeta with explicit CLI and daemon versions, for the
// stale-daemon banner goldens. The versions are valid semver so the classifier
// can order them (see internal/versionskew).
func skewMeta(cli, daemon string) tui.OverviewMeta {
	m := tui.GoldenMeta()
	m.Version = cli
	m.DaemonVersion = daemon
	return m
}

// TestGoldenOverviewSkewBanner pins the stale-daemon banner: when the roster
// came from a daemon older than this CLI, the header's fourth line (normally
// blank) carries the warning and the one-command fix, persistently.
func TestGoldenOverviewSkewBanner(t *testing.T) {
	o := tui.NewOverview(theme.Test(), skewMeta("v0.3.1", "v0.2.1")).WithSessions(rosterFixture())
	testkit.AssertGolden(t, "overview_skew_banner", testkit.Render(o, testkit.Width, testkit.Height))
}

// TestGoldenStyledOverviewSkewBanner is the styled counterpart: the banner
// renders through the warn (yellow) style, distinct from the muted header.
func TestGoldenStyledOverviewSkewBanner(t *testing.T) {
	o := tui.NewOverview(testkit.ColorTheme(), skewMeta("v0.3.1", "v0.2.1")).WithSessions(rosterFixture())
	testkit.AssertGoldenStyled(t, "overview_skew_banner", testkit.Render(o, testkit.Width, testkit.Height))
}

// TestOverviewSkewBannerCases covers the classifier-driven present/absent
// decision the golden cannot: the banner appears only when the daemon is older
// or a different build, and is absent (the header keeps its blank separator)
// when versions match, the daemon is newer, or either side is unknown.
func TestOverviewSkewBannerCases(t *testing.T) {
	const marker = "daemon is stale"
	const differsMarker = "different build"
	cases := []struct {
		name          string
		cli, daemon   string
		wantStale     bool // the "older" banner
		wantDifferent bool // the "differs" banner
	}{
		{"older daemon shows the stale banner", "v0.3.1", "v0.2.1", true, false},
		{"matching versions show no banner", "v0.3.1", "v0.3.1", false, false},
		{"newer daemon shows no banner (CLI is stale)", "v0.2.1", "v0.3.1", false, false},
		{"unknown daemon version shows no banner", "v0.3.1", "", false, false},
		{"unknown cli version shows no banner", "", "v0.2.1", false, false},
		{"non-semver differing builds show the differs banner", "dev-6661a1d", "dev-2aa7112", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := tui.NewOverview(theme.Test(), skewMeta(c.cli, c.daemon)).WithSessions(rosterFixture())
			got := testkit.Render(o, testkit.Width, testkit.Height)

			if hasStale := strings.Contains(got, marker); hasStale != c.wantStale {
				t.Errorf("stale banner present = %v, want %v\n%s", hasStale, c.wantStale, got)
			}
			if hasDiff := strings.Contains(got, differsMarker); hasDiff != c.wantDifferent {
				t.Errorf("differs banner present = %v, want %v\n%s", hasDiff, c.wantDifferent, got)
			}
			// Whenever a banner shows, it must carry the one-command fix.
			if (c.wantStale || c.wantDifferent) && !strings.Contains(got, "gofer daemon restart") {
				t.Errorf("banner omits the restart instruction:\n%s", got)
			}
		})
	}
}

// TestOverviewSkewBannerReusesClassifierSilentMatchesBaseline pins that a
// non-skewed roster renders byte-identically whether the daemon version is
// unknown or a known match — i.e. the banner reuses the header's existing
// blank-separator slot and costs nothing when silent. Both metas hold the same
// CLI Version so only the daemon axis (unknown vs equal) varies.
func TestOverviewSkewBannerReusesClassifierSilentMatchesBaseline(t *testing.T) {
	unknown := testkit.Render(tui.NewOverview(theme.Test(), skewMeta("v0.3.1", "")).WithSessions(rosterFixture()), testkit.Width, testkit.Height)
	matched := testkit.Render(tui.NewOverview(theme.Test(), skewMeta("v0.3.1", "v0.3.1")).WithSessions(rosterFixture()), testkit.Width, testkit.Height)
	if unknown != matched {
		t.Errorf("a matched daemon version differs from an unknown one — the banner slot is not reusing the blank separator\n--- unknown ---\n%s\n--- matched ---\n%s", unknown, matched)
	}
}
