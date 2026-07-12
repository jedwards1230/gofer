package tui_test

import (
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// overviewNow is the fixed reference time the roster ages rows against, so
// humanAge output is deterministic across machines and CI.
var overviewNow = time.Date(2026, 7, 12, 18, 0, 0, 0, time.UTC)

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
			Updated: overviewNow.Add(-2 * time.Minute),
		},
		{
			ID:      "0192a1b2-0000-7000-8000-000000000002",
			Title:   "wire the websocket ACP listener",
			Summary: "blocked: approve Bash(kubectl delete pod)",
			Status:  tui.StatusWorking,
			Model:   "fable-5",
			Cost:    provider.Cost{USD: 0.0912},
			Pending: 2,
			Updated: overviewNow.Add(-30 * time.Second),
		},
		{
			ID:      "0192a1b2-0000-7000-8000-000000000003",
			Title:   "keycloak path-b groundwork",
			Summary: "turn finished — awaiting the next prompt",
			Status:  tui.StatusNeedsInput,
			Model:   "fable-5",
			Cost:    provider.Cost{USD: 0.1204},
			Updated: overviewNow.Add(-5 * time.Minute),
		},
		{
			ID:      "0192a1b2-0000-7000-8000-000000000004",
			Title:   "authentik token exchange rfc 8693",
			Summary: "Keycloak Path-B foundation complete and verified",
			Status:  tui.StatusFinished,
			Model:   "fable-5",
			Cost:    provider.Cost{USD: 1.4230},
			Updated: overviewNow.Add(-time.Hour),
		},
		{
			ID:      "0192a1b2-0000-7000-8000-000000000005",
			Title:   "openclaw dev setup",
			Summary: "Heartbeat revamp spec'd and handed off",
			Status:  tui.StatusFinished,
			Model:   "fable-5",
			Cost:    provider.Cost{USD: 0.0311},
			Updated: overviewNow.Add(-26 * time.Hour),
		},
	}
}

func newOverview() tui.Overview {
	return tui.NewOverview(theme.Test(), tui.OverviewMeta{
		App:     "gofer",
		Version: "0.2.0",
		Model:   "fable-5",
		Cwd:     "~/orchestration",
		Now:     overviewNow,
	})
}

// TestGoldenOverviewFlat renders the flat, recency-sorted roster with the
// first row selected.
func TestGoldenOverviewFlat(t *testing.T) {
	o := newOverview().WithSessions(rosterFixture())
	testkit.AssertGolden(t, "overview_flat", testkit.Render(o, testkit.Width, testkit.Height))
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
