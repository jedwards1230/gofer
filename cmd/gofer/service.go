package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jedwards1230/gofer/internal/daemon"
	"github.com/jedwards1230/gofer/internal/supervisor"
)

// serviceConfig is the platform-independent description of the daemon service
// unit the launchd/systemd renderers turn into a plist/unit file. It carries
// NO token field on purpose: the bearer token never lands in a
// world-readable unit file or in the daemon's argv (visible via `ps`). When a
// non-loopback bind needs a token it is delivered out of band through a 0600
// <Root>/daemon.env file instead (see writeDaemonEnvToken / readDaemonEnvToken).
type serviceConfig struct {
	// Label is the launchd job label (reverse-DNS); used only by
	// renderLaunchdPlist. The systemd renderer keys off a fixed unit name and
	// ignores this field.
	Label string
	// ExecPath is the absolute, symlink-resolved path to this gofer binary
	// (see resolveSelfExec).
	ExecPath string
	// ListenAddr is the address the installed daemon binds.
	ListenAddr string
	// Root is the resolved session-store root (~/.gofer by default) the
	// installed daemon is pinned to, so a service-managed daemon and its
	// clients agree on one store without either relying on an ambient default.
	Root string
	// Args is the daemon's full argv tail after ExecPath, e.g.
	// ["daemon", "--listen", ListenAddr, "--root", Root]. Never contains a
	// token flag.
	Args []string
}

// daemonEnvFileName is the name, within a store root, of the 0600 env file
// that carries the bearer token to a service-managed daemon out of band —
// never through the unit file or argv. See writeDaemonEnvToken.
const daemonEnvFileName = "daemon.env"

// serviceManager abstracts the per-platform launchd/systemd operations the
// install/uninstall/status commands drive, so those commands are exercisable
// with a fake that records load/unload and writes the unit to a temp path
// (see service_test.go) rather than shelling out to launchctl/systemctl. The
// command layer owns unit-file I/O (write on install, remove on uninstall)
// uniformly; a manager only renders the file body and runs the loader.
type serviceManager interface {
	// label identifies the service (launchd job label / systemd unit name).
	label() string
	// unitPath is the absolute path the unit file is written to / read from.
	unitPath() (string, error)
	// render returns the unit-file body for cfg. Deterministic and token-free.
	render(cfg serviceConfig) []byte
	// isInstalled reports whether the unit file exists on disk.
	isInstalled() (bool, error)
	// load registers + starts the service from the already-written unit at path.
	load(ctx context.Context, path string) error
	// unload stops + deregisters the service (the command removes the file).
	unload(ctx context.Context, path string) error
	// reloadAfterRemove lets the manager forget a just-removed unit cleanly. The
	// command layer calls it in runDaemonUninstall AFTER os.Remove(path) succeeds
	// — on systemd this is `daemon-reload` (so the manager drops the deleted
	// unit from memory); launchd and unsupported platforms make it a no-op.
	// Idempotent and tolerant of an already-gone unit.
	reloadAfterRemove(ctx context.Context) error
	// running reports whether the service is currently active.
	running(ctx context.Context) (bool, error)
}

// newServiceManager is the seam tests swap to inject a fake manager. It
// defaults to the active platform's real manager.
var newServiceManager = activeServiceManager

// promptGate computes whether the first-use install prompt should appear. It
// is a package var so a test can force the interactive branch deterministically
// (a *bytes.Buffer is never a real TTY, and the reachability probe dials the
// network). The real implementation short-circuits on the cheap TTY/CI checks
// before touching the manager or the network, then defers the final decision
// to the pure shouldPromptInstall predicate.
var promptGate = func(ctx context.Context, stdout io.Writer, mgr serviceManager) bool {
	stdinTTY, stdoutTTY := stdinIsTTY(), interactiveTTY(stdout)
	ciEnv := os.Getenv("CI") != ""
	if !stdinTTY || !stdoutTTY || ciEnv {
		return false
	}
	installed, _ := mgr.isInstalled()
	reachable := daemonServiceReachable(ctx)
	return shouldPromptInstall(stdinTTY, stdoutTTY, ciEnv, installed, reachable)
}

