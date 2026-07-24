package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/mod/semver"

	"github.com/jedwards1230/gofer/internal/daemon"
)

// daemonDialTimeout bounds how long a command waits to find out whether a
// daemon is listening before falling back to (or failing without) an
// in-process path. A closed port refuses a TCP dial near-instantly, so this
// only matters for a firewalled/filtered address that would otherwise hang;
// it is generous enough for a real daemon's handshake, short enough that a
// dead address never makes a CLI invocation feel stuck.
const daemonDialTimeout = 2 * time.Second

// daemonFlags are the --daemon/--token flags every daemon-aware command
// (ps, kill, archive, attach, agents, and the daemon leg of run/resume)
// shares.
type daemonFlags struct {
	addr  string
	token string
	// epVersion is the build version the discovered endpoint file advertised
	// (daemon.Endpoint.Version), cached by [daemonFlags.resolve] ONLY when addr
	// was settled from that same file — the same trust guard resolve uses for
	// the file's token. It stays "" when addr came from a flag/env/default (a
	// remote daemon's version isn't locally knowable without a round-trip) or
	// when an older daemon never wrote one; [dialDaemon] then skips the skew
	// warning rather than warning against an unknown version.
	epVersion string
}

// addDaemonFlags registers --daemon and --token on fs and returns the flags
// struct [daemonFlags.resolve] (dialDaemon's only caller) reads back after
// Parse. Both flags default to "" — an unset sentinel distinguishing "the
// operator explicitly named an address/token" from "let resolve fall through
// to $GOFER_DAEMON/$GOFER_TOKEN, the endpoint file, or the loopback
// default" (see resolve's doc for the full precedence). The token flag's
// empty default is additionally deliberate for the same reason
// `gofer daemon`'s is (see cmd/gofer/daemon.go): flag.PrintDefaults would
// otherwise leak a token set in the environment into --help/usage output.
func addDaemonFlags(fs *flag.FlagSet) *daemonFlags {
	f := &daemonFlags{}
	fs.StringVar(&f.addr, "daemon", "", "daemon address to connect to (default: $GOFER_DAEMON, the endpoint gofer daemon advertised at <root>/daemon.json, or 127.0.0.1:7333)")
	fs.StringVar(&f.token, "token", "", "bearer token for the daemon (default: $GOFER_TOKEN, or the token from the endpoint file when its address was used)")
	return f
}

// resolve resolves the effective daemon address and bearer token dialDaemon
// uses, in precedence order:
//
//  1. An explicit --daemon/--token flag.
//  2. $GOFER_DAEMON / $GOFER_TOKEN.
//  3. The endpoint file a running `gofer daemon` advertised at
//     <root>/daemon.json (see internal/daemon.WriteEndpoint), read via
//     [daemon.ReadEndpoint] with the given root — the command's own --root
//     when it has one, so a daemon and a client given the SAME --root
//     discover each other automatically. Commands with no --root of their
//     own (ps/kill/archive/attach/bare gofer) pass "", which resolves to
//     the default ~/.gofer; a daemon started with a --root other than the
//     one the client resolves against still needs an explicit --daemon (or
//     $GOFER_DAEMON) on its clients, since its endpoint file lives
//     somewhere resolve never looks.
//  4. [daemon.DefaultListenAddr] (the loopback default).
//
// The endpoint file's token is used ONLY when its address is what resolve
// actually settled on for addr (i.e. no flag/env override chose a
// different address) — otherwise a token minted for one daemon could leak
// into a connection aimed at another one entirely.
//
// A missing or corrupt endpoint file is silently skipped — discovery is a
// convenience, never a hard requirement, and resolve never logs the file's
// contents (path-only errors, if any, are simply treated as "no file").
// resolve caches its result onto f.addr/f.token so a caller that reads them
// again after calling it (e.g. daemonDialErr, or a stderr "connected to
// daemon at %s" notice) sees the resolved values, not the flags' original
// zero values.
func (f *daemonFlags) resolve(root string) (addr, token string) {
	addr = f.addr
	token = f.token
	if addr == "" {
		addr = os.Getenv("GOFER_DAEMON")
	}
	if token == "" {
		token = os.Getenv("GOFER_TOKEN")
	}

	if addr == "" {
		if ep, err := daemon.ReadEndpoint(root); err == nil && ep.Addr != "" {
			addr = ep.Addr
			if token == "" {
				token = ep.Token
			}
			// Trust the file's advertised version only when we actually settled
			// on its address (the same guard as the token above): we're truly
			// connecting to the daemon that advertised it, so its version is the
			// one to compare against ours. A flag/env override to a different
			// address leaves epVersion "" (unknown → no warning).
			f.epVersion = ep.Version
		}
	}

	if addr == "" {
		addr = daemon.DefaultListenAddr
	}

	f.addr, f.token = addr, token
	return addr, token
}

