package main

import (
	"context"
	"flag"
	"fmt"
	"io"
)

// runKill implements `gofer kill <id>`: interrupts a live session's in-flight
// turn (if any), drops it from the roster, and closes it — the journal is
// never deleted (`gofer resume <id>` still opens it). id may be any
// unambiguous prefix of a live session's id (see [resolveSessionID]).
func runKill(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("kill", flag.ContinueOnError)
	fs.SetOutput(stderr)
	df := addDaemonFlags(fs)
	positionals, help, err := parsePositionals(fs, args)
	if err != nil {
		return err
	}
	if help {
		return nil
	}
	if len(positionals) != 1 {
		return &usageError{msg: "usage: gofer kill <id>"}
	}

	c, err := dialDaemon(ctx, df, "")
	if err != nil {
		return daemonDialErr(df.addr, err)
	}
	defer func() { _ = c.Close() }()

	id, err := resolveSessionID(ctx, c, positionals[0])
	if err != nil {
		return err
	}

	if _, err := c.Call(ctx, "gofer/kill", map[string]string{"sessionId": id}); err != nil {
		return fmt.Errorf("kill %s: %w", shortID(id), err)
	}
	_, _ = fmt.Fprintf(stderr, "gofer kill: %s killed\n", shortID(id))
	return nil
}
