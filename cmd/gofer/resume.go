package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jedwards1230/agent-sdk-go/provider"
	"github.com/jedwards1230/agent-sdk-go/runner"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// runResume implements `gofer resume`: it reopens an existing session by id
// and either continues it with a prompt or, given none, prints its current
// transcript and exits — a read-only view of the journal. Continuing with a
// prompt routes through a reachable daemon exactly like `gofer run` does (see
// its doc comment for the daemon-path differences); the read-only transcript
// view never touches a daemon at all.
func runResume(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(stderr)
	model := fs.String("m", "", "model to run (default: the sole logged-in provider's model)")
	root := fs.String("root", "", "session store root (default ~/.gofer)")
	asJSON := fs.Bool("json", false, "emit each event as JSONL instead of a human-readable transcript")
	df := addDaemonFlags(fs)
	local := addLocalFlag(fs)
	if help, err := parseFlags(fs, args); err != nil {
		return err
	} else if help {
		return nil
	}

	rest := fs.Args()
	if len(rest) == 0 {
		return &usageError{msg: "missing session id (usage: gofer resume <id> [prompt...])"}
	}
	id, promptArgs := rest[0], rest[1:]

	// Resolve --root through gofer's own default (~/.gofer, never any SDK
	// default) up front — every path below that touches the session store or
	// an auth store needs an explicit root.
	rootDir, err := supervisor.ResolveRoot(*root)
	if err != nil {
		return err
	}

	if len(promptArgs) == 0 {
		// A read-only transcript view needs no provider and no credential — it
		// reads the journal directly, so `gofer resume <id>` works even with
		// nothing configured, and never touches a daemon.
		msgs, err := runner.Transcript(ctx, id, rootDir)
		if err != nil {
			return err
		}
		return printTranscript(msgs, stdout)
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}

	var daemonClient *daemon.Client
	daemonRunning := false
	if !*local {
		// dialDaemon's own root resolution (via daemon.ReadEndpoint) already
		// falls back through [supervisor.ResolveRoot], so the raw --root flag
		// (possibly "") is passed through unresolved here.
		c, dialErr := dialDaemon(ctx, df, *root)
		switch {
		case dialErr == nil:
			daemonClient = c
			daemonRunning = true
			defer func() { _ = daemonClient.Close() }()
		case !daemonUnreachable(dialErr):
			return daemonDialErr(df.addr, dialErr)
		}
	}
	if daemonRunning {
		noteDaemonDeviations(stderr, "resume", *model, *root, *asJSON)
	}

	var modelID string
	if !daemonRunning {
		modelID = *model
		if modelID == "" {
			modelID, err = resolveRunModel(ctx, rootDir)
			if err != nil {
				return err
			}
		}
	}

	prompt := strings.Join(promptArgs, " ")
	// promptArgs is non-empty here (see the len(promptArgs) == 0 branch
	// above), so a resume prompt always comes from CLI arguments — there is no
	// interactive read, but the interrupt handler is still scoped to the run so
	// Ctrl-C interrupts the continued turn.
	ctx, stop := interruptCtx(ctx)
	defer stop()

	if daemonRunning {
		return driveDaemonSession(ctx, daemonClient, "resume", id, cwd, prompt, *asJSON, stdout, stderr)
	}

	r, err := runner.Resume(ctx, id, runner.Options{
		Root:   rootDir,
		Cwd:    cwd,
		Model:  modelID,
		System: defaultSystemPrompt,
	})
	if err != nil {
		// runner.Resume's errors are already contextual (a "runner: …"
		// message); the one case needing more is a missing credential, where
		// the SDK error is deliberately app-neutral — wrapCredentialHint adds
		// gofer's 'gofer login' remediation back.
		return wrapCredentialHint(err)
	}

	_, _ = fmt.Fprintf(stderr, "gofer resume: session %s\n", r.ID())
	_, _ = fmt.Fprintf(stderr, "gofer resume: journal %s\n", r.JournalPath())

	if useTUI(*asJSON, stdinIsTTY(), interactiveTTY(stdout)) {
		return driveTUI(ctx, r, prompt, stdout, stderr)
	}
	return driveSession(ctx, r, prompt, *asJSON, stdout, stderr)
}

// printTranscript writes msgs as a plain-text transcript, one line per
// content block across the folded messages.
func printTranscript(msgs []provider.Message, w io.Writer) error {
	for _, msg := range msgs {
		for _, b := range msg.Content {
			line, ok := transcriptLine(msg.Role, b)
			if !ok {
				continue
			}
			if _, err := fmt.Fprintln(w, line); err != nil {
				return fmt.Errorf("write transcript: %w", err)
			}
		}
	}
	return nil
}

// transcriptLine renders one content block as a transcript line, or ok=false
// for a block kind with nothing to show.
func transcriptLine(role provider.Role, b provider.ContentBlock) (string, bool) {
	switch b.Type {
	case provider.BlockText:
		return fmt.Sprintf("[%s] %s", role, b.Text), true
	case provider.BlockReasoning:
		return fmt.Sprintf("[%s] (reasoning) %s", role, b.Text), true
	case provider.BlockToolUse:
		return fmt.Sprintf("[%s] tool %s(%s)", role, b.ToolName, b.ToolInput), true
	case provider.BlockToolResult:
		return fmt.Sprintf("[%s] tool_result -> %s", role, b.ToolResult), true
	default:
		return "", false
	}
}
