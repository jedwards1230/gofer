package tui

// shell.go implements the `!` / `!!` input prefix: a shell escape typed into
// either text-entry surface (the overview dispatch bar, the attach input),
// dispatched through the same first-rune switch `/` goes through (see
// [App.dispatchInput], command.go).
//
// `!cmd` runs cmd and folds its output into the NEXT prompt this client
// submits — the user ran it so the agent can see it. `!!cmd` runs it and
// shows the operator the same output while keeping it OUT of the model's
// context. That exclusion is the entire point of the `!!` spelling, so it is
// enforced structurally, at the one place transcript-adjacent content becomes
// model input ([App.composePrompt] below), by a flag on the run — not by
// rendering it differently.
//
// A `!` run's DEFAULT is reply-now: on the attach screen it flushes everything
// pending through [App.composePrompt] and fires a turn the instant it finishes,
// so the agent replies immediately. ctrl+r toggles a sticky QUEUE mode
// ([App.shellQueue]) where a `!` run instead waits for the user's next Enter —
// stacking with more commands or a typed message before the agent responds.
// The mode is captured per-run ([shellRun.queued]) so a run's disposition is
// fixed at dispatch, not re-read later. `!!` ignores the toggle entirely: it is
// never sent regardless, so reply-now vs queue only governs `!`.
//
// Presentation is legibility, never policy. A completed run renders INTO the
// attach transcript as an [itemShellRun] block (composed per frame by
// [Model.WithShellRuns], the same render-local pattern the background-agents
// block uses), so the command and its output read as part of the conversation
// rather than in a pane below it. The block LABELS its disposition only when
// there is something to say — a queued `!` run as "queued", a `!!` run as "not
// sent to the agent"; a reply-now `!` run carries no line, because it is sent
// and answered the moment it finishes. But the label is read off
// [shellRun.inContext]/[shellRun.queued] for display only; [App.composePrompt]
// remains the SOLE decider of what actually reaches the model, so no view
// change can move a byte in or out of context. While the buffer is still being
// typed, both input surfaces flag shell mode ([shellModeRule],
// [shellPromptGlyph], [shellInputLine]) with an accented, labeled framing rule
// that clears the instant the `!` prefix stops. A `!` rule always spells out its
// live reply-now/queue disposition AND the ctrl+r toggle that flips it, in both
// states, so the mode is never a hidden setting; the startup default is
// config.TUI.ShellReplyMode (ctrl+r flips it for the session). The `!` / `!!`
// sigil is accented and a display-only space separates it from the command, so
// the sigil reads apart from what is being typed — presentation only, never a
// change to what [parseShellEscape] hands the shell.
//
// NOT a tool call, and deliberately not routed through anything that resembles
// one: this is the user running a command in their own terminal, at their own
// explicit keystroke, in this client's process. It never reaches the SDK's
// permission guard, the approval overlay, or the daemon — the guard exists to
// gate MODEL-initiated tool calls, and asking a human to approve the command
// they just typed would be theater. A future reader should read the absence of
// an approval hop here as intentional, not as a hole: nothing the model emits
// can reach this path (it is driven only by [tea.KeyPressMsg] on a submit).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// fallbackShell is the interpreter the escape runs under when $SHELL is unset
// or empty (a bare cron-ish environment, a stripped container). POSIX sh is
// the one interpreter every supported host is guaranteed to have.
const fallbackShell = "/bin/sh"

// shellWaitDelay bounds how long [exec.Cmd.Run] waits for the command's
// output pipes to close after the process itself is gone.
//
// It is load-bearing, not a nicety. Killing the shell does not close the
// pipe: any descendant that outlived it still holds the write end, and
// without a WaitDelay os/exec's copier goroutine blocks reading that pipe
// until every last one of them exits. Two ways that bites — a timeout that
// kills `sh` but not the `sleep` it left behind (CI caught exactly this: the
// 100ms timeout returned after the full 30s), and a command that backgrounds
// something and exits immediately (`!make watch &`), where Run would block
// for the background job's whole lifetime with no timeout involved at all.
// On expiry os/exec closes the pipes itself and Run returns with the output
// collected so far. Deliberately not a config knob: it is not a duration a
// user has an opinion about — tui.shell_timeout_ms is the one they'd reach
// for — it is the grace period between "this command is over" and "stop
// waiting on its stragglers".
const shellWaitDelay = 2 * time.Second

