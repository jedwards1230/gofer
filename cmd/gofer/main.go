// Command gofer is the CLI entrypoint for the gofer agent platform. Bare
// `gofer` on an interactive terminal launches the roster overview TUI,
// preferring a reachable `gofer daemon`'s live roster and falling back to a
// local in-process supervisor only when none is reachable; `gofer attach`
// launches the same TUI but requires a daemon. `run` and `resume` drive a
// real session — a real provider, the builtin tool set, and a durable JSONL
// journal — through the SDK's typed event contract, optionally routed
// through a reachable `gofer daemon`; `demo` still streams a deterministic
// faux-provider session with no network.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// run dispatches a subcommand and returns a process exit code: 0 on success, 1
// on a command error, 2 on a usage error. It takes its streams as arguments so
// the dispatch is exercisable without touching the real stdio.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	// No SIGINT-capturing context is installed here: commands that acquire a
	// prompt from an interactive stdin read (run, bare gofer) must install it
	// only AFTER the read (see interruptCtx), so Ctrl-C during a blocking,
	// non-ctx-aware read terminates the process via Go's default handling
	// instead of being swallowed. Commands that stream install it themselves.
	ctx := context.Background()

	// Bare `gofer`, with no subcommand at all, is interactive-terminal aware:
	// on a real TTY it opens the roster overview TUI (runTUI), which prefers a
	// reachable daemon's live roster over the local in-process one — the
	// shortest path from install to supervising sessions; piped/redirected
	// stdin (e.g. `echo prompt | gofer`, or any non-interactive caller) keeps
	// the original M1 behavior unchanged, running one prompt in the current
	// directory (runRun), for scripting and backward compatibility.
	if len(args) == 0 {
		if stdinIsTTY() && interactiveTTY(stdout) {
			if err := runTUI(ctx, stdin, stdout, stderr); err != nil {
				return reportCmdErr("", err, stderr)
			}
			return 0
		}
		if err := runRun(ctx, nil, stdin, stdout, stderr); err != nil {
			return reportCmdErr("", err, stderr)
		}
		return 0
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "run":
		if err := runRun(ctx, rest, stdin, stdout, stderr); err != nil {
			return reportCmdErr("run", err, stderr)
		}
		return 0
	case "resume":
		if err := runResume(ctx, rest, stdin, stdout, stderr); err != nil {
			return reportCmdErr("resume", err, stderr)
		}
		return 0
	case "exec":
		if err := runExec(ctx, rest, stdin, stdout, stderr); err != nil {
			return reportCmdErr("exec", err, stderr)
		}
		return 0
	case "attach", "agents":
		// "agents" is an alias for "attach": same runAttach, same daemon
		// discovery, same code path — see runAttach's doc for why calling it
		// with no <session> positional (the common case for both names) opens
		// the roster overview rather than a specific session's attach screen.
		if err := runAttach(ctx, rest, stdin, stdout, stderr); err != nil {
			return reportCmdErr(cmd, err, stderr)
		}
		return 0
	case "daemon", "serve":
		if err := runDaemon(ctx, rest, stdout, stderr); err != nil {
			return reportCmdErr(cmd, err, stderr)
		}
		return 0
	case "ps":
		if err := runPS(ctx, rest, stdout, stderr); err != nil {
			return reportCmdErr("ps", err, stderr)
		}
		return 0
	case "kill":
		if err := runKill(ctx, rest, stdout, stderr); err != nil {
			return reportCmdErr("kill", err, stderr)
		}
		return 0
	case "archive":
		if err := runArchive(ctx, rest, stdout, stderr); err != nil {
			return reportCmdErr("archive", err, stderr)
		}
		return 0
	case "demo":
		if err := runDemo(ctx, rest, stdout, stderr); err != nil {
			_, _ = fmt.Fprintf(stderr, "gofer demo: %v\n", err)
			return 1
		}
		return 0
	case "login":
		if err := runLogin(ctx, rest, stdin, stdout, stderr); err != nil {
			return reportCmdErr("login", err, stderr)
		}
		return 0
	case "logout":
		if err := runLogout(rest, stdout, stderr); err != nil {
			return reportCmdErr("logout", err, stderr)
		}
		return 0
	case "auth":
		if err := runAuth(rest, stdout, stderr); err != nil {
			return reportCmdErr("auth", err, stderr)
		}
		return 0
	case "version":
		runVersion(stdout)
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		_, _ = fmt.Fprintf(stderr, "gofer: unknown command %q\n\n", cmd)
		usage(stderr)
		return 2
	}
}

