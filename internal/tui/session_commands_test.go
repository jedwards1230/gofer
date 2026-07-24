package tui_test

// session_commands_test.go covers the session-lifecycle slash commands —
// /quit, /new, and /resume — end to end through App's exported Update/View
// surface, reusing app_test.go's fakeSup/press/type_/content helpers and
// command_test.go's dispatchSlash. The /resume picker's own rendering and
// navigation are covered at the component level in resumepicker_test.go
// (package tui).

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
)

// resumableRefs is the listing fakeSup answers /resume with: one session the
// roster does NOT hold (the offline case the picker exists for) and one it
// does (GoldenRoster's first row, the already-live case).
func resumableRefs() []tui.SessionRef {
	return []tui.SessionRef{
		{
			ID:      "0192a0c4-off0-7000-8000-000000000009",
			Title:   "an offline session",
			Cwd:     "/home/j/elsewhere",
			Updated: tui.GoldenNow.Add(-time.Hour),
		},
		{
			ID:      tui.GoldenRoster()[0].ID,
			Title:   tui.GoldenRoster()[0].Title,
			Cwd:     "/home/j/orchestration",
			Updated: tui.GoldenNow.Add(-2 * time.Minute),
		},
	}
}

// onAttachScreen reports whether frame is the attach screen rather than the
// roster overview: the two render different input prompts ("> " vs "❯ ", see
// model.go/overview_render.go), which is the only screen-identity signal the
// black-box tui_test package has.
func onAttachScreen(frame string) bool {
	return strings.Contains(frame, "> ▏") && !strings.Contains(frame, "❯ ")
}

// isQuit reports whether cmd is tea.Quit. bubbletea's Quit is a function
// value, so it is compared by identity through reflect rather than with ==
// (func values are not comparable).
func isQuit(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	return reflect.ValueOf(cmd).Pointer() == reflect.ValueOf(tea.Cmd(tea.Quit)).Pointer()
}

// TestQuitCommandQuits pins /quit (and its /exit alias) returning exactly
// tea.Quit — the same one line ctrl-c is bound to on every screen, with no
// teardown of its own (app.go/panel.go/dialog.go all just return tea.Quit).
func TestQuitCommandQuits(t *testing.T) {
	for _, name := range []string{"/quit", "/exit"} {
		t.Run(name, func(t *testing.T) {
			m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
			m, cmd := dispatchSlashCmd(t, m, name)
			if !isQuit(cmd) {
				t.Fatalf("%s returned %T, want tea.Quit", name, cmd)
			}
			if strings.Contains(content(m), "[Status]") {
				t.Errorf("%s opened the command panel; it must just quit:\n%s", name, content(m))
			}
		})
	}
}

// TestQuitMatchesCtrlC is the equivalence check behind /quit staying trivial:
// the command and the key must produce the same Cmd, so a teardown added to
// one is visibly missing from the other.
func TestQuitMatchesCtrlC(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	_, keyCmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	m2 := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	_, cmdCmd := dispatchSlashCmd(t, m2, "/quit")

	if !isQuit(keyCmd) {
		t.Fatal("ctrl-c no longer returns tea.Quit; this test's premise is stale")
	}
	if reflect.ValueOf(keyCmd).Pointer() != reflect.ValueOf(cmdCmd).Pointer() {
		t.Error("/quit and ctrl-c returned different Cmds — one of the two grew a teardown the other skips")
	}
}

// TestNewCommandCreatesSession pins /new going through the SAME Create seam a
// prompt typed into the dispatch bar takes, with an empty prompt (an idle
// session with no first turn), and attaching into the result.
func TestNewCommandCreatesSession(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := newTestApp(t, sup)
	m, cmd := dispatchSlashCmd(t, m, "/new")
	m = runCmd(t, m, cmd)

	if got := sup.createdPrompts(); !reflect.DeepEqual(got, []string{""}) {
		t.Fatalf("Create prompts = %q, want exactly one empty-prompt create", got)
	}
	if got := content(m); !onAttachScreen(got) {
		t.Errorf("expected /new to attach into the new session, got:\n%s", got)
	}
}