// shellRun is one `!` / `!!` invocation: what was typed, what came back, and
// whether the output is the model's to see.
type shellRun struct {
	// seq matches a dispatched run to its [shellDoneMsg]. A monotonically
	// increasing counter on [App], not the slice index, so a result landing
	// after the pane has been dismissed and refilled can still be discarded
	// rather than applied to the wrong row.
	seq int

	line string // the command as typed, sigil stripped

	// inContext is the `!` vs `!!` decision, and the ONLY thing that decides
	// whether this run's output reaches the model — see [App.composePrompt].
	inContext bool

	// queued captures [App.shellQueue] at dispatch: a queued `!` run waits for
	// the user's next Enter rather than firing a turn on completion (see the
	// [shellDoneMsg] handler in app.go). It governs the auto-send and the
	// block's disposition label ONLY — never context membership, which is
	// inContext's alone. A `!!` run ignores it (it is never sent regardless).
	queued bool

	done      bool // the process exited (or failed to start / timed out)
	output    string
	truncated bool // output hit config's tui.shell_max_output_bytes cap
	exitCode  int
	note      string // non-empty when the run never produced an exit code

	// consumed records that this run has already had its turn at a prompt, so
	// a second submit doesn't re-send the same output. Set for EVERY finished
	// run the assembly walks, `!!` runs included: a `!!` run is consumed
	// without contributing, so it can never be picked up later either.
	consumed bool
}

// shellDoneMsg carries a finished shell escape back onto the Update loop.
type shellDoneMsg struct {
	seq       int
	output    string
	truncated bool
	exitCode  int
	note      string
}

// hasInputPrefix reports whether buf opens with a sigil the dispatcher claims
// instead of the prompt path: "/" (a slash command) or "!" (a shell escape).
// It is deliberately LEADING-only and un-trimmed — an email address, a pasted
// `git log --oneline | head` with a `!` in it, or a prompt that merely
// mentions `/etc` submits as ordinary text, because the sigil is only a sigil
// at position 0. Both submit intercepts (handleOverviewKey, handleAttachKey)
// gate on this one function so the two can never diverge.
func hasInputPrefix(buf string) bool {
	return strings.HasPrefix(buf, "/") || strings.HasPrefix(buf, "!")
}

// dispatchInput routes a submitted, prefixed buffer to the handler its leading
// sigil names — the first-rune switch docs/TUI.md describes, and the single
// place a future prefix is added. Callers gate on [hasInputPrefix] first, so
// the unprefixed fall-through here is unreachable in practice and returns the
// app untouched rather than inventing a behavior for it.
func (a App) dispatchInput(buf string) (App, tea.Cmd) {
	switch {
	case strings.HasPrefix(buf, "/"):
		return a.dispatchSlash(buf)
	case strings.HasPrefix(buf, "!"):
		return a.dispatchShell(buf)
	}
	return a, nil
}

// parseShellEscape splits a submitted `!`/`!!` buffer into the command line
// and whether its output belongs in the model's context. ok is false for a
// bare `!` or `!!` (or one followed only by whitespace): there is nothing to
// run, and handing an empty string to `sh -c` would spawn a shell that does
// nothing and reports success, which reads as "it worked" for a command the
// user never finished typing.
func parseShellEscape(buf string) (line string, inContext bool, ok bool) {
	rest, found := strings.CutPrefix(buf, "!!")
	inContext = !found
	if !found {
		rest = strings.TrimPrefix(buf, "!")
	}
	line = strings.TrimSpace(rest)
	return line, inContext, line != ""
}

