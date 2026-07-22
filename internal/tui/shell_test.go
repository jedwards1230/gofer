package tui

// shell_test.go covers the `!` / `!!` shell escape (shell.go) at the unit
// level: the submit-buffer grammar, the off-loop runner's four robustness
// cases (non-zero exit, interleaved streams, an unbounded printer, a command
// that never exits), the output pane's rendering, and — the load-bearing one
// — the `!!` context exclusion asserted at the PROMPT, not at the pixels. A
// test that only checked the pane would pass even if the excluded output
// leaked into every prompt, which is the exact bug `!!` exists to prevent.
// White-box (package tui) because every one of those seams is unexported.

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// runShell executes the escape runner synchronously — exactly what a
// bubbletea Cmd does off the Update loop — and returns the message it posts
// back. $SHELL is pinned to /bin/sh so the assertions below hold whatever
// interactive shell the developer or CI runner happens to use.
func runShell(t *testing.T, line string, timeout time.Duration, limit int) shellDoneMsg {
	t.Helper()
	t.Setenv("SHELL", "/bin/sh")
	msg, ok := runShellCmd(t.TempDir(), 1, line, timeout, limit)().(shellDoneMsg)
	if !ok {
		t.Fatalf("runShellCmd(%q) posted a non-shellDoneMsg", line)
	}
	return msg
}

func TestParseShellEscape(t *testing.T) {
	tests := []struct {
		name          string
		buf           string
		wantLine      string
		wantInContext bool
		wantOK        bool
	}{
		{"single bang runs and shares", "!ls -la", "ls -la", true, true},
		{"double bang runs and withholds", "!!ls -la", "ls -la", false, true},
		{"leading space after the sigil is trimmed", "!  echo hi", "echo hi", true, true},
		{"double bang with spacing", "!!  echo hi", "echo hi", false, true},
		{"bare bang has nothing to run", "!", "", true, false},
		{"bare double bang has nothing to run", "!!", "", false, false},
		{"whitespace-only command has nothing to run", "!   ", "", true, false},
		{"double bang whitespace-only", "!!\t ", "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line, inContext, ok := parseShellEscape(tt.buf)
			if ok != tt.wantOK {
				t.Fatalf("parseShellEscape(%q) ok = %v, want %v", tt.buf, ok, tt.wantOK)
			}
			if line != tt.wantLine {
				t.Errorf("parseShellEscape(%q) line = %q, want %q", tt.buf, line, tt.wantLine)
			}
			if ok && inContext != tt.wantInContext {
				t.Errorf("parseShellEscape(%q) inContext = %v, want %v", tt.buf, inContext, tt.wantInContext)
			}
		})
	}
}

// TestHasInputPrefixLeadingOnly is the non-hijacking guard: only a sigil at
// position 0 of the SUBMITTED buffer is a prefix. Text that merely contains
// one — an email address, an exclamation, a pasted history expansion — is an
// ordinary prompt.
func TestHasInputPrefixLeadingOnly(t *testing.T) {
	triggers := []string{"/status", "/", "!ls", "!!ls", "!"}
	for _, buf := range triggers {
		if !hasInputPrefix(buf) {
			t.Errorf("hasInputPrefix(%q) = false, want true", buf)
		}
	}
	plain := []string{
		"",
		"hello",
		"mail me at sorretin@gmail.com",
		"that worked!",
		"run !! twice",
		"check the /etc/hosts file",
		" !ls",  // a leading space means the sigil isn't at position 0
		"a!b",   // mid-word
		"echo!", // trailing
	}
	for _, buf := range plain {
		if hasInputPrefix(buf) {
			t.Errorf("hasInputPrefix(%q) = true, want false — only a LEADING sigil dispatches", buf)
		}
	}
}

func TestRunShellCmdReportsExitCode(t *testing.T) {
	msg := runShell(t, "echo boom >&2; exit 3", 5*time.Second, 0)
	if msg.exitCode != 3 {
		t.Errorf("exitCode = %d, want 3", msg.exitCode)
	}
	if msg.note != "" {
		t.Errorf("note = %q, want empty — a clean non-zero exit is an exit CODE, not a note", msg.note)
	}
	if !strings.Contains(msg.output, "boom") {
		t.Errorf("output = %q, want the command's stderr retained, not swallowed", msg.output)
	}
}

