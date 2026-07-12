// Command gofer is the CLI entrypoint for the gofer agent platform. At M0 it
// offers a single working command, `demo`, which streams a deterministic
// faux-provider session through the SDK's typed event contract — the daemon,
// supervisor, and TUI land in later milestones.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches a subcommand and returns a process exit code: 0 on success, 1
// on a command error, 2 on a usage error. It takes its streams as arguments so
// the dispatch is exercisable without touching the real stdio.
func run(args []string, stdout, stderr io.Writer) int {
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

// usage writes the command listing to w.
func usage(w io.Writer) {
	_, _ = fmt.Fprint(w, `gofer — supervise coding agents (M0 scaffold)

Usage:
  gofer <command> [flags]

Commands:
  demo      Stream a faux-provider session through the SDK event contract
  version   Print the gofer version
  help      Show this help

Run "gofer demo --help" for demo flags.
`)
}