// dispatchShell starts a submitted shell escape: it records a running row (so
// `!sleep 5` shows "running…" in the transcript rather than nothing) and
// returns the [tea.Cmd] that actually runs the command OFF the Update loop —
// same discipline as [App.discoverModelsCmd]. Nothing about the process runs
// here.
func (a App) dispatchShell(buf string) (App, tea.Cmd) {
	line, inContext, ok := parseShellEscape(buf)
	if !ok {
		a.setStatus(sevWarn, "nothing to run — type a command after !")
		return a, nil
	}
	a.shellSeq++
	a.shellRuns = append(append([]shellRun(nil), a.shellRuns...), shellRun{
		seq:       a.shellSeq,
		line:      line,
		inContext: inContext,
		// Freeze the reply-now/queue mode as it stands NOW, so a later toggle
		// can't retroactively change what a dispatched run does. `!!` runs carry
		// it too but never act on it — they are never sent.
		queued: a.shellQueue,
	})
	return a, runShellCmd(a.commandEnv.Cwd, a.shellSeq, line, a.shellTimeout(), a.shellOutputLimit())
}

// applyShellDone folds a finished run's result onto its recorded row. A result
// whose seq no longer matches a live row (the run list was cleared while it
// was in flight) is dropped rather than resurrecting it. The run itself is
// unaffected by whether anyone was looking — a `!` run still owes its output
// to the next prompt regardless of which screen was showing when it finished.
func (a App) applyShellDone(msg shellDoneMsg) App {
	runs := append([]shellRun(nil), a.shellRuns...)
	for i := range runs {
		if runs[i].seq != msg.seq {
			continue
		}
		runs[i].done = true
		runs[i].output = msg.output
		runs[i].truncated = msg.truncated
		runs[i].exitCode = msg.exitCode
		runs[i].note = msg.note
		a.shellRuns = runs
		return a
	}
	return a
}

// shellRunBySeq returns the recorded run with the given seq, for the status
// acknowledgement the [shellDoneMsg] handler posts on a transcript-less screen.
func (a App) shellRunBySeq(seq int) (shellRun, bool) {
	for _, r := range a.shellRuns {
		if r.seq == seq {
			return r, true
		}
	}
	return shellRun{}, false
}

// shellTimeout and shellOutputLimit resolve tui.shell_timeout_ms and
// tui.shell_max_output_bytes off the live config, on every call and never
// cached — the same "always current, never a stale snapshot" contract
// [App.autoscrollEnabled] follows. A nil Config closure or a read error falls
// through to the same built-in defaults an unconfigured gofer runs.
func (a App) shellTimeout() time.Duration {
	if a.commandEnv.Config == nil {
		return config.DefaultShellTimeout
	}
	cfg, err := a.commandEnv.Config()
	if err != nil {
		return config.DefaultShellTimeout
	}
	return cfg.TUI.ShellTimeout()
}

func (a App) shellOutputLimit() int {
	if a.commandEnv.Config == nil {
		return config.DefaultShellMaxOutputBytes
	}
	cfg, err := a.commandEnv.Config()
	if err != nil {
		return config.DefaultShellMaxOutputBytes
	}
	return cfg.TUI.ShellOutputLimitBytes()
}

// shellInterpreter returns the shell the escape runs under: the user's own
// $SHELL, so `!` obeys their aliases-free but otherwise familiar environment,
// falling back to [fallbackShell] when it is unset.
func shellInterpreter() string {
	if sh := os.Getenv("SHELL"); sh != "" {
		return sh
	}
	return fallbackShell
}

