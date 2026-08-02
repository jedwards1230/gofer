package tui_test

// cwdprompt_test.go drives the "this session's recorded directory is gone"
// three-way prompt (jedwards1230/gofer#326) entirely through App's exported
// tea.Model surface, over a Supervisor that also implements the cwd-missing
// notifier — so the registration seam, the background→Update-loop hand-off, the
// prompt's state machine, and the Supervisor calls each branch does (or, for
// cancel, does NOT) make are all exercised the way they actually ship.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// missingCwd is the recorded directory the tests below report as gone. It is a
// path no test machine has, so nothing here can accidentally succeed against a
// real directory.
const missingCwd = "/home/j/projects/deleted-by-a-cleanup"

// notifyingSup is a [fakeSup] that ALSO implements the cwd-missing notifier
// seam App type-asserts against (an in-process supervisor deliberately does
// not — see cwdprompt.go's cwdMissingNotifier). Embedding rather than
// reimplementing keeps the op recording (fakeSup.ops, "resume:<id>:<cwd>") that
// the mutates-nothing assertions read.
type notifyingSup struct {
	*fakeSup

	mu sync.Mutex
	fn func(sessionID, cwd string)
	// subs counts Subscribe calls. It is the observable behind "the aborted
	// attach really was torn down": [App.enter] only re-subscribes for a session
	// it is not already subscribed to, so a second Enter that subscribes again
	// is a second attach ATTEMPT, and one that does not is the silent no-op.
	subs int
}

func newNotifyingSup(roster []tui.SessionInfo) *notifyingSup {
	return &notifyingSup{fakeSup: newFakeSup(roster)}
}

func (s *notifyingSup) OnSessionCwdMissing(fn func(sessionID, cwd string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fn = fn
}

// Subscribe counts the call and delegates to the embedded fake's real broker
// subscription, so the App's stream handling is unchanged by the counting.
func (s *notifyingSup) Subscribe(ctx context.Context, id string) (*event.Subscription, error) {
	s.mu.Lock()
	s.subs++
	s.mu.Unlock()
	return s.fakeSup.Subscribe(ctx, id)
}

func (s *notifyingSup) subscribes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subs
}

// fire raises one signal from a BACKGROUND goroutine, exactly as the real
// bridge does (the reconstruction core's per-session load goroutine, or
// whichever goroutine called Resume). Doing it off the test goroutine is what
// makes `go test -race ./...` able to catch a handler that touches App state
// directly instead of posting a message.
func (s *notifyingSup) fire(sessionID, cwd string) {
	s.mu.Lock()
	fn := s.fn
	s.mu.Unlock()
	if fn == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(sessionID, cwd)
	}()
	<-done
}

// initCwdApp builds an App over a notifying Supervisor and drives Init to
// completion, returning the model plus the channel the still-parked cwd-missing
// listener will deliver on.
//
// Init BATCHES the roster fetch with that listener, so this expands
// tea.BatchMsg explicitly. A one-Cmd helper (newTestApp) would swallow the
// batch and every assertion in this file would go vacuously green rather than
// red — the exact trap docs/TESTING.md documents — so the batch shape is
// asserted here rather than assumed.
func initCwdApp(t *testing.T, sup tui.Supervisor) (tea.Model, <-chan tea.Msg) {
	t.Helper()
	var m tea.Model = tui.NewApp(theme.Test(), sup, tui.GoldenMeta(), tui.GoldenCommandEnv())
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init() returned a nil Cmd; expected the roster fetch batched with the cwd-missing listener")
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("Init() did not batch: %T. Against a notifying Supervisor it must return the roster "+
			"fetch AND the cwd-missing listener, or the prompt can never open.", cmd())
	}
	if len(batch) != 2 {
		t.Fatalf("Init() batched %d commands, want 2 (roster fetch + cwd-missing listener)", len(batch))
	}

	msgs := make(chan tea.Msg, len(batch))
	for _, c := range batch {
		go func(c tea.Cmd) { msgs <- c() }(c)
	}
	// The roster fetch resolves at once against the fake; the listener parks
	// until something fires. So the first message out is the roster, and the
	// channel is left holding the listener for [deliverCwdSignal].
	select {
	case msg := <-msgs:
		m, _ = m.Update(msg)
	case <-time.After(5 * time.Second):
		t.Fatal("the roster fetch never resolved")
	}
	return m, msgs
}

