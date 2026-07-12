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
// (ps, kill, archive, and the daemon leg of run/resume) shares.
type daemonFlags struct {
	addr  string
	token string
}

// addDaemonFlags registers --daemon and --token on fs and returns the flags
// struct dialDaemon reads back after Parse. The token flag's default is
// deliberately "" rather than os.Getenv("GOFER_TOKEN") for the same reason
// `gofer daemon`'s does (see cmd/gofer/daemon.go): flag.PrintDefaults would
// otherwise leak a token set in the environment into --help/usage output.
func addDaemonFlags(fs *flag.FlagSet) *daemonFlags {
	f := &daemonFlags{}
	fs.StringVar(&f.addr, "daemon", daemon.DefaultListenAddr, "daemon address to connect to")
	fs.StringVar(&f.token, "token", "", "bearer token for the daemon (default: $GOFER_TOKEN)")
	return f
}

// resolveToken resolves the effective bearer token: the flag if set, else
// $GOFER_TOKEN. Never logged — see [dialDaemon], its only caller.
func (f *daemonFlags) resolveToken() string {
	if f.token != "" {
		return f.token
	}
	return os.Getenv("GOFER_TOKEN")
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

// dialDaemon connects to the daemon at f's address, bounded by
// [daemonDialTimeout] so a dead/filtered address cannot hang the caller. It
// returns [daemon.Dial]'s error unwrapped (still satisfying
// errors.Is(err, daemon.ErrNoDaemon) / [daemon.ErrUnauthorized]) so a caller
// can tell "nothing is listening — fall back" (run/resume) apart from
// "something is listening but rejected us — that's a real problem" (every
// daemon-aware command); see [daemonUnreachable] and [daemonDialErr].
func dialDaemon(ctx context.Context, f *daemonFlags) (*daemon.Client, error) {
	dctx, cancel := context.WithTimeout(ctx, daemonDialTimeout)
	defer cancel()
	return daemon.Dial(dctx, f.addr, f.resolveToken())
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
