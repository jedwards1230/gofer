package tui_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// rosterFixture is the shared session set the overview golden tests render: a
// working session, a working session with pending approvals, an idle session
// awaiting input, and two finished sessions of different ages.
func rosterFixture() []tui.SessionInfo {
	return []tui.SessionInfo{
		{
			ID:      "0192a1b2-0000-7000-8000-000000000001",
			Title:   "explore three agent ecosystems",
			Summary: "M2 launched; awaiting sketch review + 4 decisions",
			Status:  tui.StatusWorking,
			Model:   "fable-5",
			Cost:    provider.Cost{USD: 0.3821},
			Updated: tui.GoldenNow.Add(-2 * time.Minute),
		},
		{
			ID:      "0192a1b2-0000-7000-8000-000000000002",
			Title:   "wire the websocket ACP listener",
			Summary: "blocked: approve Bash(kubectl delete pod)",
			Status:  tui.StatusWorking,
			Model:   "fable-5",
			Cost:    provider.Cost{USD: 0.0912},
			Pending: 2,
			Updated: tui.GoldenNow.Add(-30 * time.Second),
		},
		{
			ID:      "0192a1b2-0000-7000-8000-000000000003",
			Title:   "keycloak path-b groundwork",
			Summary: "turn finished — awaiting the next prompt",
			Status:  tui.StatusNeedsInput,
			Model:   "fable-5",
			Cost:    provider.Cost{USD: 0.1204},
			Updated: tui.GoldenNow.Add(-5 * time.Minute),
		},
		{
			ID:      "0192a1b2-0000-7000-8000-000000000004",
			Title:   "authentik token exchange rfc 8693",
			Summary: "Keycloak Path-B foundation complete and verified",
			Status:  tui.StatusFinished,
			Model:   "fable-5",
			Cost:    provider.Cost{USD: 1.4230},
			Updated: tui.GoldenNow.Add(-time.Hour),
		},
		{
			ID:      "0192a1b2-0000-7000-8000-000000000005",
			Title:   "openclaw dev setup",
			Summary: "Heartbeat revamp spec'd and handed off",
			Status:  tui.StatusFinished,
			Model:   "fable-5",
			Cost:    provider.Cost{USD: 0.0311},
			Updated: tui.GoldenNow.Add(-26 * time.Hour),
		},
	}
}

func newOverview() tui.Overview {
	return tui.NewOverview(theme.Test(), tui.GoldenMeta())
}

// TestGoldenOverviewFlat renders the flat, recency-sorted roster with the
// first row selected.
func TestGoldenOverviewFlat(t *testing.T) {
	o := newOverview().WithSessions(rosterFixture())
	testkit.AssertGolden(t, "overview_flat", testkit.Render(o, testkit.Width, testkit.Height))
}

// TestGoldenStyledOverviewFlat is TestGoldenOverviewFlat's styled-golden
// counterpart: the same roster, rendered through testkit.ColorTheme, locks
// the working/needs-input rows' yellow status words against the finished
// rows' green ones — a distinction the Ascii golden's plain text can't make.
func TestGoldenStyledOverviewFlat(t *testing.T) {
	o := tui.NewOverview(testkit.ColorTheme(), tui.GoldenMeta()).WithSessions(rosterFixture())
	testkit.AssertGoldenStyled(t, "overview_flat", testkit.Render(o, testkit.Width, testkit.Height))
}

// TestGoldenOverviewGrouped renders the grouped view: Working / Needs input /
// Finished sections, each recency-sorted, with per-section counts in the
// header.
func TestGoldenOverviewGrouped(t *testing.T) {
	o := newOverview().WithSessions(rosterFixture()).ToggleView()
	testkit.AssertGolden(t, "overview_grouped", testkit.Render(o, testkit.Width, testkit.Height))
}

// TestGoldenOverviewDispatchTyping renders the roster with a partially typed
// dispatch-bar prompt, replacing the placeholder with the live buffer.
func TestGoldenOverviewDispatchTyping(t *testing.T) {
	o := newOverview().WithSessions(rosterFixture())
	for _, r := range "fix the flaky peek test" {
		o = o.TypeRune(r)
	}
	testkit.AssertGolden(t, "overview_dispatch_typing", testkit.Render(o, testkit.Width, testkit.Height))
}

// TestGoldenOverviewDispatchCursorMidText covers the cursor-aware buffer
// (inputbuf.go) rendering the "▏" glyph at its actual mid-text position, not
// always appended at the end the way the pre-cursor append-only buffer
// rendered it: type a phrase, then move left a few runes before rendering.
func TestGoldenOverviewDispatchCursorMidText(t *testing.T) {
	o := newOverview().WithSessions(rosterFixture())
	for _, r := range "fix the flaky peek test" {
		o = o.TypeRune(r)
	}
	for i := 0; i < 5; i++ {
		o = o.MoveLeft()
	}
	testkit.AssertGolden(t, "overview_dispatch_cursor_mid_text", testkit.Render(o, testkit.Width, testkit.Height))
}