// TestNewLeavesPriorSessionAlone is the invariant-4 guard: /new must start a
// SECOND session, never interrupt, kill, archive, or otherwise touch the one
// the user was attached to. It asserts on fakeSup.ops, which records every
// Interrupt/Kill/Archive/SetModel/Resume the app issues.
func TestNewLeavesPriorSessionAlone(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := newTestApp(t, sup)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // attach the selected session

	m, cmd := dispatchSlashCmd(t, m, "/new")
	_ = runCmd(t, m, cmd)

	if ops := sup.recordedOps(); len(ops) != 0 {
		t.Errorf("/new issued %q against the existing session; it must leave it entirely alone", ops)
	}
	if sent := sup.sentPrompts(); len(sent) != 0 {
		t.Errorf("/new sent %q to the existing session; it must create a new one instead", sent)
	}
}

// TestNewRejectsArguments pins the deliberate no-argument contract (see
// runNew's doc): a prompt typed after /new is REPORTED, never silently
// discarded, and no session is created from the mistake.
func TestNewRejectsArguments(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := newTestApp(t, sup)
	m, cmd := dispatchSlashCmd(t, m, "/new fix the flaky test")
	m = runCmd(t, m, cmd)

	if got := sup.createdPrompts(); len(got) != 0 {
		t.Errorf("/new with arguments created %q; it must report the mistake instead", got)
	}
	if got := content(m); !strings.Contains(got, "/new takes no arguments") {
		t.Errorf("expected a status note explaining /new takes no arguments, got:\n%s", got)
	}
}

// TestResumeBareOpensPicker pins the bare form: the panel opens on the Resume
// tab, the listing is fetched, and nothing is resumed yet.
func TestResumeBareOpensPicker(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	sup.listed = resumableRefs()
	m := newTestApp(t, sup)
	m, cmd := dispatchSlashCmd(t, m, "/resume")
	if cmd == nil {
		t.Fatal("/resume returned no Cmd; the session listing was never fetched")
	}
	m = runCmd(t, m, cmd)

	got := content(m)
	if !strings.Contains(got, "[Resume]") {
		t.Fatalf("expected the panel open on the Resume tab, got:\n%s", got)
	}
	if !strings.Contains(got, "an offline session") {
		t.Errorf("expected the fetched listing rendered in the picker, got:\n%s", got)
	}
	if ops := sup.recordedOps(); len(ops) != 0 {
		t.Errorf("opening the picker issued %q; it must only list, never resume", ops)
	}
}

// TestGoldenPanelResume pins the whole frame with the picker open and its
// listing applied — the App-level composition the component goldens don't see.
func TestGoldenPanelResume(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	sup.listed = resumableRefs()
	m := newTestApp(t, sup)
	m, cmd := dispatchSlashCmd(t, m, "/resume")
	m = runCmd(t, m, cmd)
	testkit.AssertGolden(t, "app_panel_resume", content(m))
}

// TestResumeWithIDResumesDirectly is the direct path: `/resume <id>` must
// reach Supervisor.Resume with that id and NOT open the picker.
func TestResumeWithIDResumesDirectly(t *testing.T) {
	const id = "0192a0c4-off0-7000-8000-000000000009"
	sup := newFakeSup(tui.GoldenRoster())
	m := newTestApp(t, sup)
	m, cmd := dispatchSlashCmd(t, m, "/resume "+id)
	if strings.Contains(content(m), "[Resume]") {
		t.Fatalf("/resume <id> opened the picker; it must apply the id directly:\n%s", content(m))
	}
	m = runCmd(t, m, cmd)

	ops := sup.recordedOps()
	if len(ops) != 1 || !strings.HasPrefix(ops[0], "resume:"+id+":") {
		t.Fatalf("ops = %q, want exactly one resume of %s", ops, id)
	}
	if !strings.HasSuffix(ops[0], ":"+tui.GoldenMeta().Cwd) {
		t.Errorf("ops = %q, want the client's cwd (%s) forwarded to Resume", ops, tui.GoldenMeta().Cwd)
	}
	if got := content(m); !onAttachScreen(got) {
		t.Errorf("expected a successful /resume to attach into the session, got:\n%s", got)
	}
}