// interruptCtx returns a child of ctx cancelled on the first SIGINT (Ctrl-C),
// plus a stop func to release the handler. Streaming commands install it around
// the session run so Ctrl-C interrupts the turn gracefully — but only AFTER any
// interactive prompt read, since a blocking non-ctx-aware read would otherwise
// swallow the signal (the flag package's handler disables Go's default
// terminate-on-SIGINT) and wedge the process.
func interruptCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(ctx, os.Interrupt)
}

// reportCmdErr prints a command error to stderr and returns the process exit
// code: 2 for a *usageError, 1 for anything else. An empty cmd (bare `gofer`)
// prints under the plain "gofer:" prefix.
func reportCmdErr(cmd string, err error, stderr io.Writer) int {
	prefix := "gofer"
	if cmd != "" {
		prefix += " " + cmd
	}
	_, _ = fmt.Fprintf(stderr, "%s: %v\n", prefix, err)
	var uerr *usageError
	if errors.As(err, &uerr) {
		return 2
	}
	return 1
}

// usage writes the command listing to w.
func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `gofer — supervise coding agents

Usage:
  gofer                           Launch the roster TUI (interactive terminal): prefers a
                                   reachable daemon's live roster, falls back to a local
                                   in-process one — pipe a prompt or use "gofer run" for one-shot
  gofer <command> [flags]

Commands:
  run       Start a session and drive one prompt through a real provider
  resume    Reopen a session by id: continue it, or print its transcript
  exec      Headless one-shot: stream JSONL events for one prompt (-p, --agent, --output-schema)
  attach    Open the roster TUI against a running daemon (requires one)
  agents    Alias for "attach" with no <session>: open the roster overview
  daemon    Run the supervisor behind an ACP-over-WebSocket listener (alias: serve);
            "daemon install|uninstall|status" manage a launchd/systemd unit so it
            starts on login
  ps        List sessions on a running daemon's roster (--all: include archived)
  kill      Interrupt and drop a live session from the roster (journal kept)
  archive   Drop a finished session from the roster (journal kept)
  demo      Stream a faux-provider session through the SDK event contract
  login     Authenticate a provider (OAuth by default, --api-key for a static key)
  logout    Remove a provider's stored credential
  auth      Show configured providers and credential status (default: status)
  version   Print the gofer version
  help      Show this help

Run "gofer run --help" / "gofer resume --help" / "gofer demo --help" for
per-command flags. Run "gofer login <anthropic|openai>" to start a
subscription OAuth login, or "gofer login <provider> --api-key" to store a
static key read from stdin.

Model (-m): gofer ships with no default vendor. With -m omitted, "run" and
"resume <id> <prompt>" use the sole logged-in provider's model; log in to
more than one and -m is required; log in to none and login is required
first ("gofer login").

Daemon discovery (ps/kill/archive/attach/agents, and run/resume/bare-gofer
when one is reachable): the address and token are resolved in order —
(1) an explicit --daemon/--token flag, (2) $GOFER_DAEMON/$GOFER_TOKEN,
(3) the endpoint a running "gofer daemon" advertised at <root>/daemon.json,
(4) the loopback default 127.0.0.1:7333. So on the same host, no flags are
usually needed at all once a daemon is up. "run"/"resume" auto-detect a
daemon and route through it (pass --local / --no-daemon to force the
in-process path even when one is up); bare "gofer" auto-detects one too,
falling back to the local roster TUI when none is reachable;
"ps"/"kill"/"archive"/"attach"/"agents" always require one. A daemon and a
client given the SAME --root discover each other automatically — "run" and
"resume" read the endpoint file at their own --root (default ~/.gofer);
"ps"/"kill"/"archive"/"attach"/"agents" and bare "gofer" have no --root of
their own, so they always look at the default ~/.gofer (use "gofer
attach"/"gofer agents" --daemon/--token to point at a daemon on a
non-default --root instead).
`)
}
