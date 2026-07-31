package tui

// roster_focus_test.go pins the ctrl-x delete focus contract: confirming a
// delete must land selection on the row that took the deleted session's
// place — its rendered successor — rather than resetting to the top of the
// roster (the bug: deleting mid-list bounced focus back to row 0, making a
// quick run of deletes require re-navigating from the top every time). Lives
// in package tui (not tui_test) because it drives a real rosterMsg{...}
// round trip to simulate the poll that lands AFTER the delete — the same
// technique app_internal_test.go's newAppForGolden uses — which needs the
// unexported message type.

import (
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// deleteFocusRoster returns three idle sessions sharing one cwd, most
// recently updated first (A, B, C) — the flat view's default order with no
// cwd grouping to complicate it. Idle (not Working) routes confirmDestroy's
// second press to Archive, matching TestNavCtrlXArchivesIdleSession's
// established status pick in app_test.go.
func deleteFocusRoster() []SessionInfo {
	now := time.Now()
	return []SessionInfo{
		{ID: "sess-a", Title: "A", Status: StatusIdle, Cwd: "/proj", Updated: now},
		{ID: "sess-b", Title: "B", Status: StatusIdle, Cwd: "/proj", Updated: now.Add(-1 * time.Minute)},
		{ID: "sess-c", Title: "C", Status: StatusIdle, Cwd: "/proj", Updated: now.Add(-2 * time.Minute)},
	}
}

// ctrlXFocus is the ctrl+x key both presses of the two-press confirm send.
var ctrlXFocus = tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl}

// pressAndRun drives one key (or any tea.Msg) through Update and, if a Cmd
// comes back, runs it immediately and folds the resulting Msg back in — this
// file's synchronous stand-in for bubbletea's own runtime loop. Mirrors
// app_test.go's press helper, which lives in the external tui_test package
// and so is not reachable from here.
func pressAndRun(t *testing.T, a App, msg tea.Msg) App {
	t.Helper()
	mdl, cmd := a.Update(msg)
	a = mdl.(App)
	if cmd == nil {
		return a
	}
	mdl, _ = a.Update(cmd())
	return mdl.(App)
}

// newFocusTestApp builds an App over a fresh internalFakeSup, sized and with
// sessions seeded via a real Update(rosterMsg{...}) round trip — the same
// shape newAppForGolden uses, parameterized on the roster instead of always
// seeding GoldenRoster().
func newFocusTestApp(t *testing.T, sessions []SessionInfo) App {
	t.Helper()
	sup := newInternalFakeSup(sessions)
	a := NewApp(theme.Test(), sup, GoldenMeta(), GoldenCommandEnv())

	mdl, _ := a.Update(tea.WindowSizeMsg{Width: testkit.Width, Height: testkit.Height})
	a = mdl.(App)

	mdl, _ = a.Update(rosterMsg{sessions: sessions})
	return mdl.(App)
}

// selectID walks MoveDown until id is selected, failing the test if it never
// shows up within the roster's own length.
func selectID(t *testing.T, a App, id string) App {
	t.Helper()
	for i := 0; i <= len(a.over.sessions); i++ {
		if a.over.SelectedID() == id {
			return a
		}
		a = pressAndRun(t, a, tea.KeyPressMsg{Code: tea.KeyDown})
	}
	t.Fatalf("selectID: %q never became selected (stuck at %q)", id, a.over.SelectedID())
	return a
}

// TestCtrlXDeleteFocusesSuccessorMidList is the mutation anchor for this fix:
// deleting the MIDDLE row of a three-row roster must land selection on the
// row that follows it, not on the top of the list. Reverting
// [App.confirmDestroy]'s pendingSelect capture (or the rosterMsg handler's
// use of it) turns this red — selection lands on sess-a (the top row)
// instead of sess-c.
func TestCtrlXDeleteFocusesSuccessorMidList(t *testing.T) {
	roster := deleteFocusRoster() // A, B, C
	a := newFocusTestApp(t, roster)

	a = selectID(t, a, "sess-b")

	a = pressAndRun(t, a, ctrlXFocus) // arm
	a = pressAndRun(t, a, ctrlXFocus) // confirm: archives sess-b, captures its successor

	if got := a.pendingSelect; got != "sess-c" {
		t.Fatalf("pendingSelect = %q after confirming delete on the mid-list row; want sess-c (the row below it)", got)
	}

	// Simulate the roster poll landing after the delete took effect: B is gone.
	after := []SessionInfo{roster[0], roster[2]} // A, C
	mdl, _ := a.Update(rosterMsg{sessions: after})
	a = mdl.(App)

	if got := a.over.SelectedID(); got != "sess-c" {
		t.Fatalf("selected = %q after deleting the mid-list row; want sess-c (the successor) — a reset to the top would select sess-a instead", got)
	}
}