// shouldPromptInstall is the pure gating predicate for the first-use daemon
// service install prompt: it fires ONLY on a fully interactive terminal
// (stdin AND stdout are TTYs), outside CI, when no service is installed yet
// and no daemon is already reachable. Any one of those being false makes the
// prompt a complete no-op. Kept pure (all inputs are plain bools) so the whole
// truth table is exhaustively testable without touching a TTY, the network, or
// the environment.
func shouldPromptInstall(stdinTTY, stdoutTTY, ciEnv, serviceInstalled, daemonReachable bool) bool {
	return stdinTTY && stdoutTTY && !ciEnv && !serviceInstalled && !daemonReachable
}

// daemonServicePromptText is the exact first-use prompt. The trailing space
// keeps the typed answer on the same line.
const daemonServicePromptText = "No gofer daemon service is installed. Install one so it starts on login? [y/N] "

// maybePromptDaemonServiceInstall shows the first-use install prompt at the
// canonical bare-`gofer` entry (runTUI) and, on an affirmative answer, installs
// + loads the service. It is a COMPLETE no-op — zero output, no stdin read —
// whenever the gate (promptGate) is false: piped stdin, a non-terminal stdout,
// CI, an already-installed service, or a reachable daemon. Any other answer
// (n/blank/EOF) proceeds without installing.
func maybePromptDaemonServiceInstall(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) {
	mgr := newServiceManager()
	if !promptGate(ctx, stdout, mgr) {
		return
	}
	_, _ = fmt.Fprint(stdout, daemonServicePromptText)
	line, err := readLine(stdin)
	if err != nil {
		return
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		if err := runDaemonInstall(ctx, nil, stdout, stderr); err != nil {
			_, _ = fmt.Fprintf(stderr, "gofer: daemon service install failed: %v\n", err)
		}
	default:
		// Any other answer proceeds without installing — the TUI opens as usual.
	}
}

// daemonServiceReachable reports whether a daemon is already reachable at the
// default-root discovery address, so the first-use prompt does not offer to
// install a service for a daemon that is already up. It mirrors the client
// discovery precedence (flag/env/endpoint-file/loopback-default) with no flags
// set, bounded by daemonDialTimeout so a dead address never stalls startup.
func daemonServiceReachable(ctx context.Context) bool {
	df := &daemonFlags{}
	addr, token := df.resolve("")
	dctx, cancel := context.WithTimeout(ctx, daemonDialTimeout)
	defer cancel()
	return daemon.Probe(dctx, addr, token)
}

// runDaemonInstall implements `gofer daemon install [--listen addr] [--root
// dir] [--token tok]`. It writes the platform unit file, delivers a token (if
// any) via the 0600 daemon.env file, and loads the service. A non-loopback
// --listen requires a token (--token or $GOFER_TOKEN), enforced up front by
// daemon.ValidateListen exactly as `gofer daemon` itself does.
func runDaemonInstall(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newDaemonServiceFlagSet("daemon install", stderr)
	listen := fs.String("listen", daemon.DefaultListenAddr, "address the installed daemon binds")
	root := fs.String("root", "", "session store root the installed daemon uses (default ~/.gofer)")
	// Empty default, not os.Getenv("GOFER_TOKEN"): flag.PrintDefaults would
	// otherwise render a token set in the environment into --help output. The
	// env fallback is applied explicitly below. Same rationale as daemon.go.
	token := fs.String("token", "", "bearer token for a non-loopback bind (default: $GOFER_TOKEN)")
	if help, err := parseFlags(fs, args); err != nil {
		return err
	} else if help {
		return nil
	}

	bearerToken := *token
	if bearerToken == "" {
		bearerToken = os.Getenv("GOFER_TOKEN")
	}
	// Fail fast on the misconfiguration daemon.Serve would reject anyway, so
	// the install error is clean and immediate and no unit file is written.
	if err := daemon.ValidateListen(*listen, bearerToken); err != nil {
		return err
	}

	mgr := newServiceManager()
	cfg, err := buildServiceConfig(mgr, *listen, *root)
	if err != nil {
		return err
	}
	path, err := mgr.unitPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create service unit directory: %w", err)
	}
	// launchd's StandardOut/ErrorPath require the logs dir to pre-exist.
	if err := os.MkdirAll(filepath.Join(cfg.Root, "logs"), 0o700); err != nil {
		return fmt.Errorf("create daemon logs directory: %w", err)
	}
	// The unit file is world-readable (0644) and carries no secret.
	if err := atomicWriteFile(path, mgr.render(cfg), 0o644); err != nil {
		return fmt.Errorf("write service unit %s: %w", path, err)
	}
	// A token, when present, travels only through the 0600 env file — never the
	// unit file (above) or the daemon's argv (cfg.Args).
	if bearerToken != "" {
		if err := writeDaemonEnvToken(cfg.Root, bearerToken); err != nil {
			return err
		}
	}
	if err := mgr.load(ctx, path); err != nil {
		return fmt.Errorf("load service %s: %w", mgr.label(), err)
	}

	_, _ = fmt.Fprintf(stdout, "Installed gofer daemon service %q at %s\n", mgr.label(), path)
	_, _ = fmt.Fprintf(stdout, "The daemon will start on login and bind %s.\n", cfg.ListenAddr)
	return nil
}