// deliverCwdSignal waits for the parked listener to produce its message and
// feeds it into Update — the one hop the real runtime makes on the app's
// behalf. It hands the resulting Cmd back rather than swallowing it, so a test
// can assert what opening the prompt DOES (which must be: re-arm the listener,
// and nothing else).
func deliverCwdSignal(t *testing.T, m tea.Model, msgs <-chan tea.Msg) (tea.Model, tea.Cmd) {
	t.Helper()
	select {
	case msg := <-msgs:
		return m.Update(msg)
	case <-time.After(5 * time.Second):
		t.Fatal("no cwd-missing message reached Update; the signal never crossed onto the loop")
		return m, nil
	}
}

// rearmCwdListener runs the listener Cmd Update returns after handling a
// cwd-missing message — the re-arm the real runtime would run — on its own
// goroutine, and hands back the channel the NEXT signal will arrive on. A test
// delivering two signals needs it: without the re-arm nothing is reading the
// hand-off channel, and the second signal never reaches Update at all.
func rearmCwdListener(t *testing.T, cmd tea.Cmd) <-chan tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatal("handling a cwd-missing message returned no Cmd; the listener was not re-armed")
	}
	out := make(chan tea.Msg, 1)
	go func() { out <- cmd() }()
	return out
}

// openCwdPrompt is the common setup: an App with the golden roster, a signal
// raised for id, and the prompt on screen.
func openCwdPrompt(t *testing.T, id string) (tea.Model, *notifyingSup) {
	t.Helper()
	sup := newNotifyingSup(tui.GoldenRoster())
	m, msgs := initCwdApp(t, sup)
	sup.fire(id, missingCwd)
	m, _ = deliverCwdSignal(t, m, msgs)
	return m, sup
}

// runInBackground runs cmd — expanding a tea.Batch into its members, which a
// one-Cmd helper would silently swallow — on goroutines, and gives them a
// window to land. It is how a test observes what a Cmd DOES without blocking on
// one (the cwd-missing listener parks forever until a second signal fires).
func runInBackground(cmd tea.Cmd, window time.Duration) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				go func(c tea.Cmd) { _ = c() }(c)
			}
		}
	}()
	time.Sleep(window)
}

// flat renders m and collapses every run of whitespace to one space, so a
// substring assertion about the prompt's COPY is independent of where the
// render happened to word-wrap it. Without this, asserting a sentence that
// spans a wrap point silently never matches and the test passes vacuously.
func flat(m tea.Model) string {
	return strings.Join(strings.Fields(content(m)), " ")
}

// TestCwdMissingSignalOpensThePrompt is the entry point of the whole feature:
// a background signal must cross onto the Update loop and put the three-way
// prompt on screen, without the user having typed anything.
func TestCwdMissingSignalOpensThePrompt(t *testing.T) {
	m, _ := openCwdPrompt(t, tui.GoldenRoster()[0].ID)

	got := flat(m)
	for _, want := range []string{
		"Session directory is gone",
		"Re-init this session in a new directory",
		"Cancel — leave this session untouched",
		"Archive / delete this session",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt is missing %q:\n%s", want, content(m))
		}
	}
}

// TestCwdMissingSignalLeavesTheAttachScreen pins that the prompt draws over the
// ROSTER, not over the half-opened attach it interrupted. The roster-Enter path
// has already switched screens by the time the signal lands (the load is
// asynchronous), and an empty transcript under the prompt reads as a session
// that opened with nothing in it — asserting an attach that did not happen.
func TestCwdMissingSignalLeavesTheAttachScreen(t *testing.T) {
	sup := newNotifyingSup(tui.GoldenRoster())
	m, msgs := initCwdApp(t, sup)

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // attach the selected row
	if !onAttachScreen(content(m)) {
		t.Fatalf("enter did not attach; this test would prove nothing:\n%s", content(m))
	}

	sup.fire(tui.GoldenRoster()[0].ID, missingCwd)
	m, _ = deliverCwdSignal(t, m, msgs)

	if got := content(m); onAttachScreen(got) {
		t.Errorf("the prompt is drawn over the attach screen the failed load left behind:\n%s", got)
	}
	if got := content(m); !strings.Contains(got, "Session directory is gone") {
		t.Errorf("the prompt did not open:\n%s", got)
	}
}

// TestCwdMissingPromptNamesTheMissingDirectory pins that the prompt says WHICH
// directory went missing. Naming it is how a user tells "I deleted that
// project" from "the volume isn't mounted", and those have different answers —
// a prompt that only says "the directory is gone" cannot be acted on.
func TestCwdMissingPromptNamesTheMissingDirectory(t *testing.T) {
	m, _ := openCwdPrompt(t, tui.GoldenRoster()[0].ID)

	if got := flat(m); !strings.Contains(got, missingCwd) {
		t.Errorf("the prompt never names the recorded directory %q that went missing:\n%s", missingCwd, content(m))
	}
}