// runShellCmd runs line under the user's shell in cwd, off the Update loop,
// and posts the result back as a [shellDoneMsg].
//
// Robustness lives here rather than at the call site, because every one of
// these is a way a real command breaks a TUI: the timeout bounds a command
// that never exits (ctx + [exec.CommandContext], which kills the process on
// expiry); [boundedWriter] bounds one that never stops printing; stdout and
// stderr share ONE writer so the two interleave in arrival order exactly as
// they would in a terminal (os/exec serializes the writes when both streams
// are the same comparable writer, so this is not a race); and a non-zero exit
// is reported as an exit CODE with the command's own stderr already in
// output, never swallowed.
func runShellCmd(cwd string, seq int, line string, timeout time.Duration, limit int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		out := &boundedWriter{limit: limit}
		cmd := exec.CommandContext(ctx, shellInterpreter(), "-c", line)
		cmd.Dir = cwd
		cmd.Stdout = out
		cmd.Stderr = out
		// Never inherit the TUI's stdin: it belongs to bubbletea's input
		// reader, and a command that reads it (an accidental `!cat`) would
		// otherwise steal the operator's keystrokes. An empty stdin makes such
		// a command see EOF and exit instead.
		cmd.Stdin = nil
		// Bound the post-exit pipe wait — see [shellWaitDelay]. Without it
		// the timeout above is advisory only.
		cmd.WaitDelay = shellWaitDelay

		err := cmd.Run()
		msg := shellDoneMsg{seq: seq, output: out.String(), truncated: out.truncated}
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			// Checked before the ExitError branch: a killed process also
			// returns one, and "signal: killed" is a worse answer than
			// naming the bound that killed it.
			msg.note = "timed out after " + timeout.String()
		case err == nil:
		default:
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				msg.exitCode = exitErr.ExitCode()
				break
			}
			// Couldn't start at all (no such shell, cwd gone). There is no
			// exit code to report, so the error text is the whole story.
			msg.note = err.Error()
		}
		return msg
	}
}

