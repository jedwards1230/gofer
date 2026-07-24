package tui

import (
	"testing"
	"time"

	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// multiCwdRoster is a flat roster spanning two working directories whose
// recency order (dirA, dirB, dirA, dirB) interleaves the two dirs, so the
// cwd-grouped render order (both dirA rows, then both dirB rows) genuinely
// differs from the raw [Overview.ordered] recency order. That gap is exactly
// Bug 1: navigation stepped the raw order while the roster drew the grouped
// one.
func multiCwdRoster() []SessionInfo {
	return []SessionInfo{
		{ID: "a1", Title: "dirA newest", Status: StatusWorking, Cwd: "/work/a", Updated: GoldenNow.Add(-1 * time.Minute)},
		{ID: "b1", Title: "dirB newer", Status: StatusWorking, Cwd: "/work/b", Updated: GoldenNow.Add(-2 * time.Minute)},
		{ID: "a2", Title: "dirA older", Status: StatusWorking, Cwd: "/work/a", Updated: GoldenNow.Add(-3 * time.Minute)},
		{ID: "b2", Title: "dirB oldest", Status: StatusWorking, Cwd: "/work/b", Updated: GoldenNow.Add(-4 * time.Minute)},
	}
}

// TestOverviewNavFollowsRenderedOrder pins Bug 1's contract: arrow-nav and the
// default selection walk the exact top-to-bottom order the roster renders — the
// cwd-grouped flat order — not the raw recency order.
func TestOverviewNavFollowsRenderedOrder(t *testing.T) {
	o := NewOverview(theme.Test(), GoldenMeta()).WithSessions(multiCwdRoster())

	// The rendered order groups the interleaved recency order by cwd.
	order := o.renderedOrder()
	gotIDs := make([]string, len(order))
	for i, s := range order {
		gotIDs[i] = s.ID
	}
	wantIDs := []string{"a1", "a2", "b1", "b2"}
	if len(gotIDs) != len(wantIDs) {
		t.Fatalf("renderedOrder length = %d; want %d (%v)", len(gotIDs), len(wantIDs), gotIDs)
	}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Fatalf("renderedOrder = %v; want %v (raw ordered() interleaves cwds)", gotIDs, wantIDs)
		}
	}

	// renderedOrder must equal what rows() actually draws top-to-bottom: setting
	// each session selected in renderedOrder sequence yields a strictly
	// increasing selected-line index. This is the invariant that ties nav to the
	// screen without parsing header/blank lines out of the frame.
	const width = 80
	prev := -1
	for _, s := range order {
		probe := o
		probe.selectedID = s.ID
		_, selLine := probe.rows(width)
		if selLine <= prev {
			t.Errorf("row for %s renders at line %d, not below the previous row's line %d — nav order and render order disagree", s.ID, selLine, prev)
		}
		prev = selLine
	}

	// Default selection is the first rendered row.
	if got := o.SelectedID(); got != wantIDs[0] {
		t.Errorf("default selection = %q; want first rendered row %q", got, wantIDs[0])
	}

	// MoveDown walks the rendered order top-to-bottom; the last row clamps.
	cur := o
	for i := 1; i < len(wantIDs); i++ {
		cur = cur.MoveDown()
		if got := cur.SelectedID(); got != wantIDs[i] {
			t.Fatalf("MoveDown #%d selected %q; want %q (rendered order %v)", i, got, wantIDs[i], wantIDs)
		}
	}
	if got := cur.MoveDown().SelectedID(); got != wantIDs[len(wantIDs)-1] {
		t.Errorf("MoveDown past the end selected %q; want it clamped at %q", got, wantIDs[len(wantIDs)-1])
	}

	// MoveUp reverses.
	for i := len(wantIDs) - 2; i >= 0; i-- {
		cur = cur.MoveUp()
		if got := cur.SelectedID(); got != wantIDs[i] {
			t.Fatalf("MoveUp back to index %d selected %q; want %q", i, got, wantIDs[i])
		}
	}
	if got := cur.MoveUp().SelectedID(); got != wantIDs[0] {
		t.Errorf("MoveUp past the top selected %q; want it clamped at %q", got, wantIDs[0])
	}
}