// TestCwdMissingPromptWarnsAboutCwdScopedContext is the ruling's load-bearing
// requirement, pinned by a test rather than by review: re-initing a session
// somewhere else REBASES its local context, and the prompt has to say so in the
// UI — not only in a doc the user reading it is not holding.
//
// Each of the four cwd-scoped surfaces is asserted by name, so dropping any one
// of them from the copy fails here. The assertion runs over whitespace-flattened
// output ([flat]) because the warning is word-wrapped at render width.
func TestCwdMissingPromptWarnsAboutCwdScopedContext(t *testing.T) {
	m, _ := openCwdPrompt(t, tui.GoldenRoster()[0].ID)

	got := flat(m)
	for _, want := range []string{
		"the session will load DIFFERENT local context there",
		".gofer/config.json",
		"<cwd>/.gofer/commands",
		"skills",
		"file resolution are all cwd-scoped",
		"you are rebasing this session's environment",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the cwd-scoped-context warning is missing %q:\n%s", want, content(m))
		}
	}
}

// TestCwdMissingCursorMoves pins the choice list's navigation: ↓/↑ move the
// caret over the three options and CLAMP at both ends (stepChoiceCursor), so an
// over-travelled cursor can never wrap from "archive / delete" back onto
// "re-init" — the surprise that gets the wrong answer sent.
func TestCwdMissingCursorMoves(t *testing.T) {
	m, _ := openCwdPrompt(t, tui.GoldenRoster()[0].ID)

	// Opens on the first option.
	if got := caretRow(t, m); !strings.Contains(got, "Re-init") {
		t.Fatalf("prompt opened with the caret on %q, want the re-init row", got)
	}
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if got := caretRow(t, m); !strings.Contains(got, "Cancel") {
		t.Errorf("after ↓ the caret is on %q, want the cancel row", got)
	}
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyDown}) // clamped: no wrap
	if got := caretRow(t, m); !strings.Contains(got, "Archive") {
		t.Errorf("after ↓↓↓ the caret is on %q, want the archive row (clamped at the last)", got)
	}
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyUp}) // clamped: no wrap
	if got := caretRow(t, m); !strings.Contains(got, "Re-init") {
		t.Errorf("after ↑↑↑ the caret is on %q, want the re-init row (clamped at the first)", got)
	}
}

// caretRow returns the rendered line carrying the choice-list caret.
func caretRow(t *testing.T, m tea.Model) string {
	t.Helper()
	for _, line := range strings.Split(content(m), "\n") {
		if strings.Contains(line, "▸") {
			return line
		}
	}
	t.Fatalf("no caret row in the frame:\n%s", content(m))
	return ""
}

// TestCwdMissingCancelMutatesNothing is the ruling's binding constraint: cancel
// returns to the overview having asked the Supervisor for NOTHING. It asserts
// the recorded op log is empty — fakeSup records every Resume/Kill/Archive as
// "resume:<id>:<cwd>" / "kill:<id>" / "archive:<id>" — paired with a POSITIVE
// assertion that the prompt is actually gone, so "stayed silent" cannot be
// confused with "never ran at all".
func TestCwdMissingCancelMutatesNothing(t *testing.T) {
	m, sup := openCwdPrompt(t, tui.GoldenRoster()[0].ID)

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyDown})  // onto Cancel
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // take it

	if ops := sup.recordedOps(); len(ops) != 0 {
		t.Errorf("ops = %q, want none — cancel must mutate nothing", ops)
	}
	got := content(m)
	if strings.Contains(got, "Session directory is gone") {
		t.Errorf("cancel left the prompt on screen:\n%s", got)
	}
	if !strings.Contains(got, "space peek") {
		t.Errorf("cancel did not land back on the overview:\n%s", got)
	}
}

// TestCwdMissingQuickKeyCancelMutatesNothing is the same property via the "2"
// quick key rather than the caret — the two must agree, since the numbered
// leaders in the render advertise them as equivalent.
func TestCwdMissingQuickKeyCancelMutatesNothing(t *testing.T) {
	m, sup := openCwdPrompt(t, tui.GoldenRoster()[0].ID)

	m = press(t, m, tea.KeyPressMsg{Text: "2"})

	if ops := sup.recordedOps(); len(ops) != 0 {
		t.Errorf("ops = %q, want none — the 2 quick key is cancel", ops)
	}
	if got := content(m); strings.Contains(got, "Session directory is gone") {
		t.Errorf("the 2 quick key left the prompt on screen:\n%s", got)
	}
}

