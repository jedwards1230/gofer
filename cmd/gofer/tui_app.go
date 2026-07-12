package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/runner"

	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/theme"
	"github.com/jedwards1230/gofer/internal/tuibridge"
)

// runTUI implements bare `gofer` on an interactive terminal: it launches the
// local roster overview — the in-process TUI over a supervisor this process
// itself owns, rooted at the default session store (~/.gofer). This is
// deliberately the LOCAL leg only: it neither probes for nor attaches to a
// separately running `gofer daemon` (that unified-roster leg, and `gofer
// attach`, land later in M2 — see docs/PRD.md's CLI surface). An empty
// roster (no sessions yet, or no provider credentials at all) is a valid,
// fully usable starting state: the dispatch bar creates the first session
// once the operator types a prompt and a credential is available.
func runTUI(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}

	sup, err := supervisor.New(supervisor.Config{})
	if err != nil {
		return fmt.Errorf("build supervisor: %w", err)
	}
	defer func() { _ = sup.Close() }()

	// The header's model string is display-only, resolved best-effort — see
	// resolveOverviewModel's doc. It never blocks the TUI from opening: the
	// dispatch bar creates sessions with a zero-value CreateOptions (the
	// supervisor's own credential-driven default), independent of what the
	// header shows here.
	app := tui.NewApp(theme.Default(), tuibridge.New(sup), tui.OverviewMeta{
		App:     "gofer",
		Version: version,
		Model:   resolveOverviewModel(ctx, ""),
		Cwd:     cwd,
		Now:     time.Now(),
	})

	// Installed after supervisor/app construction (neither blocks on
	// interactive input) so Ctrl-C during the run cancels the program
	// cleanly via tea.WithContext, mirroring driveTUI's interrupt handling.
	ctx, stop := interruptCtx(ctx)
	defer stop()

	p := tea.NewProgram(app, tea.WithContext(ctx), tea.WithInput(stdin), tea.WithOutput(stdout))
	if _, err := p.Run(); err != nil && !errors.Is(err, tea.ErrProgramKilled) {
		// A kill via ctx cancellation (the Ctrl-C path above) is an expected
		// exit, not a TUI failure; anything else is genuine.
		return fmt.Errorf("tui: %w", err)
	}
	return nil
}

// resolveOverviewModel resolves the model string the roster TUI's header
// shows, best-effort: the sole logged-in provider's default model when
// exactly one is credentialed, "" otherwise (zero, or more than one). Unlike
// resolveRunModel — where "no credential" and "ambiguous" are command/usage
// errors run/resume fail fast on — the overview TUI must open regardless of
// credential state; an empty header model, like an empty roster, is a valid
// starting point the operator resolves by logging in or passing -m to a
// later `gofer run`/session-scoped model override.
func resolveOverviewModel(ctx context.Context, root string) string {
	creds, err := runner.CredentialedProviders(ctx, root)
	if err != nil || len(creds) != 1 {
		return ""
	}
	return runner.DefaultModel(creds[0])
}
