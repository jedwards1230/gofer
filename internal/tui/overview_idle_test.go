package tui_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
)

// idleRosterFixture is a roster with one row of each effective status, including
// a StatusIdle (reloaded/at-rest) row — the session state the reloaded-session
// fix introduces. It drives the idle overview goldens.
func idleRosterFixture() []tui.SessionInfo {
	return []tui.SessionInfo{
		{
			ID:      "0192a1b2-0000-7000-8000-000000000001",
			Title:   "wire the websocket ACP listener",
			Summary: "turn in flight",
			Status:  tui.StatusWorking,
			Model:   "fable-5",
			Updated: tui.GoldenNow.Add(-30 * time.Second),
		},
		{
			ID:      "0192a1b2-0000-7000-8000-000000000002",
			Title:   "keycloak path-b groundwork",
			Summary: "turn finished — awaiting the next prompt",
			Status:  tui.StatusNeedsInput,
			Model:   "fable-5",
			Updated: tui.GoldenNow.Add(-2 * time.Minute),
		},
		{
			ID:      "0192a1b2-0000-7000-8000-000000000003",
			Title:   "explore three agent ecosystems",
			Summary: "reloaded from disk — not prompted since",
			Status:  tui.StatusIdle,
			Model:   "fable-5",
			Updated: tui.GoldenNow.Add(-5 * time.Minute),
		},
		{
			ID:      "0192a1b2-0000-7000-8000-000000000004",
			Title:   "authentik token exchange rfc 8693",
			Summary: "Keycloak Path-B foundation complete",
			Status:  tui.StatusFinished,
			Model:   "fable-5",
			Updated: tui.GoldenNow.Add(-time.Hour),
		},
	}
}

// TestGoldenOverviewIdleFlat renders the flat roster with a reloaded (idle) row.
// Its status word reads "Idle" (muted, not the yellow of a working/awaiting row)
// and the header tallies it separately as "N idle" rather than as awaiting input.
func TestGoldenOverviewIdleFlat(t *testing.T) {
	o := newOverview().WithSessions(idleRosterFixture())
	testkit.AssertGolden(t, "overview_idle_flat", testkit.Render(o, testkit.Width, testkit.Height))
}

// TestGoldenStyledOverviewIdleFlat is the styled counterpart: it locks the idle
// row's status word as MUTED, distinct from the yellow a working/awaiting row
// carries and the green a finished one does — the color is the whole point of a
// resting state (nothing needed), and the Ascii golden can't show it.
func TestGoldenStyledOverviewIdleFlat(t *testing.T) {
	o := tui.NewOverview(testkit.ColorTheme(), tui.GoldenMeta()).WithSessions(idleRosterFixture())
	testkit.AssertGoldenStyled(t, "overview_idle_flat", testkit.Render(o, testkit.Width, testkit.Height))
}

// TestGoldenOverviewIdleGrouped renders the grouped view with an idle row,
// exercising the dedicated "Idle" section between Needs input and Finished.
func TestGoldenOverviewIdleGrouped(t *testing.T) {
	o := newOverview().WithSessions(idleRosterFixture()).ToggleView()
	testkit.AssertGolden(t, "overview_idle_grouped", testkit.Render(o, testkit.Width, testkit.Height))
}