// TestCwdMissingEscapeDismissesToOverview pins the default dismissal path: Esc
// is cancel. Same mutates-nothing assertion, because Esc must not be a
// second-class way out that quietly does something else.
func TestCwdMissingEscapeDismissesToOverview(t *testing.T) {
	m, sup := openCwdPrompt(t, tui.GoldenRoster()[0].ID)

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})

	if ops := sup.recordedOps(); len(ops) != 0 {
		t.Errorf("ops = %q, want none — esc is cancel", ops)
	}
	got := content(m)
	if strings.Contains(got, "Session directory is gone") {
		t.Errorf("esc left the prompt on screen:\n%s", got)
	}
	if !strings.Contains(got, "space peek") {
		t.Errorf("esc did not land back on the overview:\n%s", got)
	}
}

// TestCwdMissingPromptOpenAtTeardownMutatesNothing is the client-disconnect
// half of "cancel is the default on every dismissal path", asserted client-side:
// with the prompt open and never answered, the model is simply dropped — the
// terminal closed, the process killed, the connection lost — and the Supervisor
// must have been asked for nothing at any point.
//
// It holds by construction rather than by a cleanup path: opening the prompt
// issues no op, so there is no server-side state a teardown would have to
// unwind. This test is what stops a later "optimistically start the resume
// while the user reads the prompt" from being added without noticing — the
// Cmd the opening Update returns is actually RUN here (expanding a batch, which
// a one-Cmd helper would swallow, leaving this vacuous), so an op smuggled
// alongside the listener re-arm would be recorded and fail the assertion.
func TestCwdMissingPromptOpenAtTeardownMutatesNothing(t *testing.T) {
	sup := newNotifyingSup(tui.GoldenRoster())
	m, msgs := initCwdApp(t, sup)
	sup.fire(tui.GoldenRoster()[0].ID, missingCwd)
	m, cmd := deliverCwdSignal(t, m, msgs)

	// Positive half: the prompt really is open and unanswered.
	if !strings.Contains(content(m), "Session directory is gone") {
		t.Fatalf("the prompt is not open; this test would prove nothing:\n%s", content(m))
	}
	// Negative half: let whatever opening it dispatched run, then drop the
	// model without ever answering — the client quitting, the terminal closing,
	// the connection dropping.
	runInBackground(cmd, 200*time.Millisecond)

	if ops := sup.recordedOps(); len(ops) != 0 {
		t.Errorf("ops = %q, want none — an unanswered prompt must leave the session untouched", ops)
	}
}

// TestCwdMissingArchiveKillsALiveSession pins the third choice reaching the
// existing lifecycle affordance, dispatching exactly what the roster's ctrl+x
// confirm dispatches for the same session state: the golden roster's first row
// is Working, so it is a kill.
func TestCwdMissingArchiveKillsALiveSession(t *testing.T) {
	id := tui.GoldenRoster()[0].ID
	m, sup := openCwdPrompt(t, id)

	m, cmd := m.Update(tea.KeyPressMsg{Text: "3"})
	m = runCmd(t, m, cmd)

	if ops := sup.recordedOps(); len(ops) != 1 || ops[0] != "kill:"+id {
		t.Fatalf("ops = %q, want [kill:%s]", ops, id)
	}
	if got := content(m); strings.Contains(got, "Session directory is gone") {
		t.Errorf("the archive/delete choice left the prompt on screen:\n%s", got)
	}
}

// TestCwdMissingArchiveArchivesAnUnknownSession covers the other branch: a
// session the polled roster snapshot does not hold has no live runner to kill,
// so it archives. Journals are never deleted either way (repo invariant #4).
func TestCwdMissingArchiveArchivesAnUnknownSession(t *testing.T) {
	const id = "0192a0c4-off0-7000-8000-00000000dead"
	m, sup := openCwdPrompt(t, id)

	m, cmd := m.Update(tea.KeyPressMsg{Text: "3"})
	m = runCmd(t, m, cmd)

	if ops := sup.recordedOps(); len(ops) != 1 || ops[0] != "archive:"+id {
		t.Fatalf("ops = %q, want [archive:%s]", ops, id)
	}
	if got := content(m); strings.Contains(got, "Session directory is gone") {
		t.Errorf("the archive/delete choice left the prompt on screen:\n%s", got)
	}
}