// TestGoldenOverviewSelectionMoves renders the roster after moving the
// selection down twice, exercising the caret and selection-follow.
func TestGoldenOverviewSelectionMoves(t *testing.T) {
	o := newOverview().WithSessions(rosterFixture()).MoveDown().MoveDown()
	testkit.AssertGolden(t, "overview_selection_moved", testkit.Render(o, testkit.Width, testkit.Height))
}

// TestGoldenOverviewEmpty renders the empty-roster state, which invites the
// user to start a session from the dispatch bar.
func TestGoldenOverviewEmpty(t *testing.T) {
	o := newOverview()
	testkit.AssertGolden(t, "overview_empty", testkit.Render(o, testkit.Width, testkit.Height))
}

// TestOverviewPendingReadsAsNeedsInput verifies a session with pending
// permission requests reads as a plain "Needs input" row — pending is a
// boolean folded into the status (colored via the state style), not a count:
// one or many pending approvals both surface the same signal, with no digit
// and no leading glyph. [tui.SessionInfo.Pending] is still plumbed from the
// wire (see internal/daemonbridge's toTUISessionInfo) — it just reclassifies
// the row's effective status rather than printing a number.
func TestOverviewPendingReadsAsNeedsInput(t *testing.T) {
	o := newOverview().WithSessions([]tui.SessionInfo{
		{ID: "sess-1", Title: "blocked on approval", Status: tui.StatusWorking, Pending: 2, Updated: tui.GoldenNow},
	})
	got := testkit.Render(o, testkit.Width, testkit.Height)
	if !strings.Contains(got, "Needs input") {
		t.Errorf("rendered roster does not contain the %q status word:\n%s", "Needs input", got)
	}
	// No glyph, and no pending count anywhere — pending is boolean now.
	for _, absent := range []string{"●", "●2", "(2)", "2"} {
		if strings.Contains(got, absent) {
			t.Errorf("rendered roster unexpectedly contains %q (glyph/count should be gone):\n%s", absent, got)
		}
	}
}

// TestOverviewCountsPendingAwaitsInput verifies the header counts bucket a
// permission-blocked session (Status still StatusWorking, Pending>0 — the
// daemon's coarse status doesn't demote while the turn is technically in
// flight) as "awaiting input", not "working" — the same reclassification
// [effectiveStatus] already applies for the row's status-word color, so the
// header and the roster rows agree.
func TestOverviewCountsPendingAwaitsInput(t *testing.T) {
	o := newOverview().WithSessions([]tui.SessionInfo{
		{ID: "sess-1", Title: "blocked one", Status: tui.StatusWorking, Pending: 1, Updated: tui.GoldenNow},
		{ID: "sess-2", Title: "blocked two", Status: tui.StatusWorking, Pending: 1, Updated: tui.GoldenNow},
		{ID: "sess-3", Title: "done", Status: tui.StatusFinished, Updated: tui.GoldenNow},
	})
	got := testkit.Render(o, testkit.Width, testkit.Height)
	if !strings.Contains(got, "2 awaiting input · 0 working") {
		t.Errorf("rendered header does not contain %q:\n%s", "2 awaiting input · 0 working", got)
	}
}

// multiCwdFixture builds a small roster spanning two distinct working
// directories, so the flat view's cwd grouping has more than one group to
// render.
func multiCwdFixture() []tui.SessionInfo {
	return []tui.SessionInfo{
		{
			ID:      "0192a1b2-cwd0-7000-8000-000000000001",
			Title:   "explore three agent ecosystems",
			Summary: "M2 launched; awaiting sketch review + 4 decisions",
			Status:  tui.StatusWorking,
			Cwd:     "~/orchestration",
			Updated: tui.GoldenNow.Add(-2 * time.Minute),
		},
		{
			ID:      "0192a1b2-cwd0-7000-8000-000000000002",
			Title:   "wire the websocket ACP listener",
			Summary: "blocked: approve Bash(kubectl delete pod)",
			Status:  tui.StatusWorking,
			Cwd:     "~/orchestration",
			Pending: 2,
			Updated: tui.GoldenNow.Add(-30 * time.Second),
		},
		{
			ID:      "0192a1b2-cwd1-7000-8000-000000000003",
			Title:   "live-reload html canvas server",
			Summary: "phase 1 scoped; sketch review pending",
			Status:  tui.StatusNeedsInput,
			Cwd:     "~/scrim",
			Updated: tui.GoldenNow.Add(-5 * time.Minute),
		},
	}
}

// TestGoldenOverviewMultiCwd renders the flat view over a roster spanning two
// working directories, locking TWO cwd group headers — the most-recently-
// active cwd's group (~/orchestration, holding the -30s and -2m sessions)
// first, then ~/scrim.
func TestGoldenOverviewMultiCwd(t *testing.T) {
	o := newOverview().WithSessions(multiCwdFixture())
	testkit.AssertGolden(t, "overview_multi_cwd", testkit.Render(o, testkit.Width, testkit.Height))
}

