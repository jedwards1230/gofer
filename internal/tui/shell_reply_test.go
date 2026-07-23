package tui

// shell_reply_test.go covers the reply-now / queue behavior (Part B) and the
// shell-block selection fix (Part C) at the App level: the shellDoneMsg handler
// that fires a turn for a reply-now `!` run (and does NOT for a queued `!` or
// any `!!`), the ctrl+r toggle, and the transcript-region measurement now
// reaching a shell block rendered into the attach tail. White-box (package tui)
// because every seam is unexported.

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// attachAppWithRun builds an attach-screen App wired to sup carrying a single
// shell run (seq matching run.seq) so a shellDoneMsg can complete it.
func attachAppWithRun(t *testing.T, sup *internalFakeSup, run shellRun) App {
	t.Helper()
	a := NewApp(theme.Test(), sup, GoldenMeta(), GoldenCommandEnv())
	mdl, _ := a.Update(tea.WindowSizeMsg{Width: testkit.Width, Height: testkit.Height})
	a = mdl.(App)
	a.scr = screenAttach
	a.sessID = "sess-x"
	a.shellSeq = run.seq
	a.shellRuns = []shellRun{run}
	return a
}

// TestShellDoneReplyNowFiresTurn is the ask-#1 default: a reply-now `!` run
// finishing on attach flushes composePrompt and fires a turn via
// Supervisor.Send carrying the shell output — "add as a message by default".
func TestShellDoneReplyNowFiresTurn(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachAppWithRun(t, sup, shellRun{seq: 1, line: "echo hi", inContext: true, queued: false})

	mdl, cmd := a.Update(shellDoneMsg{seq: 1, output: "hi"})
	if _, ok := mdl.(App); !ok {
		t.Fatalf("Update returned %T, want App", mdl)
	}
	if cmd == nil {
		t.Fatal("a reply-now `!` run did not fire a turn on completion")
	}
	cmd() // invoke the doSend closure so the fake records the send
	if len(sup.sends) != 1 {
		t.Fatalf("Send calls = %d, want 1", len(sup.sends))
	}
	got := sup.sends[0]
	if got.id != "sess-x" {
		t.Errorf("Send id = %q, want sess-x", got.id)
	}
	if !strings.Contains(got.prompt, "echo hi") || !strings.Contains(got.prompt, "hi") {
		t.Errorf("Send prompt = %q, want the composed shell output ($ echo hi + hi)", got.prompt)
	}
}

// TestShellDoneQueueModeDoesNotFire is the mutation twin: the SAME run finished
// in queue mode (queued=true) waits for the user's next Enter — no turn fires.
func TestShellDoneQueueModeDoesNotFire(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachAppWithRun(t, sup, shellRun{seq: 1, line: "echo hi", inContext: true, queued: true})

	_, cmd := a.Update(shellDoneMsg{seq: 1, output: "hi"})
	if cmd != nil {
		t.Fatal("a queued `!` run fired a turn on completion; it must wait for the user's next Enter")
	}
	if len(sup.sends) != 0 {
		t.Fatalf("Send calls = %d, want 0 for a queued run", len(sup.sends))
	}
}

// TestShellDoneDoubleBangNeverFires proves the `!!` reconciliation: a `!!` run
// finishing in reply-now mode still fires nothing — the auto-send is gated on
// inContext exactly as composePrompt's exclusion is, so the toggle governs `!`
// alone and `!!` stays structurally excluded.
func TestShellDoneDoubleBangNeverFires(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachAppWithRun(t, sup, shellRun{seq: 1, line: "cat secret", inContext: false, queued: false})

	_, cmd := a.Update(shellDoneMsg{seq: 1, output: "SECRET"})
	if cmd != nil {
		t.Fatal("a `!!` run fired a turn; its output must never reach the model")
	}
	if len(sup.sends) != 0 {
		t.Fatalf("Send calls = %d, want 0 for a `!!` run", len(sup.sends))
	}
}

// TestDispatchShellCapturesQueueMode: dispatchShell stamps the live shellQueue
// onto the new run, so a later toggle can't rewrite a run already dispatched.
func TestDispatchShellCapturesQueueMode(t *testing.T) {
	a := App{shellQueue: true}
	a, cmd := a.dispatchShell("!echo hi")
	if cmd == nil || len(a.shellRuns) != 1 {
		t.Fatalf("dispatchShell did not start a run: runs=%+v cmd=%v", a.shellRuns, cmd != nil)
	}
	if !a.shellRuns[0].queued {
		t.Error("dispatchShell(queue mode) did not stamp queued=true on the run")
	}

	b := App{shellQueue: false}
	b, _ = b.dispatchShell("!echo hi")
	if b.shellRuns[0].queued {
		t.Error("dispatchShell(reply mode) stamped queued=true")
	}
}