// TestCwdMissingReinitSendsTheChosenDirectory is the re-init branch's contract:
// the directory the USER typed goes out as an explicit, NON-blank cwd. That
// non-blankness is the entire mechanism — it is what tells the daemon this is
// intent rather than an echo, and therefore what makes a bad pick hard-error
// normally instead of silently resolving to something else.
func TestCwdMissingReinitSendsTheChosenDirectory(t *testing.T) {
	id := tui.GoldenRoster()[1].ID
	m, sup := openCwdPrompt(t, id)

	m = press(t, m, tea.KeyPressMsg{Text: "1"}) // into the directory stage
	m = type_(t, m, "/tmp/rehomed")
	m, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = runCmd(t, m, cmd)

	if ops := sup.recordedOps(); len(ops) != 1 || ops[0] != "resume:"+id+":/tmp/rehomed" {
		t.Fatalf("ops = %q, want [resume:%s:/tmp/rehomed] — an explicit, non-blank cwd", ops, id)
	}
	// The positive half: a successful re-init lands on the session, the same
	// create-then-attach shape every other resume has.
	if got := content(m); !onAttachScreen(got) {
		t.Errorf("expected a successful re-init to attach into the session, got:\n%s", got)
	}
}

// TestCwdMissingReinitStatesWhereItIsResuming pins "where a session resumes
// must be stated to the user, never silently substituted": the status note
// names the directory at dispatch, before the round trip resolves either way.
func TestCwdMissingReinitStatesWhereItIsResuming(t *testing.T) {
	m, _ := openCwdPrompt(t, tui.GoldenRoster()[1].ID)

	m = press(t, m, tea.KeyPressMsg{Text: "1"})
	m = type_(t, m, "/tmp/rehomed")
	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	if got := flat(m); !strings.Contains(got, "Reopening session in /tmp/rehomed.") {
		t.Errorf("the frame never states where the session is being reopened:\n%s", content(m))
	}
}

// TestCwdMissingReinitResolvesARelativePathAgainstTheClientCwd pins that a
// relative path is anchored to THIS client's working directory — the frame of
// reference the user typed it in — rather than sent raw for the daemon to
// resolve against its own.
//
// The expected value is COMPUTED from the real home rather than written out,
// because the fixture's own cwd is the literal string "~/orchestration"
// ([tui.GoldenCommandEnv]) and a hardcoded "~/orchestration/sub/dir" would pin
// the very violation this asserts against: a tilde on the wire is not an
// absolute path, and the daemon would expand it against ITS home. See
// TestCwdMissingReinitAlwaysSendsAnAbsolutePath for the property itself.
func TestCwdMissingReinitResolvesARelativePathAgainstTheClientCwd(t *testing.T) {
	id := tui.GoldenRoster()[1].ID
	m, sup := openCwdPrompt(t, id)

	m = press(t, m, tea.KeyPressMsg{Text: "1"})
	m = type_(t, m, "sub/dir")
	m, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = runCmd(t, m, cmd)

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("os.UserHomeDir: %v", err)
	}
	want := "resume:" + id + ":" + filepath.Join(home, "orchestration", "sub", "dir")
	if ops := sup.recordedOps(); len(ops) != 1 || ops[0] != want {
		t.Fatalf("ops = %q, want [%s]", ops, want)
	}
	if got := content(m); !onAttachScreen(got) {
		t.Errorf("expected a successful re-init to attach into the session, got:\n%s", got)
	}
}

// TestCwdMissingReinitAlwaysSendsAnAbsolutePath is the property behind the case
// above, over every shape a user can type: whatever goes out as the re-init cwd
// is ABSOLUTE. ACP requires it, and anything else is resolved on the far side —
// a relative path against the DAEMON's working directory, a "~" against the
// DAEMON's home — which is a different machine's idea of where the user meant.
// It fails closed today (-32602 rather than a wrong directory), which is why
// this is a property test and not a blocker; a path that happens to exist on
// both sides is how it would stop failing closed.
func TestCwdMissingReinitAlwaysSendsAnAbsolutePath(t *testing.T) {
	for _, typed := range []string{"sub/dir", "~/rehomed", "~", "./here", "../sibling", "/tmp/rehomed"} {
		t.Run(typed, func(t *testing.T) {
			id := tui.GoldenRoster()[1].ID
			m, sup := openCwdPrompt(t, id)

			m = press(t, m, tea.KeyPressMsg{Text: "1"})
			m = type_(t, m, typed)
			m, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			runCmd(t, m, cmd)

			ops := sup.recordedOps()
			if len(ops) != 1 {
				t.Fatalf("ops = %q, want exactly one resume", ops)
			}
			sent := strings.TrimPrefix(ops[0], "resume:"+id+":")
			if !filepath.IsAbs(sent) {
				t.Errorf("typed %q → cwd %q on the wire, want an absolute path", typed, sent)
			}
			if strings.Contains(sent, "~") {
				t.Errorf("typed %q → cwd %q on the wire, want the tilde expanded client-side", typed, sent)
			}
		})
	}
}