// TestOverviewIdleNotAwaitingInput is constraint (a): a reloaded (idle) row must
// NOT move the awaiting-input counter and must NOT render as "Needs input".
// Opening a reloaded session adds an idle row to the roster; the awaiting-input
// count is the same with and without it, so browsing is invisible to the counter.
func TestOverviewIdleNotAwaitingInput(t *testing.T) {
	base := []tui.SessionInfo{
		{ID: "sess-1", Title: "awaiting next prompt", Status: tui.StatusNeedsInput, Updated: tui.GoldenNow},
		{ID: "sess-2", Title: "done", Status: tui.StatusFinished, Updated: tui.GoldenNow},
	}
	// The reloaded session the user just opened, appearing as an at-rest row.
	reloaded := tui.SessionInfo{ID: "sess-3", Title: "reloaded session", Status: tui.StatusIdle, Updated: tui.GoldenNow}

	before := testkit.Render(newOverview().WithSessions(base), testkit.Width, testkit.Height)
	after := testkit.Render(newOverview().WithSessions(append(append([]tui.SessionInfo(nil), base...), reloaded)), testkit.Width, testkit.Height)

	const awaiting = "1 awaiting input"
	if !headerLine(before) || !strings.Contains(before, awaiting) {
		t.Fatalf("pre-condition: base header missing %q:\n%s", awaiting, before)
	}
	// Opening the reloaded session must not change the awaiting-input tally.
	if !strings.Contains(after, awaiting) {
		t.Errorf("awaiting-input count moved when a reloaded (idle) session was opened; want %q:\n%s", awaiting, after)
	}
	// And the reloaded row itself must not present as "Needs input".
	row := rowContaining(t, after, "reloaded session")
	if strings.Contains(row, "Needs input") {
		t.Errorf("reloaded (idle) row rendered as %q: %q", "Needs input", row)
	}
	if !strings.Contains(row, "Idle") {
		t.Errorf("reloaded row did not render the %q status word: %q", "Idle", row)
	}
	if !strings.Contains(after, "1 idle") {
		t.Errorf("header did not tally the reloaded session as idle (%q):\n%s", "1 idle", after)
	}
}

// TestOverviewIdleWithPendingStillAwaits is constraint (b): a genuine pending
// request on an at-rest (idle) row must STILL read as "Needs input" and count as
// awaiting — a real prompt is never hidden behind the resting state. The wire may
// deliver Status=StatusIdle for a session that replayed a retained approval on
// resume; effectiveStatus reclassifies it by Pending exactly as it does a working
// row (see TestOverviewCountsPendingAwaitsInput).
func TestOverviewIdleWithPendingStillAwaits(t *testing.T) {
	o := newOverview().WithSessions([]tui.SessionInfo{
		{ID: "sess-1", Title: "reloaded but blocked", Status: tui.StatusIdle, Pending: 1, Updated: tui.GoldenNow},
	})
	got := testkit.Render(o, testkit.Width, testkit.Height)
	if !strings.Contains(got, "1 awaiting input") {
		t.Errorf("idle+pending row not counted as awaiting input:\n%s", got)
	}
	row := rowContaining(t, got, "reloaded but blocked")
	if !strings.Contains(row, "Needs input") {
		t.Errorf("idle+pending row did not render %q: %q", "Needs input", row)
	}
	if strings.Contains(row, "Idle") {
		t.Errorf("idle+pending row leaked the resting %q word instead of surfacing the prompt: %q", "Idle", row)
	}
}

// TestOverviewIdleAndNeedsInputBucketSeparately is constraint (c) in counter
// form: an ordinary NeedsInput row (a session that really finished a turn) is
// still tallied as awaiting input, unchanged — the idle bucket is additive and
// pulls only reloaded/at-rest rows out of the awaiting count, nothing else.
func TestOverviewIdleAndNeedsInputBucketSeparately(t *testing.T) {
	o := newOverview().WithSessions([]tui.SessionInfo{
		{ID: "sess-1", Title: "finished a turn", Status: tui.StatusNeedsInput, Updated: tui.GoldenNow},
		{ID: "sess-2", Title: "reloaded at rest", Status: tui.StatusIdle, Updated: tui.GoldenNow},
	})
	got := testkit.Render(o, testkit.Width, testkit.Height)
	if !strings.Contains(got, "1 awaiting input") {
		t.Errorf("NeedsInput row not counted as awaiting (should be unchanged):\n%s", got)
	}
	if !strings.Contains(got, "1 idle") {
		t.Errorf("Idle row not counted as idle:\n%s", got)
	}
}

// headerLine reports whether s has the customary header (a sanity guard so the
// counter assertions are known to be reading a real header render).
func headerLine(s string) bool { return strings.Contains(s, "awaiting input") }

// rowContaining returns the single rendered line containing needle, failing the
// test when none does.
func rowContaining(t *testing.T, rendered, needle string) string {
	t.Helper()
	for _, line := range strings.Split(rendered, "\n") {
		if strings.Contains(line, needle) {
			return line
		}
	}
	t.Fatalf("no rendered row contains %q:\n%s", needle, rendered)
	return ""
}
