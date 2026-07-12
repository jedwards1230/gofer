package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jedwards1230/gofer/internal/runner"
)

// runResume implements `gofer resume`: it reopens an existing session by id
// and either continues it with a prompt or, given none, prints its current
// transcript and exits — a read-only view of the journal.
func runResume(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(stderr)
	model := fs.String("m", defaultRunModel, "model to run")
	root := fs.String("root", "", "session store root (default ~/.gofer)")
	asJSON := fs.Bool("json", false, "emit each event as JSONL instead of a human-readable transcript")
	if err := fs.Parse(args); err != nil {
		return err
	}

	rest := fs.Args()
	if len(rest) == 0 {
		return fmt.Errorf("missing session id (usage: gofer resume <id> [prompt...])")
	}
	id, promptArgs := rest[0], rest[1:]

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}

	r, err := runner.Resume(ctx, id, runner.Options{
		Root:   *root,
		Cwd:    cwd,
		Model:  *model,
		System: defaultSystemPrompt,
	})
	if err != nil {
		return fmt.Errorf("resume session: %w", err)
	}

	if len(promptArgs) == 0 {
		defer func() { _ = r.Close() }()
		return printTranscript(r, stdout)
	}

	_, _ = fmt.Fprintf(stderr, "gofer resume: session %s\n", r.ID())
	_, _ = fmt.Fprintf(stderr, "gofer resume: journal %s\n", r.JournalPath())

	prompt := strings.Join(promptArgs, " ")
	// promptArgs is non-empty here (see the len(promptArgs) == 0 branch
	// above), so a resume prompt always comes from CLI arguments.
	if useTUI(*asJSON, true, stdout) {
		return driveTUI(ctx, r, prompt, stdout, stderr)
	}
	return driveSession(ctx, r, prompt, *asJSON, stdout, stderr)
}

// printTranscript writes r's current folded context as a plain-text
// transcript, one line per message (or per tool call within a tool round).
func printTranscript(r *runner.Runner, w io.Writer) error {
	for _, cm := range r.Fold() {
		if len(cm.ToolCalls) > 0 {
			for _, tc := range cm.ToolCalls {
				if _, err := fmt.Fprintf(w, "[%s] tool %s(%s) -> %s\n", cm.Role, tc.Name, tc.Input, tc.Result); err != nil {
					return fmt.Errorf("write transcript: %w", err)
				}
			}
			continue
		}
		if _, err := fmt.Fprintf(w, "[%s] %s\n", cm.Role, cm.Content); err != nil {
			return fmt.Errorf("write transcript: %w", err)
		}
	}
	return nil
}
