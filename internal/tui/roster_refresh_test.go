package tui

// roster_refresh_test.go pins gofer#322: a roster-MUTATING op (kill, kill-tree,
// archive) must make the roster reflect the mutation as soon as the op lands,
// rather than leaving the stale row on screen until the next poll tick.
//
// These tests are in package tui because the property lives entirely in
// unexported messages — opDoneMsg.refreshRoster and rosterMsg.adhoc — which
// package tui_test cannot construct or inspect.
//
// NOTHING here sleeps or advances a clock. The whole point of the bug was a
// wait of up to rosterInterval, so a test that tolerates any wait cannot
// distinguish the fix from the bug. Every assertion is on the message chain:
// the op resolves, its opDoneMsg yields a Cmd, and that Cmd yields the fresh
// roster. If the refetch is not there, the chain simply ends and the assertion
// fires immediately.

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// The two op failures these tests inject. They are distinct values so an
// assertion on the status line proves WHICH op's error reached it.
var (
	errKillRefused    = errors.New("kill refused: session is draining")
	errArchiveRefused = errors.New("archive refused: journal is locked")
)

// mutatingSup is internalFakeSup with Kill/Archive that actually change the
// roster, which the shared fake's no-op versions do not. That matters: against
// a fake whose roster never changes, a "the row is gone after the refetch"
// assertion can only ever fail, and a "a refetch happened" assertion would pass
// against a refetch of an unchanged roster. Here the fake's roster is the
// oracle — the refetched snapshot differs from the one on screen exactly when
// the op did something.
//
// killErr/archiveErr make the op fail while still recording the attempt, for
// the partial-failure case doKillTree is built around.
type mutatingSup struct {
	*internalFakeSup

	killed     []string
	archived   []string
	killErr    error
	archiveErr error
}

func (f *mutatingSup) Kill(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killed = append(f.killed, id)
	if f.killErr != nil {
		return f.killErr
	}
	f.roster = withoutSession(f.roster, id)
	return nil
}

func (f *mutatingSup) Archive(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.archived = append(f.archived, id)
	if f.archiveErr != nil {
		return f.archiveErr
	}
	f.roster = withoutSession(f.roster, id)
	return nil
}

func withoutSession(in []SessionInfo, id string) []SessionInfo {
	out := make([]SessionInfo, 0, len(in))
	for _, s := range in {
		if s.ID != id {
			out = append(out, s)
		}
	}
	return out
}

func newMutatingApp(t *testing.T, roster []SessionInfo) (App, *mutatingSup) {
	t.Helper()
	sup := &mutatingSup{internalFakeSup: newInternalFakeSup(roster)}
	a := NewApp(theme.Test(), sup, GoldenMeta(), GoldenCommandEnv())
	mdl, _ := a.Update(tea.WindowSizeMsg{Width: testkit.Width, Height: testkit.Height})
	a = mdl.(App)
	mdl, _ = a.Update(rosterMsg{sessions: roster})
	return mdl.(App), sup
}

// runRosterRefresh drives one roster-mutating op to completion: it runs the op
// Cmd, feeds the resulting opDoneMsg back through Update, and runs whatever
// Cmd that produces. It returns the App after the refreshed roster (if any)
// has been applied, plus whether a refetch actually happened.
//
// The "if any" is the assertion surface: before the fix, Update's opDoneMsg
// case returned a nil Cmd, so refetched is false and the App still carries the
// pre-op roster.
func runRosterRefresh(t *testing.T, a App, op tea.Cmd) (App, bool) {
	t.Helper()
	if op == nil {
		t.Fatal("no op Cmd to run — the key press dispatched nothing")
	}
	done, ok := op().(opDoneMsg)
	if !ok {
		t.Fatalf("op Cmd produced %T, want opDoneMsg", op())
	}
	mdl, cmd := a.Update(done)
	a = mdl.(App)
	if cmd == nil {
		return a, false
	}
	msg := cmd()
	rm, ok := msg.(rosterMsg)
	if !ok {
		t.Fatalf("the Cmd opDoneMsg returned produced %T, want rosterMsg", msg)
	}
	if !rm.adhoc {
		t.Error("the post-op roster refetch is not marked adhoc, so applying it will re-arm the poll tick and leave a second poll chain running forever")
	}
	mdl, tick := a.Update(rm)
	if tick != nil {
		t.Error("applying the post-op roster snapshot re-armed the poll tick — that is a second, parallel poll chain per destructive action")
	}
	return mdl.(App), true
}