// addLocalFlag registers the --local / --no-daemon opt-out on fs (both names
// alias one bool). When set, run/resume skip the daemon probe entirely and
// always take the in-process path — the escape hatch for a user who has a
// daemon up but wants local behavior (the interactive TUI, -m/--root honored)
// for one invocation. It is registered only on run/resume, not the
// daemon-only ps/kill/archive commands, where opting out of the daemon makes
// no sense.
func addLocalFlag(fs *flag.FlagSet) *bool {
	local := new(bool)
	fs.BoolVar(local, "local", false, "skip the daemon and run in-process even if one is reachable (alias: --no-daemon)")
	fs.BoolVar(local, "no-daemon", false, "alias for --local")
	return local
}

// noteDaemonDeviations prints a stderr notice for each flag whose in-process
// meaning does not carry to the daemon path, so a user never has one silently
// ignored. cmd is the command label ("run"/"resume") for the message prefix.
// stderr is used deliberately: a --json notice there cannot corrupt the JSON
// stream on stdout. The daemon path swaps the interactive TUI for plain
// streaming too, but that is a rendering choice with no lost flag behind it,
// so it is documented (in run/resume's doc comments) rather than announced per
// invocation.
func noteDaemonDeviations(stderr io.Writer, cmd, model, root string, asJSON bool) {
	if model != "" {
		_, _ = fmt.Fprintf(stderr, "gofer %s: a daemon is running — model selection is the daemon's (set --model at `gofer daemon` startup); -m is ignored here\n", cmd)
	}
	if root != "" {
		_, _ = fmt.Fprintf(stderr, "gofer %s: a daemon is running — it uses its own session store; --root is ignored here (pass --local to run in-process against %s)\n", cmd, root)
	}
	if asJSON {
		_, _ = fmt.Fprintf(stderr, "gofer %s: driving via the daemon — --json emits ACP session/update JSON, not the in-process event.Event JSONL\n", cmd)
	}
}

// dialDaemon connects to the daemon at f's resolved address (see
// [daemonFlags.resolve] for the flag/env/endpoint-file/default precedence;
// root is the endpoint-file lookup root — the caller's --root when it has
// one, else ""), bounded by [daemonDialTimeout] so a dead/filtered address
// cannot hang the caller. It returns [daemon.Dial]'s error unwrapped (still
// satisfying errors.Is(err, daemon.ErrNoDaemon) / [daemon.ErrUnauthorized])
// so a caller can tell "nothing is listening — fall back" (run/resume) apart
// from "something is listening but rejected us — that's a real problem"
// (every daemon-aware command); see [daemonUnreachable] and [daemonDialErr].
func dialDaemon(ctx context.Context, f *daemonFlags, root string, stderr io.Writer) (*daemon.Client, error) {
	addr, token := f.resolve(root)
	dctx, cancel := context.WithTimeout(ctx, daemonDialTimeout)
	defer cancel()
	c, err := daemon.Dial(dctx, addr, token)
	if err != nil {
		// A failed dial means no daemon answered (or rejected us) — there's
		// nothing running to compare versions against, so don't warn.
		return nil, err
	}
	// Only after a successful dial: compare our build against the running
	// daemon's and warn — never refuse or restart — when the daemon is out of
	// date. This is warn-only: gofer never auto-restarts the daemon (that would
	// kill live sessions and can't touch a manually-run foreground daemon).
	//
	// The daemon's version comes AUTHORITATIVELY from the gofer/hello handshake
	// (the same source the router trusts — see internal/router/skew.go), which
	// works regardless of how addr was discovered. The endpoint-file hint
	// (f.epVersion, set by resolve ONLY when addr came from the file) is the
	// fallback for a daemon that predates gofer/hello or reports no version.
	// Compare against effectiveVersion(): a daemon stamps BOTH its endpoint file
	// and its gofer/hello with the SAME derived identifier (see runDaemon), so
	// comparing against the raw ldflags sentinel would report skew ("dev" vs
	// "dev-<sha>") on every local build.
	daemonVersion := f.epVersion
	if hello, herr := c.Hello(dctx); herr == nil && hello.BinaryVersion != "" {
		daemonVersion = hello.BinaryVersion
	}
	if cliVersion := effectiveVersion(); daemonVersion != "" {
		switch classifyClientDaemonSkew(cliVersion, daemonVersion) {
		case skewDaemonOlder:
			warnVersionSkew(stderr, cliVersion, daemonVersion, true)
		case skewDaemonDiffers:
			warnVersionSkew(stderr, cliVersion, daemonVersion, false)
		case skewNoWarn:
			// Equal, indeterminate, or the daemon is NEWER than this CLI — in the
			// last case restarting the daemon is the wrong advice (the CLI is the
			// stale side), so stay silent rather than nag with a misdirected fix.
		}
	}
	return c, nil
}

