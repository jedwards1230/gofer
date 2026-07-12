// Command gofer is the CLI entrypoint for the gofer agent platform. At M0 it
// offers a single working command, `demo`, which streams a deterministic
// faux-provider session through the SDK's typed event contract — the daemon,
// supervisor, and TUI land in later milestones.
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if len(args) == 0 {
		usage(stderr)
		return 2
	}

	cmd, rest := args[0], args[1:]
	switch cmd {
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

// reportCmdErr prints a command error to stderr and returns the process exit
// code: 2 for a *usageError, 1 for anything else.
func reportCmdErr(cmd string, err error, stderr io.Writer) int {
	_, _ = fmt.Fprintf(stderr, "gofer %s: %v\n", cmd, err)
	var uerr *usageError
	if errors.As(err, &uerr) {
		return 2
	}
	return 1
}

// usage writes the command listing to w.
func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `gofer — supervise coding agents (M0 scaffold)

Usage:
  gofer <command> [flags]

Commands:
  demo      Stream a faux-provider session through the SDK event contract
  login     Authenticate a provider (OAuth by default, --api-key for a static key)
  logout    Remove a provider's stored credential
  auth      Show configured providers and credential status (default: status)
  version   Print the gofer version
  help      Show this help

Run "gofer login <anthropic|openai>" to start a subscription OAuth login, or
"gofer login <provider> --api-key" to store a static key read from stdin.
`)
}
