package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/daemonbridge"
	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// runAttach implements `gofer attach [<session>]`: the daemon-backed roster
// TUI (see docs/PRD.md's CLI surface). Unlike bare `gofer` (runTUI), which
// falls back to the local in-process supervisor when no daemon is reachable,
// attach is explicitly the daemon path — no daemon reachable is a hard
// error, never a silent local fallback (there would be nothing meaningful to
// "attach" to locally: attach's whole point is reaching a daemon's live
// roster, e.g. one a phone ACP client is also driving). The daemon dial (and
// an optional <session> resolution against its live roster) happens
// unconditionally, BEFORE the interactive-terminal check that gates actually
// launching bubbletea — so "no daemon reachable" is always the error a
// non-interactive caller sees, never masked by the terminal requirement, and
// so this command is testable end to end (dial, resolve, construct the App)
// without a real TTY; only the final tea.NewProgram.Run() needs one.
//
// # The <session> argument's current fidelity
//
// A <session> argument, when given, is resolved against the daemon's live
// roster up front (a clear "no such session" error beats a silent no-op) —
// but [tui.NewApp]/[tui.OverviewMeta] have no "open directly into this
// session" affordance as of this M2 leg: the TUI always opens on the
// overview screen (see internal/tui/app.go's NewApp/OverviewMeta and
// docs/M2-PROOF.md §4). `gofer attach <id>` is therefore functionally
// identical to bare `gofer attach` today — a resolved id gets a one-line
// stderr confirmation, then the operator selects it from the overview
// (→/enter on the highlighted row) same as any other session. Landing a real
// deep-link (open straight into the attach screen) needs a small TUI
// affordance — an optional initial-session field on OverviewMeta, or a
// constructor variant — which is out of scope for this leg; flagged for a
// follow-up.
func runAttach(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(stderr)
	df := addDaemonFlags(fs)
	positionals, help, err := parsePositionals(fs, args)
	if err != nil {
		return err
	}
	if help {
		return nil
	}
	if len(positionals) > 1 {
		return &usageError{msg: "usage: gofer attach [<session>]"}
	}

	c, err := dialDaemon(ctx, df)
	if err != nil {
		// attach never falls back to the local path: no daemon reachable is
		// always the caller's problem to fix (start one, or fix --daemon /
		// --token / $GOFER_TOKEN), not something to paper over.
		return daemonDialErr(df.addr, err)
	}
	b := daemonbridge.New(c)
	defer func() { _ = b.Close() }()

	if len(positionals) == 1 {
		id, rerr := resolveSessionID(ctx, c, positionals[0])
		if rerr != nil {
			return rerr
		}
		_, _ = fmt.Fprintf(stderr, "gofer attach: %s is live — select it from the overview (direct-session-open lands in a later M2 leg)\n", shortID(id))
	}

	if !stdinIsTTY() || !interactiveTTY(stdout) {
		return &usageError{msg: "gofer attach requires an interactive terminal"}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}

	_, _ = fmt.Fprintf(stderr, "gofer attach: connected to daemon at %s\n", df.addr)
	app := tui.NewApp(theme.Default(), b, tui.OverviewMeta{
		App:     "gofer",
		Version: version,
		Cwd:     cwd,
		Now:     time.Now(),
	})

	ctx, stop := interruptCtx(ctx)
	defer stop()

	p := tea.NewProgram(app, tea.WithContext(ctx), tea.WithInput(stdin), tea.WithOutput(stdout))
	if _, err := p.Run(); err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}
