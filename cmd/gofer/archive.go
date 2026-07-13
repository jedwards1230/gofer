package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// runArchive implements `gofer archive <id>`: drops a finished (needs-input,
// no queued work) session from the roster, keeping its journal. id may be any
// unambiguous prefix of a live session's id (see [resolveSessionID]).
func runArchive(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("archive", flag.ContinueOnError)
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
		return &usageError{msg: "usage: gofer archive <id>"}
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

	if _, err := c.Call(ctx, "gofer/archive", map[string]string{"sessionId": id}); err != nil {
		return archiveErr(id, err)
	}
	_, _ = fmt.Fprintf(stderr, "gofer archive: %s archived\n", shortID(id))
	return nil
}

// archiveErr wraps a gofer/archive failure for display, adding an explicit
// "kill or interrupt it first" hint on top of the daemon's own message (see
// [supervisor.ErrRunning]) when the session is still active — the one
// archive failure mode a user can act on directly, rather than a generic
// error they'd have to go decode.
func archiveErr(id string, err error) error {
	var callErr *daemon.CallError
	if errors.As(err, &callErr) && strings.Contains(callErr.Message, "running") {
		return fmt.Errorf("archive %s: %s (kill it, or wait for it to finish, then archive)", shortID(id), callErr.Message)
	}
	return fmt.Errorf("archive %s: %w", shortID(id), err)
}