// TestArchiveRefreshesRosterImmediately is gofer#322's headline case: the
// reported symptom was an archived row lingering for up to a second after the
// confirming ctrl+x.
func TestArchiveRefreshesRosterImmediately(t *testing.T) {
	roster := GoldenRoster()
	// Row 2 is StatusNeedsInput; confirmDestroy routes Finished/Idle to archive,
	// so force the selected row to Idle to reach doArchive rather than doKill.
	roster[0].Status = StatusIdle
	target := roster[0].ID

	a, sup := newMutatingApp(t, roster)
	a = pressCtrlX(t, a) // arm
	mdl, op := a.Update(ctrlX())
	a = mdl.(App)

	a, refetched := runRosterRefresh(t, a, op)
	if !refetched {
		t.Fatal("archive landed and the roster was NOT refetched: the archived row stays on screen until the next rosterTickMsg, up to rosterInterval later — gofer#322")
	}
	if len(sup.archived) != 1 || sup.archived[0] != target {
		t.Fatalf("Archive calls = %v, want exactly [%s]", sup.archived, target)
	}
	if rosterHas(a, target) {
		t.Errorf("session %s is still on the rendered roster after its archive landed", target)
	}
}

// TestKillRefreshesRosterImmediately covers the sibling op the issue asked to
// be fixed consistently: doKill had the identical shape and the identical bug.
func TestKillRefreshesRosterImmediately(t *testing.T) {
	roster := GoldenRoster()
	roster[0].Status = StatusWorking // Working routes to doKill, not doArchive
	target := roster[0].ID

	a, sup := newMutatingApp(t, roster)
	a = pressCtrlX(t, a)
	mdl, op := a.Update(ctrlX())
	a = mdl.(App)

	a, refetched := runRosterRefresh(t, a, op)
	if !refetched {
		t.Fatal("kill landed and the roster was NOT refetched — gofer#322")
	}
	if len(sup.killed) != 1 || sup.killed[0] != target {
		t.Fatalf("Kill calls = %v, want exactly [%s]", sup.killed, target)
	}
	if rosterHas(a, target) {
		t.Errorf("session %s is still on the rendered roster after its kill landed", target)
	}
}

// TestKillTreeRefreshesRosterImmediately covers the third op. It is driven
// through doKillTree directly rather than through ctrl+t, because ctrl+t needs
// a parent/child roster to have any descendants at all and the property under
// test is the refresh, not the descendant walk (overview_test.go owns that).
func TestKillTreeRefreshesRosterImmediately(t *testing.T) {
	roster := GoldenRoster()
	ids := []string{roster[0].ID, roster[1].ID}

	a, sup := newMutatingApp(t, roster)

	a, refetched := runRosterRefresh(t, a, a.doKillTree(ids))
	if !refetched {
		t.Fatal("kill-tree landed and the roster was NOT refetched — gofer#322")
	}
	if len(sup.killed) != 2 {
		t.Fatalf("Kill calls = %v, want one per id", sup.killed)
	}
	for _, id := range ids {
		if rosterHas(a, id) {
			t.Errorf("session %s is still on the rendered roster after the kill-tree landed", id)
		}
	}
}