// TestCwdMissingReinitWithNothingTypedIsANoOp pins that Enter on an empty entry
// commits NOTHING. A blank cwd on the wire means "reopen where it was
// recorded", which is precisely the state this prompt exists because of — so
// committing one would loop the session straight back into the same failure.
func TestCwdMissingReinitWithNothingTypedIsANoOp(t *testing.T) {
	m, sup := openCwdPrompt(t, tui.GoldenRoster()[1].ID)

	m = press(t, m, tea.KeyPressMsg{Text: "1"})
	m, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = runCmd(t, m, cmd)

	if ops := sup.recordedOps(); len(ops) != 0 {
		t.Errorf("ops = %q, want none — an empty entry must never commit a blank cwd", ops)
	}
	if got := flat(m); !strings.Contains(got, "Directory:") {
		t.Errorf("the directory stage closed on an empty Enter; it must stay open:\n%s", content(m))
	}
}

// TestCwdMissingDirStageEscapeReturnsToTheChoices pins the two-stage escape the
// model picker established: Esc in the directory stage costs the typed path,
// not the whole prompt, and a SECOND Esc then cancels. Neither touches the
// Supervisor, so every escape path still lands on cancel.
func TestCwdMissingDirStageEscapeReturnsToTheChoices(t *testing.T) {
	m, sup := openCwdPrompt(t, tui.GoldenRoster()[1].ID)

	m = press(t, m, tea.KeyPressMsg{Text: "1"})
	m = type_(t, m, "/tmp/rehomed")
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})

	got := flat(m)
	if !strings.Contains(got, "Archive / delete this session") {
		t.Errorf("esc from the directory stage did not return to the choice list:\n%s", content(m))
	}
	if strings.Contains(got, "/tmp/rehomed") {
		t.Errorf("the abandoned path is still on screen:\n%s", content(m))
	}

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if got := content(m); strings.Contains(got, "Session directory is gone") {
		t.Errorf("a second esc did not dismiss the prompt:\n%s", got)
	}
	if ops := sup.recordedOps(); len(ops) != 0 {
		t.Errorf("ops = %q, want none — no escape path may mutate anything", ops)
	}
}

// TestCwdMissingPromptOutranksTheCommandPanel pins the dispatch precedence: the
// prompt opens in response to a daemon signal rather than to something the user
// typed, so it closes whatever overlay was showing and claims the next key. A
// prompt drawn UNDER an open panel — with the panel still eating keys — would
// be unanswerable.
func TestCwdMissingPromptOutranksTheCommandPanel(t *testing.T) {
	sup := newNotifyingSup(tui.GoldenRoster())
	m, msgs := initCwdApp(t, sup)
	m = dispatchSlash(t, m, "/status")
	if !strings.Contains(content(m), "[Status]") {
		t.Fatalf("the panel did not open; this test would prove nothing:\n%s", content(m))
	}

	sup.fire(tui.GoldenRoster()[0].ID, missingCwd)
	m, _ = deliverCwdSignal(t, m, msgs)

	got := content(m)
	if !strings.Contains(got, "Session directory is gone") {
		t.Errorf("the prompt did not open over the panel:\n%s", got)
	}
	if strings.Contains(got, "[Status]") {
		t.Errorf("the command panel is still open under the prompt:\n%s", got)
	}
	// And the next key answers the PROMPT, not the panel.
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	if strings.Contains(content(m), "Session directory is gone") {
		t.Errorf("esc did not reach the prompt:\n%s", content(m))
	}
	if ops := sup.recordedOps(); len(ops) != 0 {
		t.Errorf("ops = %q, want none", ops)
	}
}