// TestCtrlRTogglesShellQueue proves the global binding flips shellQueue both
// ways and posts NO status note (round-4 dropped it — the shell-mode rule label
// flip is the feedback, so a status line here was redundant noise).
func TestCtrlRTogglesShellQueue(t *testing.T) {
	a := NewApp(theme.Test(), newInternalFakeSup(GoldenRoster()), GoldenMeta(), GoldenCommandEnv())

	next, _, handled := dispatchGlobalKey(a, tea.Key{Code: 'r', Mod: tea.ModCtrl})
	if !handled {
		t.Fatal("ctrl+r was not handled by the global table")
	}
	a2 := next.(App)
	if !a2.shellQueue {
		t.Error("ctrl+r did not enable queue mode")
	}
	if a2.status != "" {
		t.Errorf("status = %q, want empty — the rule label flip is the feedback, not a status note", a2.status)
	}

	next, _, _ = dispatchGlobalKey(a2, tea.Key{Code: 'r', Mod: tea.ModCtrl})
	a3 := next.(App)
	if a3.shellQueue {
		t.Error("a second ctrl+r did not return to reply mode")
	}
	if a3.status != "" {
		t.Errorf("status = %q, want empty on the way back too", a3.status)
	}
}

// TestNewAppSeedsShellQueueFromConfig proves the persisted startup default
// (config.TUI.ShellReplyMode) reaches App.shellQueue at construction, so a user
// who always wants queue mode launches in it without pressing ctrl+r. The
// reply-now default is verified two ways — an explicit "reply" and the empty
// GoldenCommandEnv — so the queue case is the seed doing work, not the zero
// value coinciding with it.
func TestNewAppSeedsShellQueueFromConfig(t *testing.T) {
	envWith := func(mode string) CommandEnv {
		env := GoldenCommandEnv()
		env.Config = func() (config.Config, error) {
			return config.Config{TUI: config.TUI{ShellReplyMode: mode}}, nil
		}
		return env
	}
	tests := []struct {
		name string
		env  CommandEnv
		want bool
	}{
		{"queue mode seeds queue", envWith("queue"), true},
		{"reply mode seeds reply-now", envWith("reply"), false},
		{"empty config seeds reply-now", GoldenCommandEnv(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := NewApp(theme.Test(), newInternalFakeSup(GoldenRoster()), GoldenMeta(), tt.env)
			if a.shellQueue != tt.want {
				t.Fatalf("NewApp shellQueue = %v, want %v", a.shellQueue, tt.want)
			}
		})
	}
}

// TestSelectionCoversShellBlock is the Part C regression: a shell run renders
// into the attach transcript's tail, and both transcriptRegion and selectedText
// must now reach it. Before the fix, transcriptRegion measured the bare a.sess
// (no shell block), so the block rendered BELOW the computed region and
// selecting it returned "" — the reported bug. attachModel, shared by render
// and transcriptRegion, closes that gap.
//
// This fails against the pre-fix measurement: with transcript==0 the region
// collapses (topRow > bottomRow) and transcriptRegion reports ok=false, so the
// `if !ok` fatal below trips and selectedText returns "".
func TestSelectionCoversShellBlock(t *testing.T) {
	meta := GoldenMeta()
	meta.AttachSessionID = "sess-x"
	a := NewApp(theme.Test(), newInternalFakeSup(nil), meta, GoldenCommandEnv())
	mdl, _ := a.Update(tea.WindowSizeMsg{Width: testkit.Width, Height: testkit.Height})
	a = mdl.(App)
	a.shellRuns = []shellRun{finishedRun(1, "ls -la", "file1.txt\nfile2.txt", true)}

	// Locate the shell block's header and output rows in the composed frame.
	lines := strings.Split(a.render(), "\n")
	cmdRow, outRow := -1, -1
	for i, l := range lines {
		switch ansi.Strip(l) {
		case "! ls -la":
			cmdRow = i
		case "   └ file1.txt": // first output row now wears the └-gutter (renderBlock)
			outRow = i
		}
	}
	if cmdRow < 0 || outRow < 0 {
		t.Fatalf("precondition: shell block not found in render:\n%s", a.render())
	}

	// (a) the transcript region covers the shell block's rendered rows.
	top, bottom, ok := a.transcriptRegion()
	if !ok {
		t.Fatal("transcriptRegion reported no selectable rows — the shell block isn't being measured (pre-fix bare a.sess?)")
	}
	if cmdRow < top || outRow > bottom {
		t.Errorf("shell block rows [%d,%d] fall outside the transcript region [%d,%d]", cmdRow, outRow, top, bottom)
	}

	// (b) selectedText over the output row returns the shell output text.
	// "   └ file1.txt": cols 0-4 are the "   └ " gutter, "file1.txt" spans
	// cols 5-13.
	a.sel = &selectionState{startX: 5, startY: outRow, curX: 13, curY: outRow}
	if got := a.selectedText(); got != "file1.txt" {
		t.Errorf("selectedText over the shell block = %q, want %q", got, "file1.txt")
	}
}
