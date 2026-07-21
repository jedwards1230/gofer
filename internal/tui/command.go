package tui

// command.go is the slash-command dispatcher: the registry every future
// /command in docs/TUI.md's dispatcher table (P0/P1/P2) plugs into, plus the
// submit-time parse intercept both text-entry surfaces (the overview
// dispatch bar, the attach input) route through. M4 step 1 registers only
// the three commands that open the command panel (see panel.go) — each
// Run for now just opens a placeholder tab; the real /status, /config, and
// /model behavior lands in follow-up PRs without changing this seam.

import (
	"sort"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/provider"
)

// Command is one entry in the slash-command [Registry]: a name plus the
// action Run performs when the dispatcher resolves a submitted buffer to it.
// The struct already carries what a future palette/autocomplete needs
// (Summary, ArgHint, Hidden) even though M4 step 1 only exercises
// Name/Aliases/Run.
type Command struct {
	Name    string   // "status" (no leading slash)
	Aliases []string // e.g. "cfg" for /config
	Summary string   // one line, for /help and a future palette
	ArgHint string   // "" for the M4 trio; "[id]" once /model takes one
	Hidden  bool     // skip autocomplete/palette listing (a future /debug)

	// Run applies the command to a, returning the updated App and an
	// optional follow-on tea.Cmd — the same (App, tea.Cmd) shape every other
	// App mutator in app.go returns.
	Run func(App, []string) (App, tea.Cmd)
}

// Registry maps a command's name and aliases to its [Command]. The zero
// value is usable empty; [newBuiltinRegistry] builds the M4 command set.
type Registry struct {
	cmds map[string]Command
}

// register adds cmd under its Name and every Alias. Collision order for
// future extension/markdown-template commands (docs/TUI.md's "extension >
// markdown > builtin") is a registration-time concern this registry doesn't
// yet need to enforce — M4 only ever registers builtins.
func (r *Registry) register(cmd Command) {
	if r.cmds == nil {
		r.cmds = map[string]Command{}
	}
	r.cmds[cmd.Name] = cmd
	for _, alias := range cmd.Aliases {
		r.cmds[alias] = cmd
	}
}

// Lookup resolves token — a command name or alias, without its leading
// slash — to the [Command] that handles it.
func (r Registry) Lookup(token string) (Command, bool) {
	cmd, ok := r.cmds[token]
	return cmd, ok
}