// bigRosterFixture returns enough sessions (well past a small viewport's
// row budget) for the scroll tests below to force real windowing/scrolling,
// unlike rosterFixture's 5 rows.
func bigRosterFixture() []tui.SessionInfo {
	var out []tui.SessionInfo
	for i := 0; i < 20; i++ {
		out = append(out, tui.SessionInfo{
			ID:      fmt.Sprintf("0192a1b2-big0-7000-8000-%012d", i),
			Title:   fmt.Sprintf("session %02d", i),
			Status:  tui.StatusWorking,
			Updated: tui.GoldenNow.Add(-time.Duration(i) * time.Minute),
		})
	}
	return out
}

// TestOverviewScrollOverridesSelectionWindow verifies a positive scroll
// offset ([Overview.ViewWithMenu]'s new parameter) shows different rows than
// the default (scroll 0) selection-anchored window on a roster long enough
// to overflow the viewport — the manual scroll a mouse wheel/PgUp-PgDn
// drives (see App.handleWheel/handleOverviewKey), overriding the
// selection-follow behavior rather than replacing it (scroll 0 renders
// byte-identical to before this parameter existed — see the golden tests
// above).
func TestOverviewScrollOverridesSelectionWindow(t *testing.T) {
	o := newOverview().WithSessions(bigRosterFixture())
	const width, height = 80, 10 // a small viewport: bigRosterFixture overflows it

	// Move selection all the way down to the oldest (last) row, so the
	// default scroll-0 selection-anchored window sits at the BOTTOM of the
	// roster — an unambiguous contrast against a large scroll offset, which
	// scrolls back toward the TOP regardless of where the selection is.
	for range bigRosterFixture() {
		o = o.MoveDown()
	}

	selectionAnchored := o.ViewWithMenu(width, height, nil, 0, false)
	scrolledToTop := o.ViewWithMenu(width, height, nil, 1_000_000, false)
	if selectionAnchored == scrolledToTop {
		t.Fatal("a large scroll offset rendered identically to scroll=0; want the roster window to move")
	}
	if !strings.Contains(scrolledToTop, "session 00") {
		t.Errorf("fully scrolled-back render missing the newest row (session 00):\n%s", scrolledToTop)
	}
	if strings.Contains(selectionAnchored, "session 00") {
		t.Errorf("selection-anchored render (selection on the oldest row) unexpectedly shows the newest row:\n%s", selectionAnchored)
	}
}

// TestOverviewHideDispatchBlanksBar verifies the panel-open row hides the
// dispatch bar's rule/input/hint (item 3) while keeping the row COUNT
// unchanged, so the frame still totals the same height — the app_panel_*
// goldens' three-blank-line replacement, exercised directly against
// Overview rather than through the full App+panel round trip.
func TestOverviewHideDispatchBlanksBar(t *testing.T) {
	o := newOverview().WithSessions(rosterFixture())
	shown := o.ViewWithMenu(testkit.Width, testkit.Height, nil, 0, false)
	hidden := o.ViewWithMenu(testkit.Width, testkit.Height, nil, 0, true)

	if strings.Count(shown, "\n") != strings.Count(hidden, "\n") {
		t.Fatalf("hideDispatch changed the total row count: shown=%d hidden=%d rows",
			strings.Count(shown, "\n")+1, strings.Count(hidden, "\n")+1)
	}
	if !strings.Contains(shown, "describe a task for a new session") {
		t.Fatal("precondition failed: expected the dispatch placeholder with hideDispatch=false")
	}
	if strings.Contains(hidden, "describe a task for a new session") || strings.Contains(hidden, "space peek") {
		t.Errorf("hideDispatch=true still shows the dispatch bar/hint:\n%s", hidden)
	}
}

// TestOverviewSelectionByID verifies selection tracks a session across a view
// toggle even when its row index changes.
func TestOverviewSelectionByID(t *testing.T) {
	o := newOverview().WithSessions(rosterFixture())
	// Flat order is recency-first: row 0 is the 30s-old working session.
	o = o.MoveDown() // select the 2-minute working session (id ...001)
	want := o.SelectedID()
	if want != "0192a1b2-0000-7000-8000-000000000001" {
		t.Fatalf("unexpected selection after MoveDown: %q", want)
	}
	if got := o.ToggleView().SelectedID(); got != want {
		t.Errorf("selection not preserved across view toggle: got %q want %q", got, want)
	}
}

// TestOverviewDispatchSubmit verifies the dispatch bar hands off a typed
// prompt exactly once and clears itself.
func TestOverviewDispatchSubmit(t *testing.T) {
	o := newOverview().WithSessions(rosterFixture())
	for _, r := range "new task" {
		o = o.TypeRune(r)
	}
	o = o.Submit()
	got, ok := o.TakeSubmitted()
	if !ok || got != "new task" {
		t.Fatalf("TakeSubmitted = %q, %v; want %q, true", got, ok, "new task")
	}
	if _, ok := o.TakeSubmitted(); ok {
		t.Error("second TakeSubmitted returned a submission; want none")
	}
	if !o.InputEmpty() {
		t.Error("input buffer not cleared after Submit")
	}
}