// TestRunShellCmdInterleavesStdoutAndStderr proves both streams reach the
// same buffer (os/exec serializes writes when they share one writer), so a
// command's diagnostics land beside its output as they do in a terminal.
func TestRunShellCmdInterleavesStdoutAndStderr(t *testing.T) {
	msg := runShell(t, "echo one; echo two >&2; echo three", 5*time.Second, 0)
	for _, want := range []string{"one", "two", "three"} {
		if !strings.Contains(msg.output, want) {
			t.Fatalf("output = %q, want it to contain %q", msg.output, want)
		}
	}
	if msg.truncated {
		t.Error("truncated = true for a three-line command with no limit")
	}
}

func TestRunShellCmdBoundsLargeOutput(t *testing.T) {
	const limit = 64
	msg := runShell(t, "i=0; while [ $i -lt 5000 ]; do echo aaaaaaaaaaaaaaaaaaaa; i=$((i+1)); done", 20*time.Second, limit)
	if !msg.truncated {
		t.Fatal("truncated = false for output far past the limit")
	}
	if len(msg.output) > limit {
		t.Fatalf("retained %d bytes, want at most %d", len(msg.output), limit)
	}
	if msg.exitCode != 0 || msg.note != "" {
		t.Errorf("exitCode = %d note = %q; a truncated command must still be reported as having SUCCEEDED — the writer accepts every write so the child never sees a broken pipe", msg.exitCode, msg.note)
	}
}

func TestRunShellCmdTimesOut(t *testing.T) {
	start := time.Now()
	msg := runShell(t, "sleep 30", 100*time.Millisecond, 0)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the timeout did not free the run: waited %s", elapsed)
	}
	if !strings.Contains(msg.note, "timed out") {
		t.Fatalf("note = %q, want a timeout note rather than a bare \"signal: killed\"", msg.note)
	}
}

// TestRunShellCmdDoesNotBlockOnAnOrphanHoldingThePipe covers the case with no
// timeout involved at all: the shell exits immediately but leaves a
// background job holding the output pipe. Without [shellWaitDelay], os/exec's
// copier goroutine reads that pipe until the orphan exits, so this would
// block for the orphan's full lifetime — the TUI would show "running…" for 30
// seconds after a command that already finished.
func TestRunShellCmdDoesNotBlockOnAnOrphanHoldingThePipe(t *testing.T) {
	start := time.Now()
	msg := runShell(t, "sleep 30 & echo detached", 25*time.Second, 0)
	elapsed := time.Since(start)

	if elapsed > 20*time.Second {
		t.Fatalf("a backgrounded orphan held the run open for %s", elapsed)
	}
	if !strings.Contains(msg.output, "detached") {
		t.Errorf("output = %q, want the foreground command's own output kept", msg.output)
	}
}

func TestRunShellCmdReportsUnstartableShell(t *testing.T) {
	t.Setenv("SHELL", "/nonexistent/definitely-not-a-shell")
	msg, ok := runShellCmd(t.TempDir(), 1, "echo hi", 5*time.Second, 0)().(shellDoneMsg)
	if !ok {
		t.Fatal("expected a shellDoneMsg")
	}
	if msg.note == "" {
		t.Fatal("note = \"\", want the start failure surfaced rather than swallowed as a clean run")
	}
}

func TestBoundedWriterUnlimited(t *testing.T) {
	w := &boundedWriter{}
	n, err := w.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write = (%d, %v), want (5, nil)", n, err)
	}
	if w.truncated {
		t.Error("truncated = true with no limit set")
	}
}