// runDaemonUninstall implements `gofer daemon uninstall [--root dir]`: it
// unloads the service, removes the unit file, and best-effort removes the 0600
// daemon.env token file. Idempotent — an absent unit is reported, not an error.
func runDaemonUninstall(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newDaemonServiceFlagSet("daemon uninstall", stderr)
	root := fs.String("root", "", "session store root the service used (default ~/.gofer)")
	if help, err := parseFlags(fs, args); err != nil {
		return err
	} else if help {
		return nil
	}

	mgr := newServiceManager()
	path, err := mgr.unitPath()
	if err != nil {
		return err
	}
	installed, err := mgr.isInstalled()
	if err != nil {
		return err
	}
	if !installed {
		_, _ = fmt.Fprintf(stdout, "gofer daemon service %q is not installed.\n", mgr.label())
		return nil
	}

	if err := mgr.unload(ctx, path); err != nil {
		return fmt.Errorf("unload service %s: %w", mgr.label(), err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove service unit %s: %w", path, err)
	}
	// Only now that the unit file is gone: let the manager forget it (systemd
	// daemon-reload; a no-op on launchd). Running this BEFORE the remove would
	// reload while the file still exists and forget nothing.
	if err := mgr.reloadAfterRemove(ctx); err != nil {
		return fmt.Errorf("reload service manager after removing %s: %w", mgr.label(), err)
	}
	resolvedRoot, err := supervisor.ResolveRoot(*root)
	if err != nil {
		return err
	}
	if err := removeDaemonEnvToken(resolvedRoot); err != nil {
		return err
	}

	_, _ = fmt.Fprintf(stdout, "Uninstalled gofer daemon service %q.\n", mgr.label())
	return nil
}

// runDaemonStatus implements `gofer daemon status`: it reports whether the
// unit file is present and whether the service is currently running.
func runDaemonStatus(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := newDaemonServiceFlagSet("daemon status", stderr)
	if help, err := parseFlags(fs, args); err != nil {
		return err
	} else if help {
		return nil
	}

	mgr := newServiceManager()
	path, err := mgr.unitPath()
	if err != nil {
		return err
	}
	installed, err := mgr.isInstalled()
	if err != nil {
		return err
	}
	running, err := mgr.running(ctx)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(stdout, "service:   %s\n", mgr.label())
	_, _ = fmt.Fprintf(stdout, "installed: %s (%s)\n", yesNo(installed), path)
	_, _ = fmt.Fprintf(stdout, "running:   %s\n", yesNo(running))
	return nil
}

// newDaemonServiceFlagSet builds the shared flag set for the install/uninstall/
// status verbs (ContinueOnError + stderr output), matching runDaemon's setup.
func newDaemonServiceFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// buildServiceConfig assembles the token-free serviceConfig for an install:
// the symlink-resolved self path, the manager's label, and the daemon argv
// tail that pins --listen/--root explicitly so the service-managed daemon and
// its clients agree on one address and store.
func buildServiceConfig(mgr serviceManager, listen, root string) (serviceConfig, error) {
	exe, err := resolveSelfExec()
	if err != nil {
		return serviceConfig{}, err
	}
	resolvedRoot, err := supervisor.ResolveRoot(root)
	if err != nil {
		return serviceConfig{}, err
	}
	return serviceConfig{
		Label:      mgr.label(),
		ExecPath:   exe,
		ListenAddr: listen,
		Root:       resolvedRoot,
		Args:       []string{"daemon", "--listen", listen, "--root", resolvedRoot},
	}, nil
}

// resolveSelfExec returns the absolute, symlink-resolved path to the running
// gofer binary, the stable ExecStart/ProgramArguments[0] a unit file points at.
func resolveSelfExec() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve gofer executable path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		// A missing symlink target is unexpected but the raw path is still a
		// usable fallback rather than a hard install failure.
		return exe, nil
	}
	return resolved, nil
}