// List returns every non-Hidden registered command, deduplicated (a command
// registered under several aliases is stored once per alias in r.cmds but
// appears once here, keyed by Name) and sorted by Name — the source list the
// command-autocomplete popup filters from (command_menu.go) and any future
// palette.
func (r Registry) List() []Command {
	seen := make(map[string]bool, len(r.cmds))
	out := make([]Command, 0, len(r.cmds))
	for _, cmd := range r.cmds {
		if cmd.Hidden || seen[cmd.Name] {
			continue
		}
		seen[cmd.Name] = true
		out = append(out, cmd)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// matching returns [Registry.List] filtered to commands whose Name or any
// Alias has partial as a case-insensitive prefix. An empty partial (a bare
// "/") matches every command — the popup's initial, unfiltered state.
func (r Registry) matching(partial string) []Command {
	q := strings.ToLower(partial)
	all := r.List()
	out := make([]Command, 0, len(all))
	for _, cmd := range all {
		if strings.HasPrefix(strings.ToLower(cmd.Name), q) {
			out = append(out, cmd)
			continue
		}
		for _, alias := range cmd.Aliases {
			if strings.HasPrefix(strings.ToLower(alias), q) {
				out = append(out, cmd)
				break
			}
		}
	}
	return out
}

// newBuiltinRegistry returns the M4 registry: the three commands that open
// the command panel on their respective tab. `/` is command-only; `@`
// (file mention) and `!` (shell escape) are separate future prefixes
// (docs/TUI.md) — out of scope here, but [dispatchSlash]'s caller switches
// on the buffer's first rune so they slot in beside `/` later.
func newBuiltinRegistry() Registry {
	var r Registry
	r.register(Command{
		Name:    "status",
		Summary: "Show session, cwd, and provider status",
		Run:     openPanel(panelStatus),
	})
	r.register(Command{
		Name:    "usage",
		Summary: "Show this session's token and cost consumption",
		Run:     openPanel(panelUsage),
	})
	r.register(Command{
		Name:    "stats",
		Summary: "Show session lifecycle and roster-wide totals",
		Run:     openPanel(panelStats),
	})
	r.register(Command{
		Name:    "config",
		Aliases: []string{"cfg"},
		Summary: "View and edit settings",
		Run:     openPanel(panelConfig),
	})
	r.register(Command{
		Name:    "model",
		ArgHint: "[id]",
		Summary: "Pick the active/default model",
		Run:     runModel,
	})
	r.register(Command{
		Name:    "thinking",
		Aliases: []string{"effort"},
		ArgHint: "[low|medium|high|off]",
		Summary: "Set the reasoning effort for this session",
		Run:     runThinking,
	})
	return r
}

// openPanel returns a [Command.Run] that opens the command panel on tab,
// capturing a's commandEnv, current session snapshot, and resolved default
// model at open time — see [App.currentSessionInfo] and
// [Overview.DefaultModel].
func openPanel(tab commandPanelTab) func(App, []string) (App, tea.Cmd) {
	return func(a App, _ []string) (App, tea.Cmd) {
		p := newCommandPanel(a.theme, tab, a.commandEnv, a.currentSessionInfo(), a.over.DefaultModel(), a.over.Now(), a.over.Roster())
		a.panel = &p
		if tab == panelModel {
			// Only /model pays for a listing. Opening /status or /config must
			// not issue a vendor request the user never asked for — tabbing
			// across to Model later fetches then (see App.handlePanelKey).
			return a, a.discoverModelsCmd()
		}
		return a, nil
	}
}

// runModel is /model's [Command.Run]: bare `/model` opens the picker exactly
// as before, while `/model <id>` applies that id directly and never opens the
// panel. It deliberately does NOT use [openPanel] — openPanel discards its
// args, which is correct for the genuinely argument-less /status and /config
// but is the whole of issue #165 for a command that declares an ArgHint. The
// declared-hint-vs-discarded-args defect is guarded generally by
// TestArgHintCommandsConsumeArgs (command_args_test.go), not by this
// function's shape.
//
// The direct path routes into [App.applyModelSelection] — the same commit
// path Enter in the picker takes — so a string id and a picked row produce
// identical config writes, header refreshes, daemon probes, and status notes.
//
// Admission is [provider.Resolve] alone, matching the picker's typed-entry
// rule ([modelPickerView.selectedModel]): an id Resolve can route but the
// compiled-in catalog doesn't list still applies. The catalog is a vendor
// listing that goes stale, comes back empty, or is unreachable offline, and
// gating on it would break the string override in exactly the situations it
// exists for. What Resolve REJECTS is reported as a danger note here rather
// than silently opening the picker — the picker's own quiet no-op on an
// unroutable typed entry is a different surface (the reason is already on
// screen there) and stays as it is.
func runModel(a App, args []string) (App, tea.Cmd) {
	if len(args) == 0 {
		return openPanel(panelModel)(a, args)
	}
	// parseSlash splits on whitespace and no model id contains a space, so
	// more than one argument is always a mistake. Reject it by name instead
	// of joining or silently taking args[0]: "/model takes one id, got 2" is
	// actionable, whereas applying the first token would quietly set a model
	// the user did not ask for.
	if len(args) > 1 {
		a.setStatus(sevDanger, "/model takes a single model id — got "+strconv.Itoa(len(args))+" arguments")
		return a, nil
	}
	id := args[0]
	if _, err := provider.Resolve(id); err != nil {
		a.setStatus(sevDanger, "can't use model "+strconv.Quote(id)+": "+err.Error())
		return a, nil
	}
	return a.applyModelSelection(id, a.currentSessionInfo())
}

// runThinking is /thinking's [Command.Run], shaped exactly like [runModel]:
// bare `/thinking` opens the picker on the Thinking tab, while
// `/thinking <level>` applies that level directly and never opens the panel.
// Both forms land on [App.applyEffortSelection], the single commit path a
// picked row also takes, so a typed level and a selected row produce identical
// config writes, daemon calls, and status notes.
//
// Admission is [parseEffortArg] — the SDK's closed four-value vocabulary plus
// the "off"/"none"/"default" spellings of the empty (clear-the-level) value.
// That closedness is why this differs from /model's rule: a model id is an
// open-ended vendor namespace this binary's catalog can only lag behind, so
// /model deliberately admits ids it has never heard of, whereas an effort
// level outside the four is not a newer level — it is a typo, and reporting it
// by name beats forwarding it to a daemon that will reject it anyway.
func runThinking(a App, args []string) (App, tea.Cmd) {
	if len(args) == 0 {
		return openPanel(panelEffort)(a, args)
	}
	// parseSlash splits on whitespace and no level contains a space, so more
	// than one argument is always a mistake — rejected by name rather than
	// silently applying args[0], for the same reason /model does.
	if len(args) > 1 {
		a.setStatus(sevDanger, "/thinking takes a single level — got "+strconv.Itoa(len(args))+" arguments")
		return a, nil
	}
	effort, ok := parseEffortArg(args[0])
	if !ok {
		a.setStatus(sevDanger, "can't use reasoning effort "+strconv.Quote(args[0])+": want low, medium, high, or off")
		return a, nil
	}
	return a.applyEffortSelection(effort, a.currentSessionInfo())
}

// parseSlash splits a submitted "/name arg…" buffer into its command token
// and the remaining whitespace-separated arguments. buf is expected to
// already start with "/" — the caller checks that before dispatching; the
// leading slash is stripped before splitting.
func parseSlash(buf string) (name string, args []string) {
	fields := strings.Fields(strings.TrimPrefix(buf, "/"))
	if len(fields) == 0 {
		return "", nil
	}
	return fields[0], fields[1:]
}

// dispatchSlash parses and runs a submitted slash-command buffer (the
// caller has already confirmed it starts with "/"). Both submit paths — the
// overview dispatch bar and the attach input — route through this so every
// slash command behaves identically wherever it's typed. An unknown command
// sets the transient status line instead of running anything.
func (a App) dispatchSlash(buf string) (App, tea.Cmd) {
	name, args := parseSlash(buf)
	cmd, ok := a.registry.Lookup(name)
	if !ok {
		a.setStatus(sevDanger, "unknown command: /"+name)
		return a, nil
	}
	return cmd.Run(a, args)
}