func TestBoundedWriterAcceptsEveryWriteAfterTheCap(t *testing.T) {
	w := &boundedWriter{limit: 4}
	if n, err := w.Write([]byte("abcdef")); n != 6 || err != nil {
		t.Fatalf("Write = (%d, %v), want the full length accepted so the child never sees a short write", n, err)
	}
	if got := w.String(); got != "abcd" {
		t.Errorf("retained %q, want %q", got, "abcd")
	}
	if n, err := w.Write([]byte("ghi")); n != 3 || err != nil {
		t.Fatalf("second Write = (%d, %v), want (3, nil)", n, err)
	}
	if got := w.String(); got != "abcd" {
		t.Errorf("retained %q after the cap, want it unchanged", got)
	}
	if !w.truncated {
		t.Error("truncated = false after dropping bytes")
	}
}

// finishedRun builds a completed run for the composePrompt tests below.
func finishedRun(seq int, line, output string, inContext bool) shellRun {
	return shellRun{seq: seq, line: line, output: output, inContext: inContext, done: true}
}

// TestComposePromptExcludesDoubleBangOutput is THE `!!` test. It asserts on
// the string that becomes model input, so it fails if the exclusion is
// removed anywhere between the run list and the prompt — no rendering detail
// can make it pass.
func TestComposePromptExcludesDoubleBangOutput(t *testing.T) {
	a := App{shellRuns: []shellRun{
		finishedRun(1, "cat public.txt", "SHARED-OUTPUT", true),
		finishedRun(2, "cat secrets.env", "WITHHELD-OUTPUT", false),
	}}

	got := a.composePrompt("what changed?")

	if !strings.Contains(got, "SHARED-OUTPUT") {
		t.Errorf("prompt = %q, want the `!` run's output folded in", got)
	}
	if strings.Contains(got, "WITHHELD-OUTPUT") {
		t.Fatalf("prompt = %q — the `!!` run's OUTPUT reached the model; that exclusion is the whole point of the spelling", got)
	}
	if strings.Contains(got, "cat secrets.env") {
		t.Fatalf("prompt = %q — the `!!` run's COMMAND reached the model", got)
	}
	if !strings.HasSuffix(got, "what changed?") {
		t.Errorf("prompt = %q, want the user's own text last", got)
	}
}

// TestComposePromptConsumesEveryFinishedRunOnce covers the other half of the
// exclusion: a `!!` run is marked consumed WITHOUT contributing, so a later
// prompt can't pick it up either, and a `!` run isn't re-sent.
func TestComposePromptConsumesEveryFinishedRunOnce(t *testing.T) {
	a := App{shellRuns: []shellRun{
		finishedRun(1, "cat public.txt", "SHARED-OUTPUT", true),
		finishedRun(2, "cat secrets.env", "WITHHELD-OUTPUT", false),
	}}

	if first := a.composePrompt("one"); !strings.Contains(first, "SHARED-OUTPUT") {
		t.Fatalf("first prompt = %q, want the `!` output", first)
	}

	second := a.composePrompt("two")
	if second != "two" {
		t.Fatalf("second prompt = %q, want %q — every finished run is consumed exactly once", second, "two")
	}
}

// TestComposePromptLeavesRunningRunsAlone: a command still in flight is
// neither folded (half a buffer is worse than waiting) nor consumed, so its
// output reaches the NEXT prompt once it finishes.
func TestComposePromptLeavesRunningRunsAlone(t *testing.T) {
	a := App{shellRuns: []shellRun{{seq: 1, line: "sleep 1", inContext: true}}}

	if got := a.composePrompt("now"); got != "now" {
		t.Fatalf("prompt = %q, want the in-flight run skipped entirely", got)
	}
	if a.shellRuns[0].consumed {
		t.Fatal("an in-flight run was marked consumed; its output would be lost")
	}

	a = a.applyShellDone(shellDoneMsg{seq: 1, output: "LATE-OUTPUT"})
	if got := a.composePrompt("later"); !strings.Contains(got, "LATE-OUTPUT") {
		t.Fatalf("prompt = %q, want the now-finished run folded in", got)
	}
}

// TestComposePromptReportsExitCodeAndTruncation keeps the model honest about
// a command that failed or got clipped, rather than presenting partial output
// as the whole answer.
func TestComposePromptReportsExitCodeAndTruncation(t *testing.T) {
	run := finishedRun(1, "grep -r nope .", "no matches", true)
	run.exitCode = 1
	run.truncated = true
	a := App{shellRuns: []shellRun{run}}

	got := a.composePrompt("why?")
	if !strings.Contains(got, "[exit 1]") {
		t.Errorf("prompt = %q, want the non-zero exit reported", got)
	}
	if !strings.Contains(got, "[output truncated]") {
		t.Errorf("prompt = %q, want the truncation marker", got)
	}
}

