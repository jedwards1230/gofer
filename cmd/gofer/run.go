package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/jedwards1230/gofer/internal/runner"
)

// defaultRunModel is the model `gofer run`/`gofer resume` uses when -m is
// not given.
const defaultRunModel = "claude-sonnet-5"

// defaultSystemPrompt is the system prompt a run/resume session uses absent
// a richer agent manifest (a later milestone).
const defaultSystemPrompt = "You are gofer, a careful coding agent. Use your tools to accomplish the user's task."

// runRun implements `gofer run` (and bare `gofer`): it starts a fresh
// session rooted at the current directory, drives one prompt — from args, or
// one line read from stdin when none are given — through a real provider and
// the builtin tool set, and streams the resulting events to stdout.
func runRun(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	model := fs.String("m", defaultRunModel, "model to run")
	root := fs.String("root", "", "session store root (default ~/.gofer)")
	asJSON := fs.Bool("json", false, "emit each event as JSONL instead of a human-readable transcript")
	if help, err := parseFlags(fs, args); err != nil {
		return err
	} else if help {
		return nil
	}

	promptFromArgs := len(fs.Args()) > 0

	// Install the interrupt handler up front and make the prompt read itself
	// cancellable, so Ctrl-C ALWAYS exits promptly — both at the prompt and
	// during the run. The read runs on a goroutine the select abandons on
	// cancellation (signal.Notify overrides any inherited SIG_IGN), so a
	// blocking stdin read can never swallow the signal and wedge the process.
	ctx, stop := interruptCtx(ctx)
	defer stop()

	// On an interactive terminal with no prompt argument, show an explicit
	// indicator rather than silently blocking on the read; a piped stdin reads
	// one line with no indicator.
	if !promptFromArgs && stdinIsTTY() {
		_, _ = fmt.Fprint(stderr, "prompt> ")
	}
	prompt, ok, err := resolvePromptCtx(ctx, fs.Args(), stdin)
	if errors.Is(err, context.Canceled) {
		_, _ = fmt.Fprintln(stderr) // finish the ^C line cleanly
		return nil
	}
	if err != nil {
		return err
	}
	if !ok {
		return &usageError{msg: "no prompt given (pass it as an argument or pipe one line on stdin)"}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}

	r, err := runner.NewSession(ctx, runner.Options{
		Root:   *root,
		Cwd:    cwd,
		Model:  *model,
		System: defaultSystemPrompt,
	})
	if err != nil {
		return fmt.Errorf("start session: %w", err)
	}

	_, _ = fmt.Fprintf(stderr, "gofer run: session %s\n", r.ID())
	_, _ = fmt.Fprintf(stderr, "gofer run: journal %s\n", r.JournalPath())

	if useTUI(*asJSON, promptFromArgs, stdout) {
		return driveTUI(ctx, r, prompt, stdout, stderr)
	}
	return driveSession(ctx, r, prompt, *asJSON, stdout, stderr)
}
