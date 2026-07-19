package tui_test

import (
	"strings"
	"testing"

	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// binaryRoster builds a two-row roster where one session runs a DIFFERENT gofer
// build from the app's own — the mid-upgrade drain state M6 process isolation
// produces, since a daemon upgrade does not migrate live sessions: they finish
// on the binary they started with. appVersion matches [tui.GoldenMeta]'s.
const appVersion = "0.3.0"

func binaryRoster(oldVersion string) []tui.SessionInfo {
	return []tui.SessionInfo{
		{
			ID:            "sess-new",
			Title:         "started after the upgrade",
			Summary:       "running on the new binary",
			Status:        tui.StatusWorking,
			BinaryVersion: appVersion,
			Updated:       tui.GoldenNow,
		},
		{
			ID:            "sess-old",
			Title:         "started before the upgrade",
			Summary:       "draining on the old binary",
			Status:        tui.StatusWorking,
			BinaryVersion: oldVersion,
			Updated:       tui.GoldenNow,
		},
	}
}

// TestOverviewMarksSkewedBinaryVersion is the operator-visibility half of M6
// §11's "session/list shows mixed binaryVersions" criterion. The version reached
// the wire before slice 3b but no client rendered it, so a mixed-version roster
// was invisible to a human. The roster now marks exactly the rows whose build
// differs from the app's.
func TestOverviewMarksSkewedBinaryVersion(t *testing.T) {
	o := tui.NewOverview(theme.Test(), tui.GoldenMeta()).WithSessions(binaryRoster("0.2.9"))
	got := testkit.Render(o, testkit.Width, testkit.Height)

	if !strings.Contains(got, "(v0.2.9)") {
		t.Errorf("roster does not mark the skewed session's binary version %q:\n%s", "(v0.2.9)", got)
	}
	// The matching row must NOT be marked: stamping an identical version on every
	// row is noise, and the whole signal here is the DIFFERENCE.
	if strings.Contains(got, "(v"+appVersion+")") {
		t.Errorf("roster marked a session running the app's own version %q; only skew should render:\n%s", appVersion, got)
	}
}

// TestOverviewBinaryMarkSuppressed pins the three cases that must render NO
// mark, each of which would otherwise light up rows an operator should ignore:
// a session on the app's own build (the overwhelmingly common case), an offline
// or pre-M6 row that carries no version at all, and an app with no version of
// its own to compare against.
func TestOverviewBinaryMarkSuppressed(t *testing.T) {
	noVersionMeta := tui.GoldenMeta()
	noVersionMeta.Version = ""

	tests := []struct {
		name    string
		meta    tui.OverviewMeta
		session tui.SessionInfo
	}{
		{
			name:    "same version as the app",
			meta:    tui.GoldenMeta(),
			session: tui.SessionInfo{ID: "s", Title: "matching", Status: tui.StatusWorking, BinaryVersion: appVersion, Updated: tui.GoldenNow},
		},
		{
			name:    "row carries no version",
			meta:    tui.GoldenMeta(),
			session: tui.SessionInfo{ID: "s", Title: "offline or pre-M6", Status: tui.StatusFinished, Updated: tui.GoldenNow},
		},
		{
			name:    "app carries no version",
			meta:    noVersionMeta,
			session: tui.SessionInfo{ID: "s", Title: "nothing to compare", Status: tui.StatusWorking, BinaryVersion: "0.2.9", Updated: tui.GoldenNow},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := tui.NewOverview(theme.Test(), tc.meta).WithSessions([]tui.SessionInfo{tc.session})
			got := testkit.Render(o, testkit.Width, testkit.Height)
			if strings.Contains(got, "(v") {
				t.Errorf("roster rendered a binary-version mark where none was expected:\n%s", got)
			}
		})
	}
}