// TestCwdMissingCancelThenReEnterRetriesTheAttach is the SECOND-attempt half of
// the remedy, and the reason it needs asserting at all: cancelling the prompt
// leaves the session exactly as it was, so the obvious next move is to press
// Enter on the same roster row again — and that must retry, not show a silent
// empty attach screen.
//
// The retry is a re-SUBSCRIBE. [App.enter] is a no-op while the App still holds
// a subscription for that session id, so an aborted attach that left its stream
// behind would make the second Enter do literally nothing: no attach, no load,
// no signal, no prompt. Counting subscribes is therefore the honest assertion at
// this layer — the daemon-side half (the load being re-issued rather than
// memoized) is pinned by internal/wirestream's
// TestSecondReferenceAfterCwdMissingLoadsAgain.
func TestCwdMissingCancelThenReEnterRetriesTheAttach(t *testing.T) {
	id := tui.GoldenRoster()[0].ID
	sup := newNotifyingSup(tui.GoldenRoster())
	m, msgs := initCwdApp(t, sup)

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // attach: subscribe #1
	if got := sup.subscribes(); got != 1 {
		t.Fatalf("subscribes after the first enter = %d, want 1 — this test would prove nothing", got)
	}

	sup.fire(id, missingCwd)
	m, _ = deliverCwdSignal(t, m, msgs)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEscape}) // cancel

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // attach again
	if got := sup.subscribes(); got != 2 {
		t.Errorf("subscribes after re-entering the same row = %d, want 2 — the aborted attach left its subscription "+
			"behind, so the second enter is a silent no-op and the prompt can never re-open", got)
	}
	if ops := sup.recordedOps(); len(ops) != 0 {
		t.Errorf("ops = %q, want none — retrying the attach must still mutate nothing", ops)
	}
	_ = m
}

// TestCwdMissingSecondSignalDoesNotHijackTheOpenPrompt pins that a signal
// arriving while a prompt is already open is DROPPED, not adopted. Adopting it
// would discard the stage and the path the user has typed, and silently
// re-point the answer at a different session — the user would then commit a
// directory for a session they never chose. Nothing is lost by dropping it:
// attaching that session again raises it again.
func TestCwdMissingSecondSignalDoesNotHijackTheOpenPrompt(t *testing.T) {
	first, second := tui.GoldenRoster()[0], tui.GoldenRoster()[1]
	sup := newNotifyingSup(tui.GoldenRoster())
	m, msgs := initCwdApp(t, sup)

	sup.fire(first.ID, missingCwd)
	m, listen := deliverCwdSignal(t, m, msgs)
	next := rearmCwdListener(t, listen)
	m = press(t, m, tea.KeyPressMsg{Text: "1"}) // into the directory stage
	m = type_(t, m, "/tmp/rehomed")

	sup.fire(second.ID, "/home/j/projects/some-other-gone-dir")
	m, _ = deliverCwdSignal(t, m, next)

	got := flat(m)
	if !strings.Contains(got, "/tmp/rehomed") {
		t.Errorf("the second signal discarded the directory the user had typed:\n%s", content(m))
	}
	if strings.Contains(got, "some-other-gone-dir") {
		t.Errorf("the second signal re-pointed the open prompt at a different session:\n%s", content(m))
	}
	// And committing still answers the session the prompt opened for.
	m, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	runCmd(t, m, cmd)
	want := "resume:" + first.ID + ":/tmp/rehomed"
	if ops := sup.recordedOps(); len(ops) != 1 || ops[0] != want {
		t.Fatalf("ops = %q, want [%s]", ops, want)
	}
}

// TestCwdMissingResumeErrorIsNotPaintedUnderThePrompt covers the /resume path's
// double report. That path learns of the failure twice — the wrapped error it
// returns AND the signal the bridge relays — from two different goroutines, so
// which reaches Update first is scheduler-dependent. This drives the order where
// the signal wins: the raw "…session cwd … is no longer available…" string must
// NOT then be painted in the footer underneath the prompt that replaced it.
func TestCwdMissingResumeErrorIsNotPaintedUnderThePrompt(t *testing.T) {
	const id = "0192a0c4-off0-7000-8000-00000000beef"
	const raw = "daemonbridge: resume " + id + `: session cwd "` + missingCwd + `" is no longer available: stat: no such file or directory`

	sup := newNotifyingSup(tui.GoldenRoster())
	sup.resumeErr = errors.New(raw)
	m, msgs := initCwdApp(t, sup)

	// Dispatch /resume WITHOUT running its Cmd, so the resume's own error is
	// still in flight when the signal lands.
	m = type_(t, m, "/resume "+id)
	m, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("/resume <id> dispatched no command; this test would prove nothing")
	}

	sup.fire(id, missingCwd)
	m, _ = deliverCwdSignal(t, m, msgs)
	if !strings.Contains(content(m), "Session directory is gone") {
		t.Fatalf("the prompt did not open; this test would prove nothing:\n%s", content(m))
	}

	m, _ = m.Update(cmd()) // the resume's error lands second

	// The assertion matches the error's PREFIX, not its tail: the status footer
	// is truncated to the terminal width, so a marker chosen from the end of a
	// 160-column error message would be clipped away and the check would pass
	// whether or not the error was painted.
	if got := content(m); strings.Contains(got, "daemonbridge: resume") {
		t.Errorf("the raw resume error is painted under the prompt that exists to replace it:\n%s", got)
	}
	if got := content(m); !strings.Contains(got, "Session directory is gone") {
		t.Errorf("the late error tore the prompt down:\n%s", got)
	}
}