func TestApplyShellDoneIgnoresUnknownSeq(t *testing.T) {
	a := App{shellRuns: []shellRun{finishedRun(1, "echo hi", "hi", true)}}
	before := a.shellRuns[0]
	a = a.applyShellDone(shellDoneMsg{seq: 99, output: "STALE"})
	if a.shellRuns[0] != before {
		t.Fatalf("a stale result overwrote a live row: %+v", a.shellRuns[0])
	}
}

func TestDispatchShellRejectsEmptyCommand(t *testing.T) {
	for _, buf := range []string{"!", "!!", "!   "} {
		a, cmd := App{}.dispatchShell(buf)
		if cmd != nil {
			t.Fatalf("dispatchShell(%q) returned a Cmd; an empty command must never reach a shell", buf)
		}
		if len(a.shellRuns) != 0 {
			t.Fatalf("dispatchShell(%q) recorded a run: %+v", buf, a.shellRuns)
		}
		if a.status == "" {
			t.Fatalf("dispatchShell(%q) said nothing; the user should be told there was nothing to run", buf)
		}
	}
}

// TestDispatchInputRoutesBySigil pins the shared first-rune switch both
// submit intercepts route through: the two prefixes never cross wires.
func TestDispatchInputRoutesBySigil(t *testing.T) {
	a := App{registry: newBuiltinRegistry()}

	slash, _ := a.dispatchInput("/nope")
	if !strings.Contains(slash.status, "unknown command") {
		t.Errorf("a `/` buffer did not reach dispatchSlash: status = %q", slash.status)
	}
	if len(slash.shellRuns) != 0 {
		t.Errorf("a `/` buffer started a shell run: %+v", slash.shellRuns)
	}

	shell, cmd := a.dispatchInput("!true")
	if cmd == nil || len(shell.shellRuns) != 1 {
		t.Errorf("a `!` buffer did not reach dispatchShell: runs = %+v cmd = %v", shell.shellRuns, cmd != nil)
	}
}

func TestGoldenShellPane(t *testing.T) {
	ok := finishedRun(1, "git status --short", " M internal/tui/app.go\n?? internal/tui/shell.go", true)
	failed := finishedRun(2, "cat missing.txt", "cat: missing.txt: No such file or directory", false)
	failed.exitCode = 1
	got := strings.Join(shellPaneLines(theme.Test(), []shellRun{ok, failed}, testkit.Width, shellPaneMaxRows), "\n")
	testkit.AssertGolden(t, "shell_pane", got)
}

func TestGoldenShellPaneStyled(t *testing.T) {
	ok := finishedRun(1, "git status --short", " M internal/tui/app.go\n?? internal/tui/shell.go", true)
	failed := finishedRun(2, "cat missing.txt", "cat: missing.txt: No such file or directory", false)
	failed.exitCode = 1
	got := strings.Join(shellPaneLines(testkit.ColorTheme(), []shellRun{ok, failed}, testkit.Width, shellPaneMaxRows), "\n")
	testkit.AssertGoldenStyled(t, "shell_pane", got)
}

func TestGoldenShellPaneRunning(t *testing.T) {
	got := strings.Join(shellPaneLines(theme.Test(), []shellRun{{seq: 1, line: "make test", inContext: true}}, testkit.Width, shellPaneMaxRows), "\n")
	testkit.AssertGolden(t, "shell_pane_running", got)
}