// TestResumeUnknownIDReportsError pins that a resume the backend refuses lands
// on the status line as a danger note rather than failing silently.
func TestResumeUnknownIDReportsError(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	sup.resumeErr = errors.New("supervisor: resume 0192dead: no such session")
	m := newTestApp(t, sup)
	m, cmd := dispatchSlashCmd(t, m, "/resume 0192dead-0000-7000-8000-000000000000")
	m = runCmd(t, m, cmd)

	if got := content(m); !strings.Contains(got, "no such session") {
		t.Errorf("expected the resume failure on the status line, got:\n%s", got)
	}
}

// TestResumeMalformedIDIsRejected pins the client-side shape check: an id that
// could never name a journal file is refused by name, before any op is issued.
func TestResumeMalformedIDIsRejected(t *testing.T) {
	for _, id := range []string{"../escape", "sessions/abc", "."} {
		t.Run(id, func(t *testing.T) {
			sup := newFakeSup(tui.GoldenRoster())
			m := newTestApp(t, sup)
			m, cmd := dispatchSlashCmd(t, m, "/resume "+id)
			m = runCmd(t, m, cmd)

			if ops := sup.recordedOps(); len(ops) != 0 {
				t.Errorf("ops = %q, want none — a malformed id must never reach the supervisor", ops)
			}
			got := content(m)
			if !strings.Contains(got, "not a valid session id") || !strings.Contains(got, id) {
				t.Errorf("expected a danger note naming %q, got:\n%s", id, got)
			}
		})
	}
}

// TestResumeTooManyArgumentsIsRejected mirrors /model's multi-argument rule: a
// session id contains no whitespace, so two tokens is always a mistake.
func TestResumeTooManyArgumentsIsRejected(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := newTestApp(t, sup)
	m, cmd := dispatchSlashCmd(t, m, "/resume one two")
	m = runCmd(t, m, cmd)

	if ops := sup.recordedOps(); len(ops) != 0 {
		t.Errorf("ops = %q, want none", ops)
	}
	if got := content(m); !strings.Contains(got, "single session id") {
		t.Errorf("expected a danger note about the argument count, got:\n%s", got)
	}
}

// TestResumeAlreadyLiveSessionSkipsTheOp pins the redundant-load guard: a
// session the roster already holds is live, so /resume just attaches. Issuing
// session/load again would replay its whole history onto the reconstruction
// broker a second time and double the attach transcript.
func TestResumeAlreadyLiveSessionSkipsTheOp(t *testing.T) {
	live := tui.GoldenRoster()[0].ID
	sup := newFakeSup(tui.GoldenRoster())
	m := newTestApp(t, sup)
	m, cmd := dispatchSlashCmd(t, m, "/resume "+live)
	m = runCmd(t, m, cmd)

	if ops := sup.recordedOps(); len(ops) != 0 {
		t.Errorf("ops = %q, want none — an already-live session needs no resume", ops)
	}
	if got := content(m); !onAttachScreen(got) {
		t.Errorf("expected /resume of a live session to attach into it, got:\n%s", got)
	}
}