// TestCwdMissingResumeErrorForAnotherSessionStillReports is the negative half of
// the guard above: the suppression is scoped to the session the prompt is
// answering. An unrelated resume that fails while the prompt happens to be open
// still has to say so, or the guard would swallow real errors.
func TestCwdMissingResumeErrorForAnotherSessionStillReports(t *testing.T) {
	sup := newNotifyingSup(tui.GoldenRoster())
	sup.resumeErr = errors.New("supervisor: resume 0192dead: no such session")
	m, msgs := initCwdApp(t, sup)

	m = type_(t, m, "/resume 0192dead-0000-7000-8000-000000000000")
	m, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("/resume <id> dispatched no command; this test would prove nothing")
	}

	sup.fire(tui.GoldenRoster()[0].ID, missingCwd) // a DIFFERENT session's prompt
	m, _ = deliverCwdSignal(t, m, msgs)
	m, _ = m.Update(cmd())

	if got := content(m); !strings.Contains(got, "no such session") {
		t.Errorf("an unrelated resume failure was swallowed by the open prompt:\n%s", got)
	}
}

// TestCwdMissingPromptKeepsItsChoicesOnAShortTerminal pins the clip order. The
// prompt is MODAL — it claims every key while it is open — so a frame that has
// room for the heading and the headline but not for the three options is a
// wedge: something has visibly gone wrong and nothing on screen says which key
// answers it. The context rows are what gets clipped; the actionable rows are
// what survives.
func TestCwdMissingPromptKeepsItsChoicesOnAShortTerminal(t *testing.T) {
	sup := newNotifyingSup(tui.GoldenRoster())
	var m tea.Model = tui.NewApp(theme.Test(), sup, tui.GoldenMeta(), tui.GoldenCommandEnv())
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	cmd := m.Init()
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatalf("Init() did not batch: %T", cmd())
	}
	msgs := make(chan tea.Msg, len(batch))
	for _, c := range batch {
		go func(c tea.Cmd) { msgs <- c() }(c)
	}
	m, _ = m.Update(<-msgs)

	sup.fire(tui.GoldenRoster()[0].ID, missingCwd)
	m, _ = deliverCwdSignal(t, m, msgs)

	// Squeeze the terminal down to a height that cannot hold the whole prompt.
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 10})

	got := flat(m)
	for _, want := range []string{"1 Re-init", "2 Cancel", "3 Archive"} {
		if !strings.Contains(got, want) {
			t.Errorf("a short terminal clipped away %q — the modal is unanswerable:\n%s", want, content(m))
		}
	}
}

// TestGoldenCwdMissingPrompt is the render-critical component's golden: the
// exact text and geometry of the three-way prompt composed into a real frame,
// written before any styling work. The Ascii profile cannot see colour, which
// is why every state signal here — the heading, the warning, the numbered
// leaders, the hint — is carried by TEXT, and why the pair of VHS tapes
// (vhs/roster-cwd-missing-*.tape) exists alongside it.
func TestGoldenCwdMissingPrompt(t *testing.T) {
	m, _ := openCwdPrompt(t, tui.GoldenRoster()[0].ID)
	testkit.AssertGolden(t, "app_cwd_missing_prompt", content(m))
}

// TestGoldenCwdMissingDirPicker is the directory stage's golden: the warning is
// STILL on screen at the moment of commitment, above the entry line and the
// candidate list.
func TestGoldenCwdMissingDirPicker(t *testing.T) {
	m, _ := openCwdPrompt(t, tui.GoldenRoster()[0].ID)
	m = press(t, m, tea.KeyPressMsg{Text: "1"})
	m = type_(t, m, "/tmp/rehomed")
	testkit.AssertGolden(t, "app_cwd_missing_dir_picker", content(m))
}