// TestShellPaneDegenerateSizes covers the render sizes that have panicked
// this TUI before: a zero/negative height budget and a zero width.
func TestShellPaneDegenerateSizes(t *testing.T) {
	runs := []shellRun{finishedRun(1, "echo hi", "hi", true)}
	for _, rows := range []int{-1, 0, 1, 2, 3} {
		if lines := shellPaneLines(theme.Test(), runs, testkit.Width, rows); rows < 3 && lines != nil {
			t.Fatalf("shellPaneLines(rows=%d) = %v, want nil — the pane can't fit its own rule and hint", rows, lines)
		}
	}
	if lines := shellPaneLines(theme.Test(), runs, 0, shellPaneMaxRows); len(lines) == 0 {
		t.Fatal("shellPaneLines(width=0) rendered nothing; want a clamped, non-panicking render")
	}
	if lines := shellPaneLines(theme.Test(), nil, testkit.Width, shellPaneMaxRows); lines != nil {
		t.Fatalf("shellPaneLines(no runs) = %v, want nil", lines)
	}
}

// TestShellPaneFirstFrameRender is the zero-height first-frame guard: App
// renders once BEFORE the terminal reports its size, so every overlay must
// survive a height of 0 without panicking or overflowing the frame.
func TestShellPaneFirstFrameRender(t *testing.T) {
	a := NewApp(theme.Test(), newInternalFakeSup(GoldenRoster()), GoldenMeta(), GoldenCommandEnv())
	a.shellRuns = []shellRun{finishedRun(1, "echo hi", "hi", true)}
	a.shellOpen = true

	if got := a.render(); strings.Count(got, "\n") > 2 {
		t.Fatalf("the zero-size first frame rendered %d rows:\n%s", strings.Count(got, "\n")+1, got)
	}

	a.width, a.height = testkit.Width, testkit.Height
	if got := strings.Count(a.render(), "\n") + 1; got > testkit.Height {
		t.Fatalf("the shell pane pushed the frame to %d rows, past the %d-row terminal", got, testkit.Height)
	}
}

// TestGoldenShellPaneOverOverview locks the frame arithmetic: the pane is an
// overlay composed BELOW the roster and ABOVE the dispatch bar, and the
// screen above it shrinks so the whole frame still totals the terminal
// height.
func TestGoldenShellPaneOverOverview(t *testing.T) {
	a := NewApp(theme.Test(), newInternalFakeSup(GoldenRoster()), GoldenMeta(), GoldenCommandEnv())
	a.width, a.height = testkit.Width, testkit.Height
	a.over = a.over.WithSessions(GoldenRoster())
	a.shellRuns = []shellRun{finishedRun(1, "git status --short", " M internal/tui/app.go", true)}
	a.shellOpen = true

	got := a.render()
	if rows := strings.Count(got, "\n") + 1; rows != testkit.Height {
		t.Fatalf("frame is %d rows, want exactly %d", rows, testkit.Height)
	}
	testkit.AssertGolden(t, "app_shell_pane_overview", got)
}

// TestShellPaneEscDismissKeepsPendingContext covers the two-stage Esc: the
// pane closes, but a `!` run that hasn't reached a prompt yet still owes its
// output to the next one.
func TestShellPaneEscDismissKeepsPendingContext(t *testing.T) {
	a := App{shellOpen: true, shellRuns: []shellRun{finishedRun(1, "echo hi", "PENDING-OUTPUT", true)}}
	next, _ := a.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	app, ok := next.(App)
	if !ok {
		t.Fatal("Update returned a non-App model")
	}
	if app.shellOpen {
		t.Fatal("esc did not dismiss the shell pane")
	}
	if got := app.composePrompt("go on"); !strings.Contains(got, "PENDING-OUTPUT") {
		t.Fatalf("prompt = %q — dismissing the pane discarded output the model was still owed", got)
	}
}

// TestShellResultDoesNotReopenADismissedPane: an operator who pressed Esc on
// a long-running `!` asked for their screen back; the result landing later
// must not paint itself back over whatever they moved on to.
func TestShellResultDoesNotReopenADismissedPane(t *testing.T) {
	a := App{shellOpen: false, shellRuns: []shellRun{{seq: 1, line: "sleep 30", inContext: true}}}
	a = a.applyShellDone(shellDoneMsg{seq: 1, output: "late"})
	if a.shellOpen {
		t.Fatal("a finished run re-opened a dismissed pane")
	}
	if !a.shellRuns[0].done {
		t.Fatal("the result was dropped along with the pane's visibility")
	}
}
