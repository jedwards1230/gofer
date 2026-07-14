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

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/daemonbridge"
	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/theme"
	"github.com/jedwards1230/gofer/internal/tuibridge"
)

// runTUI implements bare `gofer` on an interactive terminal: it PREFERS a
// reachable `gofer daemon`'s live roster — a session created from a phone or
// editor ACP client pointed at that daemon appears here too, per
// docs/M2-PROOF.md §4 — and falls back to the local in-process supervisor
// (an in-process store this process itself owns, no daemon involved) only
// when no daemon is reachable at all at the default address. An empty
// roster (no sessions yet, or no provider credentials at all, on the local
// path) is a valid, fully usable starting state either way: the dispatch bar
// creates the first session once the operator types a prompt.
func runTUI(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) error {
	// First-use moment: on a fully interactive terminal with no daemon service
	// installed and none reachable, offer to install one so the daemon starts on
	// login. A complete no-op otherwise (piped stdin, non-tty stdout, CI, or an
	// already-present service/daemon) — see maybePromptDaemonServiceInstall.
	maybePromptDaemonServiceInstall(ctx, stdin, stdout, stderr)

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}

	// Bare `gofer` parses no flags (it is the zero-argument dispatch path in
	// main.go), so it probes whatever [daemonFlags.resolve] discovers with
	// both fields unset: $GOFER_DAEMON/$GOFER_TOKEN, else the endpoint file a
	// running `gofer daemon` advertised, else the loopback default — the same
	// precedence ps/kill/archive/attach/agents use with an explicit
	// --daemon/--token they didn't pass. An operator wanting a specific
	// non-discoverable address still uses `gofer attach` instead, which does
	// parse --daemon/--token.
	df := &daemonFlags{}
	backend, err := selectTUIBackend(ctx, df, cwd, "")
	if err != nil {
		return err
	}
	defer func() { _ = backend.close() }()
	// Printed on every startup so the operator always knows which roster
	// they're looking at — the local and daemon rosters are two different
	// session stores, never merged (see selectTUIBackend's doc).
	_, _ = fmt.Fprintf(stderr, "gofer: tui backend: %s\n", backend.label)

	app := tui.NewApp(theme.Default(), backend.sup, backend.meta, backend.env)

	// Installed after the backend/app construction (neither blocks on
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

// tuiBackend is the resolved [tui.Supervisor] the roster TUI renders,
// however it was built, plus what [runTUI]/[runAttach] need to report it and
// tear it down cleanly.
type tuiBackend struct {
	sup   tui.Supervisor
	meta  tui.OverviewMeta
	env   tui.CommandEnv
	close func() error
	label string // human-readable, for the startup stderr notice
}

// selectTUIBackend picks the daemon-backed or local in-process backend for
// the roster TUI. It is the seam this leg's daemon-preference logic is
// tested through directly (see tui_app_test.go), without ever launching
// bubbletea: dial a daemon at df's address; a genuinely unreachable one
// (df.addr refused/timed out/nothing listening — [daemon.ErrNoDaemon], the
// same distinction [daemon.Probe] makes) falls back to the local path, while
// one that IS listening but rejected the connection ([daemon.ErrUnauthorized]
// — wrong/missing token) is a hard error: the caller found a real daemon, so
// silently rendering an empty local roster instead would hide a credential
// problem rather than surface it.
//
// The daemon and local rosters are never merged — they are two different
// session stores (the daemon's own, versus this process's ~/.gofer) — so
// exactly one backend renders per invocation, and its label is always
// printed so the operator knows which one they're looking at.
func selectTUIBackend(ctx context.Context, df *daemonFlags, cwd, root string) (tuiBackend, error) {
	// Resolve --root through gofer's own default (~/.gofer, never any SDK
	// default) once, up front, and reuse it everywhere a store root is
	// needed below: the local supervisor's session store, the overview
	// header's credential probe, and the command panel's env (auth.json and
	// config.json are always LOCAL to this operator's machine, independent
	// of whether the roster itself comes from a remote daemon).
	rootDir, err := supervisor.ResolveRoot(root)
	if err != nil {
		return tuiBackend{}, err
	}
	env := buildCommandEnv(rootDir, cwd)

	c, dialErr := dialDaemon(ctx, df, "")
	switch {
	case dialErr == nil:
		b := daemonbridge.New(c)
		return tuiBackend{
			sup:   b,
			close: b.Close,
			label: fmt.Sprintf("daemon at %s", df.addr),
			meta: tui.OverviewMeta{
				App:     "gofer",
				Version: version,
				Cwd:     cwd,
				Now:     time.Now(),
			},
			env: env,
		}, nil
	case !daemonUnreachable(dialErr):
		return tuiBackend{}, daemonDialErr(df.addr, dialErr)
	}

	sup, err := supervisor.New(supervisor.Config{Root: rootDir})
	if err != nil {
		return tuiBackend{}, fmt.Errorf("build supervisor: %w", err)
	}
	return tuiBackend{
		sup:   tuibridge.New(sup),
		close: sup.Close,
		label: "local in-process supervisor (no daemon reachable)",
		meta: tui.OverviewMeta{
			App:     "gofer",
			Version: version,
			Model:   resolveOverviewModel(ctx, rootDir),
			Cwd:     cwd,
			Now:     time.Now(),
		},
		env: env,
	}, nil
}

// buildCommandEnv builds the command panel's read-only data source (see
// [tui.CommandEnv]'s doc): version/cwd/root identity plus lazy wrappers
// around the SDK auth store and gofer's own config loader, both rooted at
// root. Auth reuses newAuthStore (the same store `gofer auth`/`gofer
// login` drive) rather than opening auth.json a second way.
func buildCommandEnv(root, cwd string) tui.CommandEnv {
	return tui.CommandEnv{
		Version: version,
		Cwd:     cwd,
		Root:    root,
		Auth: func() ([]tui.ProviderAuth, error) {
			store, err := newAuthStore(root)
			if err != nil {
				return nil, err
			}
			entries, err := store.Status()
			if err != nil {
				return nil, err
			}
			out := make([]tui.ProviderAuth, len(entries))
			for i, e := range entries {
				out[i] = tui.ProviderAuth{
					Provider: e.Provider,
					Kind:     tui.AuthKind(e.Kind),
					Expires:  e.Expires,
					Expired:  e.Expired,
				}
			}
			return out, nil
		},
		Config: func() (config.Config, error) {
			return config.Load(config.DefaultPath(root))
		},
	}
}

// resolveOverviewModel resolves the model string the LOCAL-backend roster
// TUI's header shows, best-effort: the sole logged-in provider's default
// model when exactly one is credentialed, "" otherwise (zero, or more than
// one). It has no daemon-backend equivalent — a daemon's own default model
// is resolved once at `gofer daemon` startup and is not exposed over
// gofer/roster as a fleet-wide value, so the daemon-backend header's Model
// field is left "" (see selectTUIBackend); per-session model still shows on
// each roster row via [tui.SessionInfo.Model]. Unlike resolveRunModel — where
// "no credential" and "ambiguous" are command/usage errors run/resume fail
// fast on — the overview TUI must open regardless of credential state; an
// empty header model, like an empty roster, is a valid starting point the
// operator resolves by logging in or passing -m to a later `gofer
// run`/session-scoped model override.
func resolveOverviewModel(ctx context.Context, root string) string {
	creds, err := runner.CredentialedProviders(ctx, root)
	if err != nil || len(creds) != 1 {
		return ""
	}
	return runner.DefaultModel(creds[0])
}