// TestKillTreePartialFailureStillRefreshes is the case that decides WHY the
// refetch is not gated on err == nil. doKillTree attempts every id and reports
// only the FIRST error, so a run that returns an error can still have killed
// rows. Skipping the refetch on the error path would leave exactly those rows
// stale — the original bug, surviving in the failure case.
//
// Here every Kill fails, so nothing changes on the roster; what is asserted is
// that the refetch happens ANYWAY, which is the behaviour that covers the
// partial case without the App having to guess which ids succeeded.
func TestKillTreePartialFailureStillRefreshes(t *testing.T) {
	roster := GoldenRoster()
	a, sup := newMutatingApp(t, roster)
	sup.killErr = errKillRefused

	_, refetched := runRosterRefresh(t, a, a.doKillTree([]string{roster[0].ID}))
	if !refetched {
		t.Fatal("a failing kill-tree did not refetch the roster; a PARTIAL failure would leave the rows it did kill rendering stale")
	}
}

// TestFailedArchiveLeavesRowAndSaysWhy is the issue's other definition-of-done
// bullet: a failed op must not leave the UI asserting something that did not
// happen. The refetch runs, the row is still there because the daemon still has
// it, and the error is on the status line.
func TestFailedArchiveLeavesRowAndSaysWhy(t *testing.T) {
	roster := GoldenRoster()
	roster[0].Status = StatusIdle
	target := roster[0].ID

	a, sup := newMutatingApp(t, roster)
	sup.archiveErr = errArchiveRefused

	a = pressCtrlX(t, a)
	mdl, op := a.Update(ctrlX())
	a = mdl.(App)

	a, _ = runRosterRefresh(t, a, op)
	if !rosterHas(a, target) {
		t.Errorf("session %s vanished from the roster even though its archive FAILED", target)
	}
	if a.status != errArchiveRefused.Error() {
		t.Errorf("status = %q, want the archive error %q", a.status, errArchiveRefused.Error())
	}
	if a.pendingSelect != "" {
		t.Errorf("pendingSelect = %q after a failed op, want it cleared", a.pendingSelect)
	}
}

// TestPollSnapshotRearmsTickAdhocDoesNot is the positive/negative control for
// the single-chain property runRosterRefresh asserts on the way past. Both
// halves are needed: without the first, "returns no Cmd" would also pass
// against a rosterMsg handler that had stopped ticking altogether, which is the
// opposite bug (the roster would never refresh on its own again).
func TestPollSnapshotRearmsTickAdhocDoesNot(t *testing.T) {
	a, _ := newMutatingApp(t, GoldenRoster())

	if _, cmd := a.Update(rosterMsg{sessions: GoldenRoster()}); cmd == nil {
		t.Error("a poll-origin roster snapshot did NOT re-arm the tick — the 1s roster poll has stopped")
	}
	if _, cmd := a.Update(rosterMsg{sessions: GoldenRoster(), adhoc: true}); cmd != nil {
		t.Error("an out-of-band roster snapshot re-armed the tick — every one of these adds a parallel poll chain that never stops")
	}
}

// rosterHas reports whether id is on the roster the overview is currently
// rendering. It reads the overview's own session list rather than scanning
// rendered text: a row can be off the visible window at these sizes, and a
// window miss would read as a successful delete.
func rosterHas(a App, id string) bool {
	for _, s := range a.over.sessions {
		if s.ID == id {
			return true
		}
	}
	return false
}

func ctrlX() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: 'x', Mod: tea.ModCtrl}
}

// pressCtrlX sends the ARMING press and asserts it armed rather than acted —
// otherwise a change that made the first press destructive would still leave
// these tests green, having simply run the op one press early.
func pressCtrlX(t *testing.T, a App) App {
	t.Helper()
	mdl, cmd := a.Update(ctrlX())
	if cmd != nil {
		t.Fatal("the FIRST ctrl+x dispatched a Cmd — it must only arm the confirm")
	}
	a = mdl.(App)
	if a.ctrlXArmed == "" {
		t.Fatal("the first ctrl+x did not arm the confirm")
	}
	return a
}