// TestCtrlXDeleteLastSessionFocusesNewLastRow verifies deleting the LAST row
// focuses the new last row (its predecessor), not the top of the list.
func TestCtrlXDeleteLastSessionFocusesNewLastRow(t *testing.T) {
	roster := deleteFocusRoster() // A, B, C
	a := newFocusTestApp(t, roster)

	a = selectID(t, a, "sess-c") // last row

	a = pressAndRun(t, a, ctrlXFocus)
	a = pressAndRun(t, a, ctrlXFocus)

	if got := a.pendingSelect; got != "sess-b" {
		t.Fatalf("pendingSelect = %q after confirming delete on the last row; want sess-b (the new last row)", got)
	}

	after := []SessionInfo{roster[0], roster[1]} // A, B (C gone)
	mdl, _ := a.Update(rosterMsg{sessions: after})
	a = mdl.(App)

	if got := a.over.SelectedID(); got != "sess-b" {
		t.Fatalf("selected = %q after deleting the last row; want sess-b (the new last row), not sess-a (the top)", got)
	}
}

// TestCtrlXDeleteLoneCwdGroupFocusesNextSession verifies deleting the only
// session in its cwd group — which removes that group's header along with
// the row — lands on the next real session row, not a header or the top.
func TestCtrlXDeleteLoneCwdGroupFocusesNextSession(t *testing.T) {
	now := time.Now()
	roster := []SessionInfo{
		{ID: "sess-a", Title: "A", Status: StatusIdle, Cwd: "/one", Updated: now},
		{ID: "sess-b", Title: "B", Status: StatusIdle, Cwd: "/two", Updated: now.Add(-1 * time.Minute)}, // sole occupant of /two
		{ID: "sess-c", Title: "C", Status: StatusIdle, Cwd: "/three", Updated: now.Add(-2 * time.Minute)},
	}
	a := newFocusTestApp(t, roster)

	a = selectID(t, a, "sess-b")

	a = pressAndRun(t, a, ctrlXFocus)
	a = pressAndRun(t, a, ctrlXFocus)

	if got := a.pendingSelect; got != "sess-c" {
		t.Fatalf("pendingSelect = %q after deleting the sole session of a cwd group; want sess-c", got)
	}

	after := []SessionInfo{roster[0], roster[2]} // A, C — /two's group is gone entirely
	mdl, _ := a.Update(rosterMsg{sessions: after})
	a = mdl.(App)

	if got := a.over.SelectedID(); got != "sess-c" {
		t.Fatalf("selected = %q after deleting the sole session of its cwd group; want sess-c (the next session row), not a header or the top", got)
	}
}

// TestCtrlXDeleteOnlySessionLeavesEmptyRosterNoCrash verifies deleting the
// only session in the whole roster leaves an empty, non-crashing selection.
func TestCtrlXDeleteOnlySessionLeavesEmptyRosterNoCrash(t *testing.T) {
	roster := []SessionInfo{
		{ID: "sess-a", Title: "A", Status: StatusIdle, Cwd: "/proj", Updated: time.Now()},
	}
	a := newFocusTestApp(t, roster)

	a = pressAndRun(t, a, ctrlXFocus)
	a = pressAndRun(t, a, ctrlXFocus)

	if got := a.pendingSelect; got != "" {
		t.Fatalf("pendingSelect = %q after deleting the roster's only session; want empty (no successor)", got)
	}

	mdl, _ := a.Update(rosterMsg{sessions: nil})
	a = mdl.(App)

	if got := a.over.SelectedID(); got != "" {
		t.Fatalf("selected = %q against an empty roster; want no selection", got)
	}
	if _, ok := a.over.Selected(); ok {
		t.Fatal("Selected() reports ok=true against an empty roster")
	}
	// A render must not panic against the now-empty overview.
	_ = a.render()
}
