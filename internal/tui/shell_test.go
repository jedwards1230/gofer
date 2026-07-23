package tui

// shell_test.go covers the `!` / `!!` shell escape (shell.go) at the unit
// level: the submit-buffer grammar, the off-loop runner's four robustness
// cases (non-zero exit, interleaved streams, an unbounded printer, a command
// that never exits), the transcript-block and mode-indicator rendering, and —
// the load-bearing one — the `!!` context exclusion asserted at the PROMPT,
// not at the pixels. A test that only checked the rendering would pass even if
// the excluded output leaked into every prompt, which is the exact bug `!!`
// exists to prevent. White-box (package tui) because every one of those seams
// is unexported.

import (
	"strings"
	"testing"
	"time"

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

// TestGoldenShellRunBlock pins the transcript block a completed `!` / `!!` run
// renders as (round-5): the sigil as the marker (`!` / `!!`, distinct colors),
// the command header, output under the `└` gutter, and an `exit N` line iff the
// exit was non-zero — no `$`, no `· not sent to the agent` text (the `!!` marker
// carries that signal now). Rendered through the Model transcript the same way
// every other block is, so the block reads as part of the conversation.
func TestGoldenShellRunBlock(t *testing.T) {
	m := shellRunModel(theme.Test())
	got := m.View(testkit.Width, testkit.Height)
	testkit.AssertGolden(t, "shell_run_block", got)
}

func TestGoldenShellRunBlockStyled(t *testing.T) {
	m := shellRunModel(testkit.ColorTheme())
	got := m.View(testkit.Width, testkit.Height)
	testkit.AssertGoldenStyled(t, "shell_run_block", got)
}

// shellRunModel is the shared fixture for the two goldens above: a fresh
// transcript carrying a sent `!` run and a withheld, failed `!!` run, composed
// the way App.render composes them.
func shellRunModel(th theme.Theme) Model {
	ok := finishedRun(1, "git status --short", " M internal/tui/app.go\n?? internal/tui/shell.go", true)
	failed := finishedRun(2, "cat missing.txt", "cat: missing.txt: No such file or directory", false)
	failed.exitCode = 1
	// A `!` run that exits non-zero with NO output — covers the bare `└ exit 1`
	// outcome line (round-5): the exit line renders iff the code is non-zero.
	exitOnly := finishedRun(3, "false", "", true)
	exitOnly.exitCode = 1
	return New(th).WithShellRuns([]shellRun{ok, failed, exitOnly})
}

// TestGoldenShellRunRunning pins the in-flight render: the sigil marker + command
// header (`! make test`) and a muted "running…" line, no output or outcome yet.
func TestGoldenShellRunRunning(t *testing.T) {
	m := New(theme.Test()).WithShellRuns([]shellRun{{seq: 1, line: "make test", inContext: true}})
	testkit.AssertGolden(t, "shell_run_running", m.View(testkit.Width, testkit.Height))
}

// TestWithShellRunsSkipsConsumedRuns is the anti-duplication guard: a `!` run
// whose output has already been folded into a prompt (consumed) must NOT also
// render as a shell block, because its content now arrives as the echoed user
// message. A `!!` run clears on consume too — it was never in the thread to
// begin with.
func TestWithShellRunsSkipsConsumedRuns(t *testing.T) {
	sent := finishedRun(1, "echo SENT", "SENT-OUTPUT", true)
	sent.consumed = true
	withheld := finishedRun(2, "echo HELD", "HELD-OUTPUT", false)
	withheld.consumed = true

	m := New(theme.Test()).WithShellRuns([]shellRun{sent, withheld})
	got := m.View(testkit.Width, testkit.Height)
	for _, unwanted := range []string{"echo SENT", "SENT-OUTPUT", "echo HELD", "HELD-OUTPUT"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("a consumed run still rendered %q:\n%s", unwanted, got)
		}
	}
}

// TestWithShellRunsRendersOnlyUnconsumed pairs with the guard above: an
// unconsumed run IS shown, so the two together prove the filter turns on the
// consumed flag specifically, not on something incidental.
func TestWithShellRunsRendersOnlyUnconsumed(t *testing.T) {
	live := finishedRun(1, "echo LIVE", "LIVE-OUTPUT", true)
	m := New(theme.Test()).WithShellRuns([]shellRun{live})
	got := m.View(testkit.Width, testkit.Height)
	if !strings.Contains(got, "LIVE-OUTPUT") {
		t.Fatalf("an unconsumed run did not render its output:\n%s", got)
	}
}

