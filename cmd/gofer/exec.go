package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	sdkexec "github.com/jedwards1230/agent-sdk-go/exec"
	"github.com/jedwards1230/agent-sdk-go/runner"

	"github.com/jedwards1230/gofer/internal/supervisor"
)

// execRunnerOpts, when non-nil, is called with the runner.Options runExec is
// about to pass to runner.New — the test seam that lets tests inject a
// scripted Provider/Tools/IDGen/Clock without a real credential or network
// call. Nil in production.
var execRunnerOpts func(*runner.Options)

// runExec implements `gofer exec`: a headless, one-shot, in-process run —
// never daemon-routed, unlike run/resume/bare gofer. It drives exactly one
// prompt through a real provider and streams the SDK's typed event contract
// as JSONL directly to stdout via [sdkexec.Run]; stdout carries nothing else
// (no session banner, no summary), so the output stays a machine-readable
// pipe. Errors — including a schema mismatch reported as a *sdkexec.SchemaError
// — go to stderr only, via the normal reportCmdErr path.
func runExec(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	fs.SetOutput(stderr)
	prompt := fs.String("p", "", "prompt text (default: read all of stdin)")
	agent := fs.String("agent", "", "agent to run (default: gofer's built-in identity; no agent-manifest registry exists yet, so any other name is an error)")
	asJSON := fs.Bool("json", true, "exec output is always JSONL; only true is accepted")
	outputSchema := fs.String("output-schema", "", "path to a JSON-schema-subset document to validate the run's final text result against")
	model := fs.String("m", "", "model to run (default: the sole logged-in provider's model)")
	root := fs.String("root", "", "session store root (default ~/.gofer)")
	help, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if help {
		return nil
	}
	if fs.NArg() > 0 {
		return &usageError{msg: "gofer exec takes no positional arguments (pass -p, or pipe the prompt on stdin)"}
	}
	if !*asJSON {
		return &usageError{msg: "exec output is always JSONL"}
	}
	if *agent != "" {
		return fmt.Errorf("unknown agent %q: agent manifests are not configured", *agent)
	}

	// Read any schema file and resolve the prompt (both may block on I/O)
	// before installing the interrupt handler — mirrors run.go's ordering:
	// interruptCtx wraps the run only AFTER a blocking, non-ctx-aware read,
	// so Ctrl-C during that read keeps Go's default terminate-on-SIGINT
	// behavior instead of being captured and swallowed.
	var schema []byte
	if *outputSchema != "" {
		schema, err = os.ReadFile(*outputSchema)
		if err != nil {
			return fmt.Errorf("read output schema: %w", err)
		}
	}
	promptText, err := resolveExecPrompt(*prompt, stdin)
	if err != nil {
		return err
	}

	rootDir, err := supervisor.ResolveRoot(*root)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}
	modelID := *model
	if modelID == "" {
		modelID, err = resolveRunModel(ctx, rootDir)
		if err != nil {
			return err
		}
	}

	ctx, stop := interruptCtx(ctx)
	defer stop()

	opts := runner.Options{
		Root:   rootDir,
		Cwd:    cwd,
		Model:  modelID,
		System: defaultSystemPrompt,
	}
	if execRunnerOpts != nil {
		execRunnerOpts(&opts)
	}
	r, err := runner.New(ctx, opts)
	if err != nil {
		return wrapCredentialHint(err)
	}

	_, runErr := sdkexec.Run(ctx, r, promptText, sdkexec.Options{Out: stdout, OutputSchema: schema})
	// sdkexec.Run never closes the session — join Close's error (e.g. a
	// journal-write failure the background consumer observed) with any run
	// error so neither is silently dropped.
	return errors.Join(runErr, r.Close())
}

// resolveExecPrompt returns the exec prompt: flagPrompt (from -p) if given,
// else ALL of stdin, trimmed. This is deliberately NOT resolvePrompt's
// one-line interactive read — exec is a headless, piped contract, so it reads
// stdin to EOF. Empty (after trim) with no -p is a usage error.
func resolveExecPrompt(flagPrompt string, stdin io.Reader) (string, error) {
	if flagPrompt != "" {
		return flagPrompt, nil
	}
	data, err := io.ReadAll(stdin)
	if err != nil {
		return "", fmt.Errorf("read prompt from stdin: %w", err)
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return "", &usageError{msg: "no prompt given (pass -p, or pipe one on stdin)"}
	}
	return text, nil
}
