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

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// fallbackShell is the interpreter the escape runs under when $SHELL is unset
// or empty (a bare cron-ish environment, a stripped container). POSIX sh is
// the one interpreter every supported host is guaranteed to have.
const fallbackShell = "/bin/sh"

// shellPaneMaxRows caps how many rows the shell-output pane occupies. Output
// longer than that is tailed (the newest lines win — the end of a command's
// output is what the operator just asked for), with the full text still going
// to the model when the run is a `!` run.
const shellPaneMaxRows = 10

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

// dispatchShell starts a submitted shell escape: it records a running row for
// the pane (so `!sleep 5` shows "running…" rather than nothing) and returns
// the [tea.Cmd] that actually runs the command OFF the Update loop — same
// discipline as [App.discoverModelsCmd]. Nothing about the process runs here.
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
	})
	a.shellOpen = true
	return a, runShellCmd(a.commandEnv.Cwd, a.shellSeq, line, a.shellTimeout(), a.shellOutputLimit())
}

// applyShellDone folds a finished run into the pane. A result whose seq no
// longer matches a live row (the pane was cleared while it was in flight) is
// dropped rather than resurrecting it.
//
// It deliberately does NOT re-open a dismissed pane: an operator who pressed
// Esc on a running `!sleep 30` asked for their screen back, and having the
// overlay reappear half a minute later over whatever they moved on to is the
// opposite of what they asked for. The run itself is unaffected — a `!` run
// still owes its output to the next prompt whether or not anyone looked at
// it.
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

// shellPaneLines renders the shell-output pane: a rule, then the tail of every
// run's block, then a hint row. It mirrors [commandMenu.Lines]' shape — plain
// rules, pre-truncated rows, nil when there is nothing to show — so
// [App.frameLayout] budgets for it exactly the same way. Newest content wins
// when the block overflows maxRows: the end of a command's output is what the
// operator just asked for.
func shellPaneLines(th theme.Theme, runs []shellRun, width, maxRows int) []string {
	if len(runs) == 0 || maxRows < 1 {
		return nil
	}
	if width < 1 {
		width = 1
	}

	var body []string
	for _, r := range runs {
		body = append(body, r.paneLines(th, width)...)
	}
	// Reserve the rule and the hint row out of maxRows before tailing the
	// body, so the pane never renders taller than the budget it was given.
	avail := maxRows - 2
	if avail < 1 {
		return nil
	}
	if len(body) > avail {
		body = body[len(body)-avail:]
	}

	lines := make([]string, 0, len(body)+2)
	lines = append(lines, strings.Repeat("─", width))
	lines = append(lines, body...)
	lines = append(lines, truncate(th.MutedStyle().Render("shell output · esc to dismiss"), width))
	return lines
}

// paneLines renders one run for the pane: a `$ command` header carrying the
// run's context disposition, its output, and its outcome. The `!!` marker is
// shown because the operator needs to know which spelling they used — but it
// is only a LABEL. The exclusion itself is [App.composePrompt]'s, so a
// rendering bug here can never turn into a context leak.
func (r shellRun) paneLines(th theme.Theme, width int) []string {
	header := "$ " + r.line
	if !r.inContext {
		header += "  (not sent to the model)"
	}
	lines := []string{truncate(th.AccentStyle().Render(header), width)}

	if !r.done {
		return append(lines, truncate(th.MutedStyle().Render("  running…"), width))
	}
	for _, l := range strings.Split(strings.TrimRight(r.output, "\n"), "\n") {
		if l == "" && r.output == "" {
			continue
		}
		lines = append(lines, truncate("  "+l, width))
	}
	switch {
	case r.note != "":
		lines = append(lines, truncate(th.DangerStyle().Render("  "+r.note), width))
	case r.exitCode != 0:
		lines = append(lines, truncate(th.DangerStyle().Render(fmt.Sprintf("  exit %d", r.exitCode)), width))
	}
	if r.truncated {
		lines = append(lines, truncate(th.MutedStyle().Render("  … output truncated"), width))
	}
	return lines
}
