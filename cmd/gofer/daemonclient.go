package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

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
func dialDaemon(ctx context.Context, f *daemonFlags, root string) (*daemon.Client, error) {
	addr, token := f.resolve(root)
	dctx, cancel := context.WithTimeout(ctx, daemonDialTimeout)
	defer cancel()
	return daemon.Dial(dctx, addr, token)
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