// writeDaemonEnvToken writes GOFER_TOKEN=<token> to <root>/daemon.env
// atomically at mode 0600, mirroring daemon.WriteEndpoint's temp-file+rename
// approach. This is the ONLY place a service-managed daemon's token is
// persisted; it is never templated into the unit file or passed on argv, and
// this function never logs the token.
func writeDaemonEnvToken(root, token string) error {
	resolvedRoot, err := supervisor.ResolveRoot(root)
	if err != nil {
		return err
	}
	path := filepath.Join(resolvedRoot, daemonEnvFileName)
	return atomicWriteFile(path, []byte("GOFER_TOKEN="+token+"\n"), 0o600)
}

// readDaemonEnvToken reads GOFER_TOKEN from <root>/daemon.env if it exists —
// the final token fallback runDaemon consults so a service-managed daemon picks
// up its token uniformly on both platforms without it ever appearing on argv.
// A missing file returns ("", nil). It never logs the file contents.
func readDaemonEnvToken(root string) (string, error) {
	resolvedRoot, err := supervisor.ResolveRoot(root)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(resolvedRoot, daemonEnvFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("read daemon env file: %w", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "GOFER_TOKEN="); ok {
			return v, nil
		}
	}
	return "", nil
}

// removeDaemonEnvToken removes <root>/daemon.env if present; an absent file is
// not an error.
func removeDaemonEnvToken(root string) error {
	path := filepath.Join(root, daemonEnvFileName)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove daemon env file %s: %w", path, err)
	}
	return nil
}

// atomicWriteFile writes data to path via a sibling temp file renamed into
// place, so a crash mid-write never leaves a truncated file. The final file
// carries the requested mode (0600 for the token env file, 0644 for the unit).
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".gofer-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp file over %s: %w", path, err)
	}
	return nil
}

// yesNo renders a bool as yes/no for the status report.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// renderLaunchdPlist renders cfg as a launchd user-agent plist. Deterministic
// and token-free: ProgramArguments is [ExecPath] + cfg.Args (which never
// contains a token), RunAtLoad + KeepAlive keep the daemon up, and stdout/
// stderr are captured under <Root>/logs/. Lives here (un-tagged) so its golden
// test compiles and runs on any OS.
func renderLaunchdPlist(cfg serviceConfig) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	b.WriteString("<dict>\n")
	b.WriteString("\t<key>Label</key>\n")
	b.WriteString("\t<string>" + xmlEscape(cfg.Label) + "</string>\n")
	b.WriteString("\t<key>ProgramArguments</key>\n")
	b.WriteString("\t<array>\n")
	b.WriteString("\t\t<string>" + xmlEscape(cfg.ExecPath) + "</string>\n")
	for _, a := range cfg.Args {
		b.WriteString("\t\t<string>" + xmlEscape(a) + "</string>\n")
	}
	b.WriteString("\t</array>\n")
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	b.WriteString("\t<key>KeepAlive</key>\n\t<true/>\n")
	b.WriteString("\t<key>StandardOutPath</key>\n")
	b.WriteString("\t<string>" + xmlEscape(filepath.Join(cfg.Root, "logs", "daemon.out.log")) + "</string>\n")
	b.WriteString("\t<key>StandardErrorPath</key>\n")
	b.WriteString("\t<string>" + xmlEscape(filepath.Join(cfg.Root, "logs", "daemon.err.log")) + "</string>\n")
	b.WriteString("</dict>\n")
	b.WriteString("</plist>\n")
	return []byte(b.String())
}

// renderSystemdUnit renders cfg as a systemd user service unit. Deterministic
// and token-free: ExecStart is ExecPath + cfg.Args joined (no token flag),
// Restart=on-failure keeps it up, WantedBy=default.target enables it for the
// user session. Lives here (un-tagged) so its golden test runs on any OS.
func renderSystemdUnit(cfg serviceConfig) []byte {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=gofer agent supervisor daemon\n")
	b.WriteString("After=network.target\n")
	b.WriteString("\n")
	b.WriteString("[Service]\n")
	b.WriteString("ExecStart=" + cfg.ExecPath + " " + strings.Join(cfg.Args, " ") + "\n")
	b.WriteString("Restart=on-failure\n")
	b.WriteString("\n")
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return []byte(b.String())
}

// xmlEscape escapes the five characters that would break a plist string value.
func xmlEscape(s string) string {
	return xmlEscaper.Replace(s)
}

var xmlEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&apos;",
)