// boundedWriter is an [io.Writer] that retains at most limit bytes (limit <= 0
// retains everything) and records whether it dropped any. It always reports
// the full write as accepted: a short write or an error would hand the child
// process a broken pipe mid-run, turning "your command printed a lot" into
// "your command died", which is a strictly worse thing to show the operator.
type boundedWriter struct {
	limit     int
	buf       bytes.Buffer
	truncated bool
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if w.limit <= 0 {
		return w.buf.Write(p)
	}
	room := w.limit - w.buf.Len()
	if room <= 0 {
		w.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		w.truncated = true
		if _, err := w.buf.Write(p[:room]); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	return w.buf.Write(p)
}

func (w *boundedWriter) String() string { return w.buf.String() }

// composePrompt is THE point where locally produced content becomes model
// input: it prepends every finished, unconsumed `!` run's output to prompt and
// marks every finished run consumed (so a second submit never re-sends the
// same output).
//
// `!!` is enforced here, and only here. A run with inContext false is walked,
// marked consumed, and skipped — its bytes never enter the returned string, so
// no amount of rendering, copying, or re-submitting can leak it into a prompt.
// A run still in flight is left alone entirely (neither folded nor consumed):
// it is finished by the time the NEXT prompt goes out, and truncating a
// half-collected buffer into the model's context would be worse than waiting.
//
// A pointer receiver because it must record the consumption; it clones the
// slice first, keeping [App]'s copy-on-write discipline so a stale App copy
// never observes the mutation.
func (a *App) composePrompt(prompt string) string {
	runs := append([]shellRun(nil), a.shellRuns...)
	var b strings.Builder
	for i := range runs {
		if !runs[i].done || runs[i].consumed {
			continue
		}
		runs[i].consumed = true
		if !runs[i].inContext {
			continue
		}
		b.WriteString(runs[i].contextBlock())
	}
	a.shellRuns = runs
	if b.Len() == 0 {
		return prompt
	}
	return b.String() + prompt
}

// contextBlock renders one `!` run the way the model sees it: the command that
// produced the output, the output itself, and — only when there is something
// abnormal to say — the exit code and truncation marker. Deliberately plain
// and small (docs/CLAUDE.md's context-cost discipline): a shell transcript,
// not a wrapper format the model has to learn.
func (r shellRun) contextBlock() string {
	var b strings.Builder
	fmt.Fprintf(&b, "$ %s\n", r.line)
	if r.output != "" {
		b.WriteString(strings.TrimRight(r.output, "\n"))
		b.WriteString("\n")
	}
	if r.note != "" {
		fmt.Fprintf(&b, "[%s]\n", r.note)
	} else if r.exitCode != 0 {
		fmt.Fprintf(&b, "[exit %d]\n", r.exitCode)
	}
	if r.truncated {
		b.WriteString("[output truncated]\n")
	}
	b.WriteString("\n")
	return b.String()
}

// shellDispositionLabel is the one-line disposition a run wears in the
// transcript block ([Model.renderShellRunLines]) and in the transcript-less
// status ack ([shellRun.shellRunStatus]): whether its output is the model's to
// see. It is derived from [shellRun.inContext] for DISPLAY only —
// [App.composePrompt] is what actually includes or excludes the bytes, so this
// label can never be the thing that leaks a `!!` run.
func (r shellRun) shellDispositionLabel() string {
	if r.inContext {
		return "sent with your next message"
	}
	return "not sent to the agent"
}

// blockDisposition is the transcript block's disposition label
// ([Model.renderShellRunLines]) — narrower than [shellRun.shellDispositionLabel]
// because on the attach screen a reply-now `!` run needs no line at all: it is
// sent and answered the instant it finishes, so "sent with your next message"
// would be both wrong and noise. Only the two runs that WAIT wear a label — a
// queued `!` run ("queued") and a `!!` run ("not sent to the agent"). Like
// every disposition here it is read off inContext/queued for DISPLAY only;
// [App.composePrompt] remains the sole decider of what reaches the model.
func (r shellRun) blockDisposition() string {
	switch {
	case !r.inContext:
		return "not sent to the agent"
	case r.queued:
		return "queued"
	default:
		return ""
	}
}

// shellRunStatus is the one-line acknowledgement a finished run posts on the
// STATUS line — used only on screens with no transcript to render it into (the
// overview, peek), so a `!` typed at the dispatch bar still gives feedback that
// it ran and where its output went. On the attach screen the transcript block
// ([Model.WithShellRuns]) carries all of this, so the handler skips the note
// there rather than talk over what the reader is looking at.
func (r shellRun) shellRunStatus() (statusSeverity, string) {
	disp := " — " + r.shellDispositionLabel()
	switch {
	case r.note != "":
		// A timed-out / failed-to-start `!` run still folds whatever partial
		// output it collected (composePrompt does not gate on note), so the
		// disposition is as relevant here as on the other branches.
		return sevDanger, fmt.Sprintf("%s: %s%s", r.line, r.note, disp)
	case r.exitCode != 0:
		return sevWarn, fmt.Sprintf("%s exited %d%s", r.line, r.exitCode, disp)
	default:
		return sevOK, "ran " + r.line + disp
	}
}

// shellModeLabel is the chip the input surfaces show while a shell escape is
// being TYPED (before it is run): "" when the buffer is not a shell escape, so
// an ordinary prompt's input frame is byte-identical to what it always drew.
//
// A `!` buffer ALWAYS spells out its live reply-now/queue disposition and the
// ctrl+r toggle that flips it, in BOTH states — never a bare "shell". That is
// the discoverability fix: reply-now used to render an unlabeled "shell" with no
// hint the mode was a choice or that ctrl+r existed, so a user could not find
// the setting at all (the `· queue` half only appeared once they had already
// flipped it). Now reply-now reads "shell · enter runs + replies · ctrl+r to
// queue" and queue reads "shell · queue · enter stacks · ctrl+r to reply", so
// both the current mode and the way to change it are visible before the run.
//
// `!!` is exempt: it is never sent regardless of the toggle, so it keeps its
// own "shell · not sent" chip with no reply/queue wording (the toggle would be
// a lie there). The label is TEXT (it survives the Ascii golden profile); the
// accent is the color-only layer shellModeRule adds on top.
func shellModeLabel(buf string, queue bool) string {
	if !strings.HasPrefix(buf, "!") {
		return ""
	}
	if strings.HasPrefix(buf, "!!") {
		return "shell · not sent"
	}
	if queue {
		return "shell · queue · enter stacks · ctrl+r to reply"
	}
	return "shell · enter runs + replies · ctrl+r to queue"
}

// shellModeRule renders the input box's top framing rule, labeled and accented
// when buf is a shell escape and the plain full-width rule otherwise. Both
// text-entry surfaces (the overview dispatch bar, the attach input) call it in
// place of their own `strings.Repeat("─", width)`, so shell mode reads the
// same wherever a command is typed. The label is TEXT, not just color, so it
// survives the Ascii profile the golden tests force (this TUI's
// "state reads without color" rule); the accent is the color-only layer a
// styled golden pins on top.
func shellModeRule(th theme.Theme, width int, buf string, queue bool) string {
	if width < 1 {
		width = 1
	}
	label := shellModeLabel(buf, queue)
	if label == "" {
		return strings.Repeat("─", width)
	}
	// "── <label> " then fill the remainder with rule, to exactly width cells
	// before styling (styling adds ANSI but no visible width). A width too
	// narrow for the label degrades to a plain accented rule rather than
	// overflowing.
	prefix := "── " + label + " "
	if ansi.StringWidth(prefix) >= width {
		return truncate(th.AccentStyle().Render(strings.Repeat("─", width)), width)
	}
	rule := prefix + strings.Repeat("─", width-ansi.StringWidth(prefix))
	return truncate(th.AccentStyle().Render(rule), width)
}

// shellPromptGlyph renders a text-entry surface's prompt glyph (the overview's
// "❯", the attach input's ">"), accented while the buffer is a shell escape so
// the caret itself signals shell mode alongside the labeled rule above it. A
// non-shell buffer returns the bare glyph, unstyled, exactly as before.
func shellPromptGlyph(th theme.Theme, glyph, buf string) string {
	if strings.HasPrefix(buf, "!") {
		return th.AccentStyle().Render(glyph)
	}
	return glyph
}

// shellInputLine renders buf the way [inputBuffer.Render] does — runes,
// clampCursor, [displaySafe]'d pre/cursor/post halves — but with the leading
// `!` / `!!` sigil accented so the sigil that TRIGGERS shell mode is visually
// distinct from the command the user is entering (ask #1), and a single
// DISPLAY-ONLY space between the sigil and the command so the two read apart
// (`! ls docs`, not `!ls docs` — ask #3). A non-shell buffer returns
// [inputBuffer.Render] verbatim, so an ordinary prompt's input line is
// byte-for-byte what it always drew (zero golden churn) and the accent is
// exactly the color-only layer a styled golden pins on top.
//
// The gap is pure presentation. It is spliced into this rendered line only; it
// never enters buf, and the submit path parses buf.String() ([parseShellEscape],
// not this line), so `!ls` and `! ls` are byte-identical to the shell — the gap
// changes what the operator SEES, never what runs. It is added only when a
// command follows the sigil, so a bare `!` / `!!` gets no dangling trailing
// space.
//
// The cursor glyph is spliced at its actual rune position and is never itself
// accented — it is a separate caret, not part of the sigil — so a cursor
// sitting before the `!`, between the two `!` of `!!`, or out in the command
// text all render correctly: the sigil runes on either side of it keep the
// accent, the caret does not, and the gap sits between the sigil (with its
// caret, if any) and the command (with its caret, if any).
func shellInputLine(th theme.Theme, buf inputBuffer, cursorGlyph string) string {
	s := buf.String()
	if !strings.HasPrefix(s, "!") {
		return buf.Render(cursorGlyph)
	}
	sigilLen := 1
	if strings.HasPrefix(s, "!!") {
		sigilLen = 2
	}
	r := buf.runes()
	cur := clampCursor(buf.Cursor(), len(r))

	accent := func(runes []rune) string {
		if len(runes) == 0 {
			return ""
		}
		return th.AccentStyle().Render(displaySafe(string(runes)))
	}
	plain := func(runes []rune) string { return displaySafe(string(runes)) }

	// The display gap, present only when a command follows the sigil.
	gap := ""
	if len(r) > sigilLen {
		gap = " "
	}

	// The cursor falls inside (or at an edge of) the sigil: accent the sigil
	// runes on either side of the caret, then the gap, then the plain command.
	if cur <= sigilLen {
		return accent(r[:cur]) + cursorGlyph + accent(r[cur:sigilLen]) + gap + plain(r[sigilLen:])
	}
	// The cursor is out in the command text: the whole sigil is accented, the
	// gap follows it, and the caret splices into the plain tail.
	return accent(r[:sigilLen]) + gap + plain(r[sigilLen:cur]) + cursorGlyph + plain(r[cur:])
}
