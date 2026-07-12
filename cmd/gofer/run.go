package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jedwards1230/agent-sdk-go/runner"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// defaultSystemPrompt is the system prompt a run/resume session uses absent
// a richer agent manifest (a later milestone).
const defaultSystemPrompt = "You are gofer, a careful coding agent. Use your tools to accomplish the user's task."

// errNoProviderCredentials is returned when -m is not given and no provider
// has a credential configured at all. gofer deliberately ships with no
// flagship-vendor default: the run model is resolved from whichever
// provider(s) are actually logged in, never hardcoded to one vendor.
var errNoProviderCredentials = errors.New(noProviderCredentialsMsg())

// noProviderCredentialsMsg builds errNoProviderCredentials' message from
// runner's provider/env-var tables, so the hint can never drift from the
// providers gofer actually supports.
func noProviderCredentialsMsg() string {
	providers := runner.SupportedProviders()
	logins := make([]string, len(providers))
	envVars := make([]string, len(providers))
	for i, p := range providers {
		logins[i] = fmt.Sprintf("'gofer login %s'", p)
		envVars[i] = runner.EnvVar(p)
	}
	return fmt.Sprintf("no provider credentials — run %s (or set %s)",
		strings.Join(logins, " or "), strings.Join(envVars, " / "))
}

// credentialHintError decorates the SDK runner's [runner.NoCredentialError]
// with gofer's own remediation ('gofer login <provider>') — the message the
// now-deleted internal/runner package used to produce directly. The SDK
// package stays app-neutral (it can't know gofer's CLI verb), so gofer adds
// the hint back at the one place its errors surface. It implements Unwrap so
// errors.Is(err, runner.ErrNoCredential) still holds through the wrap.
type credentialHintError struct {
	msg   string
	cause *runner.NoCredentialError
}

func (e *credentialHintError) Error() string { return e.msg }
func (e *credentialHintError) Unwrap() error { return e.cause }

// wrapCredentialHint adds gofer's login hint to a [runner.NoCredentialError],
// leaving any other error (including nil) untouched. Call it around every
// runner.New/runner.Resume error before returning it to reportCmdErr.
func wrapCredentialHint(err error) error {
	var nce *runner.NoCredentialError
	if !errors.As(err, &nce) {
		return err
	}
	msg := fmt.Sprintf("no credential for %s — run 'gofer login %s'", nce.Provider, nce.Provider)
	if nce.EnvVar != "" {
		msg = fmt.Sprintf("%s or set %s", msg, nce.EnvVar)
	}
	return &credentialHintError{msg: msg, cause: nce}
}

// ambiguousModelMsg is the usage-error message when -m is not given and more
// than one provider has a credential configured: gofer picks no favorite
// among logged-in providers, so the caller must say which model to run.
func ambiguousModelMsg(creds []string) string {
	models := make([]string, len(creds))
	for i, p := range creds {
		models[i] = runner.DefaultModel(p)
	}
	return fmt.Sprintf("multiple providers have credentials — pass -m (e.g. -m %s)", strings.Join(models, " or -m "))
}

// resolveRunModel picks the model `gofer run`/`gofer resume` uses when -m is
// not given: the sole logged-in provider's default model. No credentials is
// a command error (exit 1); more than one is a usage error (exit 2) — gofer
// never guesses a vendor to favor.
func resolveRunModel(ctx context.Context, root string) (string, error) {
	creds, err := runner.CredentialedProviders(ctx, root)
	if err != nil {
		return "", err
	}
	switch len(creds) {
	case 1:
		return runner.DefaultModel(creds[0]), nil
	case 0:
		return "", errNoProviderCredentials
	default:
		return "", &usageError{msg: ambiguousModelMsg(creds)}
	}
}

// runRun implements `gofer run` (and bare `gofer`): it starts a fresh
// session rooted at the current directory, drives one prompt — from args, or
// one line read from stdin when none are given — and streams the resulting
// output to stdout. When a `gofer daemon` is reachable at --daemon (default
// 127.0.0.1:7333), the session is driven THROUGH it as an ACP client
// (driveDaemonSession) — no privileged path, the same surface a phone or
// editor client uses (docs/M2-PROOF.md). With no daemon reachable, it falls
// back unchanged to the in-process path: a real provider and the builtin tool
// set via runner.New.
//
// Known daemon-path differences from the in-process path — inherent to M2's
// ACP surface, not oversights (each prints a one-line stderr notice when the
// relevant flag was set, and --local opts out of the daemon entirely):
//
//   - -m is ignored (ACP's session/new carries no model field — the daemon
//     resolved its own default model at startup, see `gofer daemon --model`).
//   - --root is ignored (the daemon uses its own session store, chosen at its
//     startup; --root cannot retarget it).
//   - --json emits ACP's session/update JSON rather than the SDK's event.Event
//     JSON the in-process --json emits.
//   - the interactive attach TUI is never used; the daemon path always plain-
//     streams the turn to stdout.
func runRun(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
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

	promptFromArgs := len(fs.Args()) > 0

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}

	// Dial before any local model/credential resolution: a running daemon
	// needs neither (it already resolved its own default model at startup),
	// so the common "there's a daemon, just use it" case pays no
	// credential-lookup cost at all. A dial failure that ISN'T "nothing is
	// listening" (e.g. a wrong token) is a real problem to surface, not a
	// silent fallback — see [daemonUnreachable]. --local skips the probe
	// outright, forcing the in-process path.
	var daemonClient *daemon.Client
	daemonRunning := false
	if !*local {
		c, dialErr := dialDaemon(ctx, df)
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
		noteDaemonDeviations(stderr, "run", *model, *root, *asJSON)
	}

	// Resolve the model before acquiring the prompt (which may block on an
	// interactive stdin read): a caller with no usable credential should fail
	// fast, not sit at a prompt> indicator first. Skipped entirely on the
	// daemon path — see above.
	var modelID string
	if !daemonRunning {
		modelID = *model
		if modelID == "" {
			var rerr error
			modelID, rerr = resolveRunModel(ctx, *root)
			if rerr != nil {
				return rerr
			}
		}
	}

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

	if daemonRunning {
		return driveDaemonSession(ctx, daemonClient, "run", "", cwd, prompt, *asJSON, stdout, stderr)
	}

	r, err := runner.New(ctx, runner.Options{
		Root:   *root,
		Cwd:    cwd,
		Model:  modelID,
		System: defaultSystemPrompt,
	})
	if err != nil {
		// runner.New's errors are already contextual (a "runner: …" message);
		// the one case needing more is a missing credential, where the SDK
		// error is deliberately app-neutral — wrapCredentialHint adds gofer's
		// 'gofer login' remediation back. Any other error passes through as-is.
		return wrapCredentialHint(err)
	}

	_, _ = fmt.Fprintf(stderr, "gofer run: session %s\n", r.ID())
	_, _ = fmt.Fprintf(stderr, "gofer run: journal %s\n", r.JournalPath())

	if useTUI(*asJSON, stdinIsTTY(), interactiveTTY(stdout)) {
		return driveTUI(ctx, r, prompt, stdout, stderr)
	}
	return driveSession(ctx, r, prompt, *asJSON, stdout, stderr)
}
