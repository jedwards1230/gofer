package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/daemonbridge"
	"github.com/jedwards1230/gofer/internal/modelcatalog"
	"github.com/jedwards1230/gofer/internal/supervisor"
	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/theme"
	"github.com/jedwards1230/gofer/internal/tuibridge"
)

// runTUI implements bare `gofer` on an interactive terminal: it PREFERS a
// reachable `gofer daemon`'s live roster — a session created from a phone or
// editor ACP client pointed at that daemon appears here too — and falls back
// to the local in-process supervisor (an in-process store this process
// itself owns, no daemon involved) only
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
	backend, err := selectTUIBackend(ctx, df, cwd, "", stderr)
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
func selectTUIBackend(ctx context.Context, df *daemonFlags, cwd, root string, stderr io.Writer) (tuiBackend, error) {
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

	c, dialErr := dialDaemon(ctx, df, "", stderr)
	switch {
	case dialErr == nil:
		b := daemonbridge.New(c)
		// The panel's /model needs to know the default it writes cannot reach
		// this daemon (see tui.CommandEnv.DaemonBacked) — the local config
		// wrappers above are unchanged either way, since auth.json/config.json
		// are always this machine's.
		env.DaemonBacked = true
		return tuiBackend{
			sup:   b,
			close: b.Close,
			label: fmt.Sprintf("daemon at %s", df.addr),
			meta: tui.OverviewMeta{
				App:     "gofer",
				Version: effectiveVersion(),
				// The DAEMON's default model, read off gofer/hello — not a
				// locally recomputed one. The daemon resolved its own default at
				// startup and every session it creates without an explicit model
				// uses that value, so it is the only answer the header can show
				// that is true of the sessions this backend actually starts.
				Model: daemonDefaultModel(ctx, c),
				Cwd:   cwd,
				Now:   time.Now(),
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
	// ONE resolution feeding both the header and the bridge's create default:
	// the header used to display a model the bridge never passed to Create, so
	// a session started from the TUI reached the runner with an empty model and
	// died on `runner: unknown model ""` while the header cheerfully showed a
	// real one (issue #147). Mirrors the daemon's own
	// [daemon.Config.DefaultModel] shape.
	//
	// The header gets the value resolved NOW (it has to render something), but
	// the bridge gets the RESOLVER, not the value: a default changed later in
	// this process's life — `/model` writing session.model — must reach the
	// next session created, which a string captured here never could (issue
	// #156). The TUI keeps its header in step itself via
	// [tui.Overview.WithDefaultModel]; re-resolving on each create keeps
	// config.json the one source of truth for what actually runs.
	model := resolveOverviewModel(ctx, rootDir)
	return tuiBackend{
		sup: tuibridge.New(sup, func(ctx context.Context) string {
			return resolveOverviewModel(ctx, rootDir)
		}),
		close: sup.Close,
		label: "local in-process supervisor (no daemon reachable)",
		meta: tui.OverviewMeta{
			App:     "gofer",
			Version: effectiveVersion(),
			Model:   model,
			Cwd:     cwd,
			Now:     time.Now(),
		},
		env: env,
	}, nil
}

// daemonDefaultModel reads the connected daemon's own default model off the
// gofer/hello handshake, best-effort: a daemon predating the field (or one
// that never resolved a default) yields "", which the header renders exactly
// as it did before this existed. Non-fatal by design — a header detail must
// never keep the roster from opening.
func daemonDefaultModel(ctx context.Context, c *daemon.Client) string {
	hello, err := c.Hello(ctx)
	if err != nil {
		return ""
	}
	return hello.DefaultModel
}

// modelDiscoveryClient and modelDiscoveryBaseURL are the transport production
// live model discovery runs on. nil/"" are what [modelcatalog.WithDiscovery]
// documents as "http.DefaultClient against the real vendor host", and passing
// http.DefaultClient explicitly means exactly that — with one difference that
// is the entire reason these are variables: a test can pin BOTH, and pinning
// them is what makes the production wiring assertable without touching a
// vendor host.
//
// That matters more than it looks. Discovery is opt-in
// ([modelcatalog.WithDiscovery]'s doc explains why), so a call site that stops
// passing it does not fail, does not warn, and does not change a single test
// result — the feature just silently stops existing. The only way to catch that
// is a test that drives the REAL production call site and observes a request,
// which requires the production client to be swappable. See
// TestProductionCommandEnvPerformsDiscovery.
//
// A non-nil client also pins the SDK auth store's token refresh, not just the
// listing: modelcatalog threads the injected client into auth.WithHTTPClient
// precisely so both calls are pinnable together (see its codexCredential doc).
// With nil, the refresh would fall back to the store's own client and could
// reach a real vendor auth host.
var (
	modelDiscoveryClient  = http.DefaultClient
	modelDiscoveryBaseURL = ""
)

// buildCommandEnv builds the command panel's data source (see
// [tui.CommandEnv]'s doc): version/cwd/root identity plus lazy wrappers
// around the SDK auth store and gofer's own config loader/writer, both
// rooted at root. Auth reuses newAuthStore (the same store `gofer
// auth`/`gofer login` drive) rather than opening auth.json a second way.
//
// Models is the one wrapper that can leave the machine — it enables live
// discovery. Every other closure here is a local file read.
func buildCommandEnv(root, cwd string) tui.CommandEnv {
	return tui.CommandEnv{
		Version: effectiveVersion(),
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
		SaveConfig: func(c config.Config) error {
			return config.Save(config.DefaultPath(root), c)
		},
		Models: func(ctx context.Context, providerID string) ([]modelcatalog.Model, error) {
			// WithDiscovery is REQUIRED here and is the whole point of this
			// closure: without it Catalog is offline and returns the same
			// compiled-in floor the picker already seeded itself with, so
			// /model would silently never show a live model. Catalog absorbs
			// every discovery failure back down to that floor internally, so
			// there is no failure mode to handle at this layer.
			return modelcatalog.Catalog(ctx, root, providerID,
				modelcatalog.WithDiscovery(modelDiscoveryClient, modelDiscoveryBaseURL))
		},
	}
}

// resolveOverviewModel resolves the model the LOCAL-backend roster TUI both
// SHOWS in its header and CREATES sessions with, best-effort. It defers
// wholly to [resolveRunModel] so the TUI, `gofer run`, and `gofer daemon`
// resolve identically — config.Session.Model first, else the sole logged-in
// provider's default — rather than the TUI keeping a second, subtly different
// rule of its own.
//
// The daemon backend has its own equivalent (daemonDefaultModel, off
// gofer/hello): a daemon's default is resolved at ITS startup against ITS
// store, so recomputing one locally could name a model that daemon would
// never use.
//
// Unlike resolveRunModel — where "no credential" and "ambiguous" are
// command/usage errors run/resume fail fast on — the overview TUI must open
// regardless of credential state, so every error collapses to "". An empty
// value here is not a silent failure: the header shows no model, and a
// create attempt fails with the actionable [supervisor.ErrNoModel] rather
// than reaching the runner as an empty model id.
func resolveOverviewModel(ctx context.Context, root string) string {
	model, err := resolveRunModel(ctx, root)
	if err != nil {
		return ""
	}
	return model
}
