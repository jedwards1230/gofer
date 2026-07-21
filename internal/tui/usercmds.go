package tui

// usercmds.go adapts user-authored markdown commands (internal/usercmd) into
// dispatcher entries. Everything about PARSING a command file — discovery,
// frontmatter, `$1`/`$ARGUMENTS` substitution — lives in usercmd, which has no
// bubbletea dependency and is table-tested without a terminal; this file only
// wraps each loaded file in a [Command] and decides what running one DOES.
//
// A markdown command does exactly what typing its expanded body into the
// attach input would: it goes out through [App.doSend], the same
// Supervisor.Send seam a hand-typed prompt uses. There is no second send path,
// so a markdown command can never diverge from a typed one.

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/usercmd"
)

// reloadUserCommands rebuilds the registry's markdown layer from disk —
// [usercmd.UserDir] under the resolved store root and [usercmd.ProjectDir]
// under this client's cwd, both threaded in from [CommandEnv] rather than
// recomputed, so `--root` moves the user-scope directory with it.
//
// Called at [NewApp] and again on the closed→open transition of the
// command-autocomplete popup ([App.syncMenu]) — once per `/` typed, never per
// keystroke and never inside [Registry.matching]. That is the same spirit as
// CommandEnv's lazy reads (env.go): no cached-forever snapshot, but no
// filesystem walk on a hot path either. A command file written while the TUI
// runs is picked up the next time the popup opens.
//
// Skipped files ([usercmd.Warning]) become one transient status note rather
// than being swallowed: a command that silently never appears is the single
// most confusing failure this feature can have.
func (a App) reloadUserCommands() App {
	cmds, warns := usercmd.Load(a.commandEnv.Root, a.commandEnv.Cwd)
	a.registry.setLayer(sourceMarkdown, userCommands(cmds))
	if len(warns) > 0 {
		note := "skipped " + strconv.Itoa(len(warns)) + " command file"
		if len(warns) > 1 {
			note += "s"
		}
		a.setStatus(sevWarn, note+": "+warns[0].Error())
	}
	return a
}

// userCommands wraps each loaded file as a registry [Command]. Precedence is
// carried by the layer, not by this slice's order — see [Registry.setLayer].
func userCommands(loaded []usercmd.Command) []Command {
	out := make([]Command, 0, len(loaded))
	for _, uc := range loaded {
		out = append(out, Command{
			Name:    uc.Name,
			Summary: uc.Description,
			ArgHint: uc.ArgumentHint,
			Run:     runUserCommand(uc),
		})
	}
	return out
}

// runUserCommand returns the [Command.Run] for one markdown command:
// substitute the typed arguments into the body ([usercmd.Expand]) and submit
// the result as this session's next prompt.
//
// The two refusals both report rather than drop, matching /model's rule that
// a command with no other feedback channel must never fail silently:
//
//   - No attached session. The overview has nothing to send to (its own Enter
//     CREATES a session from the typed text, which is a different action than
//     "run this command against what I'm looking at" — quietly picking one
//     would surprise either way). A danger note says what to do instead.
//   - An empty expansion — an empty file, or a body that was nothing but
//     `$1` with no argument given. Sending an empty prompt would burn a turn
//     on nothing.
func runUserCommand(uc usercmd.Command) func(App, []string) (App, tea.Cmd) {
	return func(a App, args []string) (App, tea.Cmd) {
		prompt := strings.TrimSpace(usercmd.Expand(uc.Body, args))
		if prompt == "" {
			a.setStatus(sevWarn, "/"+uc.Name+" expanded to an empty prompt — nothing sent")
			return a, nil
		}
		if a.sessID == "" {
			a.setStatus(sevDanger, "/"+uc.Name+" sends a prompt — attach a session first (→ on the roster)")
			return a, nil
		}
		// Same tail-snap a hand-typed prompt does (handleAttachKey): the reply
		// is exactly what a scrolled-back reader wants to see next.
		a.scroll = 0
		// And the same prompt assembly, for the reason stated at the top of
		// this file: a markdown command must do exactly what typing its
		// expanded body would. A pending `!` shell run owes its output to the
		// next prompt whichever way that prompt is composed — skipping the
		// fold here would make /mycmd and the identical hand-typed text
		// produce different model input, which is precisely the divergence
		// "there is no second send path" exists to rule out. `!!` runs stay
		// excluded here for free: composePrompt is the one place that decides
		// (shell.go).
		prompt = a.composePrompt(prompt)
		return a, a.doSend(a.sessID, prompt)
	}
}