// TestResumePickerEnterResumes drives the picker's Enter: ↓ onto the offline
// row, Enter, and the op must carry that row's id and its OWN cwd — not this
// client's, which is what makes resuming a session from another project land
// in the right directory.
func TestResumePickerEnterResumes(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	sup.listed = resumableRefs()
	m := newTestApp(t, sup)
	m, cmd := dispatchSlashCmd(t, m, "/resume")
	m = runCmd(t, m, cmd)

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyDown}) // the offline row (newest-first: offline is 1h, live is 2m)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	m, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = runCmd(t, m, cmd)

	ops := sup.recordedOps()
	want := "resume:0192a0c4-off0-7000-8000-000000000009:/home/j/elsewhere"
	if len(ops) != 1 || ops[0] != want {
		t.Fatalf("ops = %q, want [%s]", ops, want)
	}
	if strings.Contains(content(m), "[Resume]") {
		t.Errorf("the panel stayed open after Enter; resuming is a committing action:\n%s", content(m))
	}
}

// TestResumePickerEnterWithNoSelectionIsNoOp pins Enter's no-op state: with no
// row highlighted the panel stays open and nothing is resumed, matching the
// Model tab's contract for the same state.
func TestResumePickerEnterWithNoSelectionIsNoOp(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	sup.listed = resumableRefs()
	m := newTestApp(t, sup)
	m, cmd := dispatchSlashCmd(t, m, "/resume")
	m = runCmd(t, m, cmd)

	m, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = runCmd(t, m, cmd)

	if ops := sup.recordedOps(); len(ops) != 0 {
		t.Errorf("ops = %q, want none with nothing highlighted", ops)
	}
	if !strings.Contains(content(m), "[Resume]") {
		t.Errorf("the panel closed on a no-op Enter; it must stay open:\n%s", content(m))
	}
}

// TestResumePickerReportsListingFailure pins the failed-listing path reaching
// the panel: the reason is rendered in the picker, not swallowed into an empty
// list that would read as "you have no sessions".
func TestResumePickerReportsListingFailure(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	sup.listErr = errors.New("connection refused")
	m := newTestApp(t, sup)
	m, cmd := dispatchSlashCmd(t, m, "/resume")
	m = runCmd(t, m, cmd)

	got := content(m)
	if !strings.Contains(got, "Couldn't list sessions") || !strings.Contains(got, "connection refused") {
		t.Errorf("expected the listing failure rendered in the picker, got:\n%s", got)
	}
	if strings.Contains(got, "No sessions on disk yet") {
		t.Errorf("a failed listing rendered as an empty store — the two states must stay distinct:\n%s", got)
	}
}

// TestResumeTabInFetchesOnce pins the tab-in fetch rule the Model tab also
// follows: tabbing across to Resume lists once, and bouncing away and back
// does not list again.
func TestResumeTabInFetchesOnce(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	sup.listed = resumableRefs()
	m := newTestApp(t, sup)
	m, cmd := dispatchSlashCmd(t, m, "/status")
	m = runCmd(t, m, cmd)

	// → until the Resume tab is active, rather than a fixed press count: the
	// tab bar grows as commands land, and how many presses it takes is not what
	// this test is about. The bound is generous but finite so a Resume tab that
	// somehow became unreachable fails here instead of spinning (←/→ wrap, so
	// any bound past the tab count would loop forever otherwise).
	var reached bool
	for range 16 {
		if strings.Contains(content(m), "[Resume]") {
			reached = true
			break
		}
		m, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
		m = runCmd(t, m, cmd)
	}
	if !reached {
		t.Fatalf("→ never reached the Resume tab:\n%s", content(m))
	}
	if got := content(m); !strings.Contains(got, "an offline session") {
		t.Fatalf("expected tabbing into Resume to fetch the listing, got:\n%s", got)
	}
	if got := sup.listCalls(); got != 1 {
		t.Fatalf("ListSessions called %d times, want 1", got)
	}

	m, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	m = runCmd(t, m, cmd)
	m, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	_ = runCmd(t, m, cmd)
	if got := sup.listCalls(); got != 1 {
		t.Errorf("ListSessions called %d times after a tab bounce, want 1 — an answered listing must not re-fetch", got)
	}
}
