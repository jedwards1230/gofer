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

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/usercmd"
)

// userCommandsMsg carries a completed markdown-command load
// ([App.loadUserCommandsCmd]) back to [App.Update]. It has no error field for
// the same reason [modelsLoadedMsg] doesn't: every per-file failure is already
// a [usercmd.Warning] that degrades to "that one file is skipped", and a load
// that finds nothing is indistinguishable from a user with no command files —
// the normal case.
type userCommandsMsg struct {
	cmds  []usercmd.Command
	warns []usercmd.Warning
}

// loadUserCommandsCmd reads the markdown command layer OFF the Update loop —
// [usercmd.UserDir] under the resolved store root and [usercmd.ProjectDir]
// under this client's cwd, both threaded in from [CommandEnv] rather than
// recomputed, so `--root` moves the user-scope directory with it.
//
// It runs off the loop because [usercmd.Load] stats and walks two directory
// trees and reads every `.md` file in them, and none of that is bounded in
// time: a network-mounted cwd, an autofs mount waking up, or simply a large
// `.gofer/commands` tree turns the `/` keypress that triggers the reload into
// a visible freeze. Same reasoning (and same shape) as
// [App.discoverModelsCmd]: the registry keeps serving its last-known command
// set — which is the complete, correct one in every case except a file
// created seconds ago in this same session — and the fresh set replaces it in
// place when it lands.
//
// The one synchronous load is in [NewApp], before `tea.NewProgram` exists:
// there is no event loop to block and no frame to drop there, and doing it
// eagerly means a command typed in the first keystrokes resolves rather than
// racing the load.
func (a App) loadUserCommandsCmd() tea.Cmd {
	root, cwd, opts := a.commandEnv.Root, a.commandEnv.Cwd, a.userCommandOptions()
	return func() tea.Msg {
		cmds, warns := usercmd.Load(root, cwd, opts)
		return userCommandsMsg{cmds: cmds, warns: warns}
	}
}

// applyUserCommands installs a completed load's commands as the registry's
// markdown layer and reports anything that was skipped.
//
// Skipped files ([usercmd.Warning]) become one transient status note rather
// than being swallowed: a command that silently never appears is the single
// most confusing failure this feature can have, and the two rules that reject
// a file outright — an untypeable name, and a project file trying to replace a
// builtin — are exactly the ones a user needs told about.
func (a App) applyUserCommands(msg userCommandsMsg) App {
	a.registry.setLayer(sourceMarkdown, userCommands(msg.cmds))
	if n := len(msg.warns); n > 0 {
		note := "skipped " + strconv.Itoa(n) + " command file"
		if n > 1 {
			note += "s"
		}
		a.setStatus(sevWarn, note+": "+msg.warns[0].Error())
	}
	return a
}

// userCommandOptions builds the loader's [usercmd.Options] for this App: the
// builtin names a project file may not claim, and the per-file size cap.
//
// The cap is read off a.commandEnv.Config() on every call rather than cached —
// the same "always current, never a stale snapshot" contract
// [App.pasteLimitBytes] follows. A nil Config closure or a read error both
// fall through to the default.
func (a App) userCommandOptions() usercmd.Options {
	limit := config.DefaultMaxCommandFileBytes
	if a.commandEnv.Config != nil {
		if cfg, err := a.commandEnv.Config(); err == nil {
			limit = cfg.TUI.CommandFileLimitBytes()
		}
	}
	// Snapshot the reserved set rather than closing over the registry: the
	// predicate runs on another goroutine (loadUserCommandsCmd), and the
	// registry is a field of a value type this App copy owns.
	reserved := a.registry.builtinNames()
	return usercmd.Options{
		ReservedForProject: func(name string) bool { return reserved[name] },
		MaxFileBytes:       limit,
	}
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
// a command with no other feedback channel must never fail silently. They are
// checked in this order on purpose — the more actionable message wins when
// both apply:
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
		if a.sessID == "" {
			a.setStatus(sevDanger, "/"+uc.Name+" sends a prompt — attach a session first (→ on the roster)")
			return a, nil
		}
		prompt := strings.TrimSpace(usercmd.Expand(uc.Body, args))
		if prompt == "" {
			a.setStatus(sevWarn, "/"+uc.Name+" expanded to an empty prompt — nothing sent")
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