// TestWithShellRunsNoVisibleRunsLeavesModelUntouched: a transcript with only
// consumed runs renders byte-for-byte as one with no runs at all, so a session
// that has moved past its shell commands looks like it never ran any.
func TestWithShellRunsNoVisibleRunsLeavesModelUntouched(t *testing.T) {
	consumed := finishedRun(1, "echo hi", "hi", true)
	consumed.consumed = true
	base := New(theme.Test())
	if got, want := base.WithShellRuns([]shellRun{consumed}).View(testkit.Width, testkit.Height), base.View(testkit.Width, testkit.Height); got != want {
		t.Fatalf("a consumed-only run list changed the render:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestWithShellRunsDegenerateSizes is the first-frame guard this repo's panic
// history demands: composing a shell block and rendering it must survive the
// zero/negative sizes App renders at before WindowSizeMsg arrives. (The pinned
// footer legitimately exceeds a 1-row height — that is Model.view's own
// bottom-anchoring, unrelated to the block — so this asserts no panic and a
// non-empty frame, not a row budget.)
func TestWithShellRunsDegenerateSizes(t *testing.T) {
	m := shellRunModel(theme.Test())
	for _, size := range []struct{ w, h int }{{0, 0}, {-1, -1}, {1, 1}, {testkit.Width, 1}, {1, testkit.Height}} {
		if got := m.View(size.w, size.h); got == "" && size.h >= 1 && size.w >= 1 {
			t.Fatalf("View(%d,%d) rendered nothing", size.w, size.h)
		}
	}
}

// TestShellMarkerDistinctAndDisplayOnly is the round-5 SAFETY guard: the sigil
// is the block marker, and a `!!` (private) run's marker must be visually
// UNMISTAKABLE apart from a `!` run's — a distinct glyph AND a distinct color —
// because that marker is now the only at-a-glance "the agent can't see this"
// signal (the `· not sent to the agent` text line is gone). The glyph is derived
// from r.inContext only, never from what composePrompt actually sends.
func TestShellMarkerDistinctAndDisplayOnly(t *testing.T) {
	m := New(testkit.ColorTheme()) // real colors, so the styles emit distinct SGR

	shared := shellRun{line: "ls", inContext: true}
	private := shellRun{line: "cat secret", inContext: false}

	sg, ss := m.shellMarker(shared)
	pg, ps := m.shellMarker(private)

	if sg != "!" {
		t.Errorf("`!` run glyph = %q, want %q", sg, "!")
	}
	if pg != "!!" {
		t.Errorf("`!!` run glyph = %q, want %q — the doubled sigil is the private-run marker", pg, "!!")
	}
	// Distinct color: the two marker styles must render the SAME text differently,
	// so the private run is unmistakable even where the glyph width alone might be
	// missed. Render a fixed probe through each style and require they differ.
	if ss.Render("X") == ps.Render("X") {
		t.Errorf("`!` and `!!` markers render identically (%q); the private run is not visually distinct", ss.Render("X"))
	}
}

// TestGoldenShellModeInput locks the whole attach input box in shell mode: the
// labeled top rule and the accented prompt glyph, the ask-#1 affordance
// composed exactly as a user sees it while typing a `!!` command.
func TestGoldenShellModeInput(t *testing.T) {
	m := New(theme.Test()).SetInput("!!rm -rf /tmp/scratch")
	testkit.AssertGolden(t, "shell_mode_input", m.View(testkit.Width, testkit.Height))
}

func TestGoldenShellModeInputStyled(t *testing.T) {
	m := New(testkit.ColorTheme()).SetInput("!grep -r TODO .")
	testkit.AssertGoldenStyled(t, "shell_mode_input", m.View(testkit.Width, testkit.Height))
}

// TestGoldenShellModeInputBare locks the round-4 fix at the whole-box level: a
// freshly typed `!` with no command yet still shows the separator space before
// the caret (`> ! ▏`), so the space is visible from the first keystroke rather
// than only once a command char is added.
func TestGoldenShellModeInputBare(t *testing.T) {
	m := New(theme.Test()).SetInput("!")
	testkit.AssertGolden(t, "shell_mode_input_bare", m.View(testkit.Width, testkit.Height))
}

// TestShellInputLineAccentsSigil is the ask-#1 assertion: the leading `!` /
// `!!` sigil in the input buffer carries the accent SGR so it reads apart from
// the command being typed, while a non-shell buffer is byte-for-byte
// [inputBuffer.Render] (zero accent, zero golden churn).
func TestShellInputLineAccentsSigil(t *testing.T) {
	th := testkit.ColorTheme() // TrueColor, so AccentStyle actually emits SGR

	buf := inputBuffer{}.SetText("!ls -la")
	if got, want := shellInputLine(th, buf, "▏"), th.AccentStyle().Render("!"); !strings.Contains(got, want) {
		t.Errorf("shellInputLine(%q) = %q, want it to accent the `!` sigil (%q)", buf.String(), got, want)
	}

	buf2 := inputBuffer{}.SetText("!!rm -rf /tmp")
	if got, want := shellInputLine(th, buf2, "▏"), th.AccentStyle().Render("!!"); !strings.Contains(got, want) {
		t.Errorf("shellInputLine(%q) = %q, want it to accent the `!!` sigil (%q)", buf2.String(), got, want)
	}

	plain := inputBuffer{}.SetText("hello world")
	if got, want := shellInputLine(th, plain, "▏"), plain.Render("▏"); got != want {
		t.Errorf("shellInputLine(non-shell) = %q, want inputBuffer.Render verbatim %q", got, want)
	}
}

// TestShellInputLineAsciiSpacesSigilAndKeepsCursor pins the splice under the
// Ascii profile (accent is a no-op): shellInputLine renders the sigil, then a
// single ALWAYS-PRESENT DISPLAY-ONLY space (ask #3 + round-4: the gap shows for
// a bare `!` / `!!` too), with the caret at its exact rune position. A caret at
// or past the sigil/command boundary lands AFTER the gap (`!`→`! ▏`, `!ls` caret
// before `l`→`! ▏ls`); a caret strictly inside `!!` keeps the gap trailing the
// sigil (`!▏! rm`). The expected string is reconstructed from the spec so a
// mis-splice at any cursor position — a dropped/doubled space, a caret off by a
// rune, or the boundary caret landing before the gap — goes red.
func TestShellInputLineAsciiSpacesSigilAndKeepsCursor(t *testing.T) {
	th := theme.Test()
	for _, s := range []string{"!ls", "!!rm -rf", "!", "!!"} {
		sigilLen := 1
		if strings.HasPrefix(s, "!!") {
			sigilLen = 2
		}
		const gap = " " // always present, even with no command yet
		for cur := 0; cur <= len([]rune(s)); cur++ {
			buf := inputBuffer{}.SetTextCursor(s, cur)
			var want string
			if cur < sigilLen {
				want = s[:cur] + "▏" + s[cur:sigilLen] + gap + s[sigilLen:]
			} else {
				want = s[:sigilLen] + gap + s[sigilLen:cur] + "▏" + s[cur:]
			}
			if got := shellInputLine(th, buf, "▏"); got != want {
				t.Errorf("shellInputLine(%q, cur=%d) = %q, want %q", s, cur, got, want)
			}
		}
	}
}

// TestShellInputLineBareSigilShowsGap is the round-4 assertion, isolated: a
// freshly typed `!` (cursor at the end, no command yet) renders `! ▏` — the
// separator space visible before any command char — and `!!` renders `!! ▏`.
// Neutralizing the always-gap (only spacing once a command follows) turns this
// red.
func TestShellInputLineBareSigilShowsGap(t *testing.T) {
	th := theme.Test()
	for _, tc := range []struct{ in, want string }{
		{"!", "! ▏"},
		{"!!", "!! ▏"},
	} {
		buf := inputBuffer{}.SetText(tc.in)
		if got := shellInputLine(th, buf, "▏"); got != tc.want {
			t.Errorf("shellInputLine(%q) = %q, want %q — the gap must show before any command", tc.in, got, tc.want)
		}
	}
}

// TestShellInputLineSpaceIsDisplayOnly is the ask-#3 invariant: the gap is a
// rendering concern only. Rendering the line neither mutates buf nor changes
// what [parseShellEscape] hands the shell, so a user who typed `!ls docs` still
// runs exactly `ls docs` — the single internal space, no leading space from the
// gap. Neutralizing the display-only property (routing the gap back through buf,
// or letting it into the parsed command) turns the parse assertions red.
func TestShellInputLineSpaceIsDisplayOnly(t *testing.T) {
	const typed = "!ls docs"
	buf := inputBuffer{}.SetText(typed)

	// The rendered line carries the display gap (Ascii: `! ls docs` + caret).
	got := shellInputLine(theme.Test(), buf, "▏")
	if !strings.Contains(got, "! ls docs") {
		t.Errorf("shellInputLine(%q) = %q, want the display gap `! ls docs`", typed, got)
	}

	// Rendering did not mutate the buffer…
	if buf.String() != typed {
		t.Errorf("buf mutated by render: %q, want %q", buf.String(), typed)
	}
	// …and the parse of the buffer carries no injected space: exactly `ls docs`.
	if line, _, ok := parseShellEscape(buf.String()); !ok || line != "ls docs" {
		t.Errorf("parseShellEscape(%q) = (%q, ok=%v), want (\"ls docs\", true) — the display gap must not reach the command", buf.String(), line, ok)
	}
	// The explicit-space spelling parses identically: `!ls docs` and `! ls docs`
	// run the same command, which is what "display-only" has to mean downstream.
	if line, _, ok := parseShellEscape("! ls docs"); !ok || line != "ls docs" {
		t.Errorf("parseShellEscape(%q) = (%q, ok=%v), want (\"ls docs\", true)", "! ls docs", line, ok)
	}
}

// TestGoldenShellModeInputStyledSigil is the styled golden's assertion twin: the
// whole attach input box in shell mode carries the accent SGR on the sigil, not
// only on the prompt glyph.
func TestGoldenShellModeInputStyledSigil(t *testing.T) {
	th := testkit.ColorTheme()
	m := New(th).SetInput("!grep -r TODO .")
	got := m.View(testkit.Width, testkit.Height)
	if want := th.AccentStyle().Render("!"); !strings.Contains(got, want) {
		t.Errorf("styled shell-mode input did not accent the `!` sigil (want %q):\n%s", want, got)
	}
}

// TestShellRunBlockNoDispositionLine is the round-5 assertion: no shell block
// prints a `· not sent to the agent` disposition TEXT line anymore — the sigil
// marker carries the private-run signal now (see TestShellMarkerDistinctAndDisplayOnly).
// The proof that the signal MOVED rather than vanished: the `!!` run's block
// leads with the `!!` sigil marker, and the `!` run's with a single `!`.
func TestShellRunBlockNoDispositionLine(t *testing.T) {
	private := finishedRun(1, "cat secret", "x", false) // !! run
	out := New(theme.Test()).WithShellRuns([]shellRun{private}).View(testkit.Width, testkit.Height)
	if strings.Contains(out, "not sent to the agent") {
		t.Errorf("round-5 removed the disposition TEXT line, but the block still prints it:\n%s", out)
	}
	if !strings.Contains(out, "!! cat secret") {
		t.Errorf("`!!` run block does not lead with the `!!` sigil marker:\n%s", out)
	}

	shared := finishedRun(2, "echo hi", "hi", true)
	out2 := New(theme.Test()).WithShellRuns([]shellRun{shared}).View(testkit.Width, testkit.Height)
	if !strings.Contains(out2, "! echo hi") {
		t.Errorf("`!` run block does not lead with the `!` sigil marker:\n%s", out2)
	}
}

// TestShellRunStatusDisposition pins the transcript-less acknowledgement
// (overview/peek): a clean `!` run reports success and where its output went, a
// `!!` run reports it was withheld, and a failed run leads with the failure.
func TestShellRunStatusDisposition(t *testing.T) {
	if sev, note := finishedRun(1, "ls", "", true).shellRunStatus(); sev != sevOK || !strings.Contains(note, "sent with your next message") {
		t.Errorf("clean `!` run status = (%v, %q), want ok + sent disposition", sev, note)
	}
	if sev, note := finishedRun(2, "cat s", "", false).shellRunStatus(); sev != sevOK || !strings.Contains(note, "not sent to the agent") {
		t.Errorf("clean `!!` run status = (%v, %q), want ok + not-sent disposition", sev, note)
	}
	bad := finishedRun(3, "false", "", true)
	bad.exitCode = 1
	if sev, note := bad.shellRunStatus(); sev != sevWarn || !strings.Contains(note, "exited 1") {
		t.Errorf("failed run status = (%v, %q), want warn + exit code", sev, note)
	}
}