// clientDaemonSkew is how this CLI's build relates to the daemon it connected
// to, reduced to the cases dialDaemon warns (or stays silent) on. It is the
// client↔daemon analogue of internal/router's skewClass, minus the wire axis
// the router also tracks (the client speaks the daemon's wire or the dial would
// have failed).
type clientDaemonSkew int

const (
	// skewNoWarn: nothing actionable — the versions are equal, exactly one side
	// identified itself (unknown, never a false positive), or the daemon is
	// NEWER than this CLI (the CLI is the stale side, so "restart the daemon" is
	// the wrong fix).
	skewNoWarn clientDaemonSkew = iota
	// skewDaemonOlder: both versions are comparable semver and the daemon's is
	// strictly older — the stale-daemon case a restart fixes.
	skewDaemonOlder
	// skewDaemonDiffers: the versions differ but their order can't be
	// established (one or both are non-semver dev builds like "dev-<sha>"), so
	// the direction is unknown but the daemon is definitely a different build.
	skewDaemonDiffers
)

// classifyClientDaemonSkew decides whether — and how — to warn about the gap
// between this CLI's build (cliVersion) and the daemon's (daemonVersion). It is
// pure so the decision is table-tested without a daemon.
//
// It never false-positives on an unknown: an empty version on either side, or
// two equal versions, is [skewNoWarn]. When BOTH are valid semver it uses
// [semver.Compare] for an authoritative direction (release tags AND Go
// pseudo-versions like v0.3.1-0.YYYY…-<sha> are valid semver and order
// correctly); only a strictly-older daemon warns as [skewDaemonOlder], a newer
// or equal-precedence daemon is [skewNoWarn]. Versions that differ but aren't
// both semver (local "dev-<sha>" builds) are [skewDaemonDiffers] — a real but
// undirected skew.
func classifyClientDaemonSkew(cliVersion, daemonVersion string) clientDaemonSkew {
	if cliVersion == "" || daemonVersion == "" || cliVersion == daemonVersion {
		return skewNoWarn
	}
	if semver.IsValid(cliVersion) && semver.IsValid(daemonVersion) {
		if semver.Compare(daemonVersion, cliVersion) < 0 {
			return skewDaemonOlder
		}
		// Daemon newer than, or equal precedence to, the CLI: nothing a daemon
		// restart fixes.
		return skewNoWarn
	}
	return skewDaemonDiffers
}

// warnVersionSkew prints a loud, unmistakable stderr warning when this CLI's
// build (cliVersion) differs from the running daemon's (daemonVersion). When
// older is true the daemon is provably out of date (older semver); otherwise it
// is a different build of undetermined direction. Either way it names both
// versions and how to restart the daemon, then leaves the caller to proceed:
// gofer never auto-restarts the daemon (that would kill live sessions and can't
// touch a manually-run foreground daemon) — it warns and continues.
func warnVersionSkew(stderr io.Writer, cliVersion, daemonVersion string, older bool) {
	lead := fmt.Sprintf("the daemon is a different build (%s) than this CLI (%s)", daemonVersion, cliVersion)
	if older {
		lead = fmt.Sprintf("the daemon (%s) is older than this CLI (%s)", daemonVersion, cliVersion)
	}
	_, _ = fmt.Fprintf(stderr, `gofer: WARNING: version skew — %s.
The running daemon is out of date; restart it to pick up the new build:
  • foreground daemon: stop it (Ctrl-C) and re-run `+"`gofer daemon`"+`
  • service-managed:   `+"`gofer daemon uninstall && gofer daemon install`"+`
Continuing with the running daemon.
`, lead)
}

// daemonUnreachable reports whether err is [dialDaemon] reporting no daemon at
// all (as opposed to one that's running but rejected the connection) — the
// signal run/resume use to fall back to the in-process path.
func daemonUnreachable(err error) bool {
	return errors.Is(err, daemon.ErrNoDaemon)
}

// daemonDialErr turns a [dialDaemon] failure into the clear, addr-specific
// message every daemon-required command (ps, kill, archive, and run/resume
// once a daemon is known to be running) prints to stderr: an unreachable
// address gets a nudge to `gofer daemon`, while a reachable-but-unauthorized
// daemon reports the auth problem distinctly rather than looking identical to
// "nothing is listening."
func daemonDialErr(addr string, err error) error {
	if errors.Is(err, daemon.ErrUnauthorized) {
		return fmt.Errorf("daemon at %s rejected the connection: unauthorized (check --token or $GOFER_TOKEN)", addr)
	}
	return fmt.Errorf("no gofer daemon running at %s — start one with `gofer daemon`", addr)
}
