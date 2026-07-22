package tui

// command.go is the slash-command dispatcher: the registry every future
// /command in docs/TUI.md's dispatcher table (P0/P1/P2) plugs into, plus the
// submit-time parse intercept both text-entry surfaces (the overview
// dispatch bar, the attach input) route through. M4 step 1 registers only
// the three commands that open the command panel (see panel.go) — each
// Run for now just opens a placeholder tab; the real /status, /config, and
// /model behavior lands in follow-up PRs without changing this seam.

import (
	"os"
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

	// Source is which layer contributed this command, and therefore who wins
	// a name collision. The zero value is [sourceBuiltin], so a plain
	// Command literal registers as a builtin exactly as before this field
	// existed.
	Source CommandSource

	// Run applies the command to a, returning the updated App and an
	// optional follow-on tea.Cmd — the same (App, tea.Cmd) shape every other
	// App mutator in app.go returns.
	Run func(App, []string) (App, tea.Cmd)
}

// CommandSource is a registry layer. Its ORDER is the contract — docs/TUI.md's
// "extension > markdown > builtin" precedence — so the constants below are
// ranked lowest-wins-least to highest-wins-most and [Registry.rebuild] simply
// applies them in that order.
type CommandSource int

const (
	// sourceBuiltin is a command compiled into gofer ([newBuiltinRegistry]).
	// It is the zero value so an un-annotated Command literal is a builtin.
	sourceBuiltin CommandSource = iota
	// sourceMarkdown is a user-authored markdown file (internal/usercmd),
	// loaded per-app from the store root and the project directory. It
	// deliberately outranks a builtin: overriding /status with your own
	// prompt file is the point of the feature, not an accident to guard.
	sourceMarkdown
	// sourceExtension is a plugin's runtime registerCommand — P1, not built.
	// The constant exists so the tier it will occupy is already reserved at
	// the top of the order rather than being retrofitted later.
	sourceExtension
	// numCommandSources sizes [Registry.layers]; keep it last.
	numCommandSources
)

// Registry resolves a command name or alias to the [Command] that handles it,
// across the layered sources above. The zero value is usable empty;
// [newBuiltinRegistry] builds the M4/M5 builtin set.
//
// Commands are kept per-layer (layers) rather than only in the resolved index
// (cmds) for two reasons: precedence must be a property of WHERE a command
// came from, not of the order someone happened to call register; and a layer
// must be replaceable wholesale ([Registry.setLayer]) so a reload drops
// markdown commands whose files were deleted instead of accumulating them.
type Registry struct {
	layers [numCommandSources][]Command
	cmds   map[string]Command // resolved index, rebuilt from layers
}

// register adds cmd to its [Command.Source] layer, under its Name and every
// Alias.
func (r *Registry) register(cmd Command) {
	r.layers[cmd.Source] = append(r.layers[cmd.Source], cmd)
	r.rebuild()
}

// setLayer replaces every command from src with cmds (each stamped with src),
// then re-resolves. Passing nil clears the layer — which is how a markdown
// reload forgets a deleted file.
func (r *Registry) setLayer(src CommandSource, cmds []Command) {
	next := make([]Command, len(cmds))
	for i, c := range cmds {
		c.Source = src
		next[i] = c
	}
	r.layers[src] = next
	r.rebuild()
}

// builtinNames returns every name and alias the BUILTIN layer occupies, as a
// set. It backs [usercmd.Options.ReservedForProject]: a markdown file from
// `<cwd>/.gofer/commands` — which is whatever a cloned repository shipped, not
// something this user wrote — may not claim one of these, while a file from
// the user's own `<root>/commands` still may (see usercmds.go).
//
// Aliases are included: reserving /config but not /cfg would leave the exact
// hijack the reservation exists to prevent available under the other spelling.
func (r Registry) builtinNames() map[string]bool {
	out := make(map[string]bool, len(r.layers[sourceBuiltin])*2)
	for _, cmd := range r.layers[sourceBuiltin] {
		out[cmd.Name] = true
		for _, alias := range cmd.Aliases {
			out[alias] = true
		}
	}
	return out
}

// rebuild re-resolves the name/alias index from the layers, lowest source
// rank first so a higher one overwrites it. Three rules beyond that:
//
//   - Within a layer, a later registration wins (unchanged from M4).
//
//   - **A name is resolved first, then its aliases follow it.** An alias is a
//     second spelling of a name, so whatever wins the name owns the spelling:
//     a markdown `config.md` shadowing builtin /config also answers /cfg.
//     Leaving /cfg on the shadowed builtin would silently keep the overridden
//     behavior alive under a name the user believed they had replaced.
//
//   - **A NAME always beats an ALIAS**, whatever the layer — aliases are
//     applied in a first pass and names in a second — so a command called
//     "cfg" is reachable as /cfg even while /config carries that alias.
//
// Together these make [Registry.List] deterministic: exactly one entry per
// Name, chosen by source rank, never by map iteration order.
func (r *Registry) rebuild() {
	type aliasRef struct{ alias, owner string }

	byName := map[string]Command{}
	order := make([]string, 0, len(r.cmds))
	var aliases []aliasRef
	for _, layer := range r.layers {
		for _, cmd := range layer {
			if _, seen := byName[cmd.Name]; !seen {
				order = append(order, cmd.Name)
			}
			byName[cmd.Name] = cmd
			for _, alias := range cmd.Aliases {
				aliases = append(aliases, aliasRef{alias: alias, owner: cmd.Name})
			}
		}
	}
	idx := make(map[string]Command, len(byName)+len(aliases))
	for _, ref := range aliases {
		idx[ref.alias] = byName[ref.owner] // the winner of the name, not the declarer
	}
	for _, name := range order {
		idx[name] = byName[name]
	}
	r.cmds = idx
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
// the command panel on their respective tab. `/` is command-only — the other
// two input prefixes live outside this registry: `!` / `!!` (shell escape)
// dispatches beside it through the same first-rune switch
// ([App.dispatchInput], shell.go), and `@` (file mention) is a completion
// affordance inside a prompt rather than a dispatched command
// (filemention.go).
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
		Name:    "new",
		Summary: "Start a new session and attach to it",
		Run:     runNew,
	})
	r.register(Command{
		Name:    "resume",
		ArgHint: "[session-id]",
		Summary: "Reopen a session from disk",
		Run:     runResume,
	})
	r.register(Command{
		Name:    "quit",
		Aliases: []string{"exit"},
		Summary: "Quit gofer",
		Run:     runQuit,
	})
	r.register(Command{
		Name:    "thinking",
		Aliases: []string{"effort"},
		ArgHint: "[low|medium|high|off]",
		Summary: "Set the reasoning effort for this session",
		Run:     runThinking,
	})
	r.register(Command{
		Name:    "yolo",
		ArgHint: "[on|off]",
		Summary: "Toggle guardrails for new sessions",
		Run:     runYolo,
	})
	r.register(Command{
		Name:    "help",
		Summary: "List commands and keys",
		Run:     openPanel(panelHelp),
	})
	return r
}

// runQuit is /quit's [Command.Run]. Quitting the TUI is exactly tea.Quit
// everywhere else it is bound (ctrl-c, on every screen and over the panel — see
// app.go/panel.go/dialog.go), with no teardown of its own: the daemon
// connection, the event subscription, and the reconstruction core are all owned
// and closed by cmd/gofer once the program returns, not by the model. So this
// command is that same one line, and adding a confirmation here would make the
// command MORE ceremonious than the key it duplicates.
func runQuit(a App, _ []string) (App, tea.Cmd) {
	return a, tea.Quit
}

// runNew is /new's [Command.Run]: it starts a fresh session — new id, new
// journal — through [Supervisor.Create] and attaches into it, which is the same
// seam (and the same createdMsg landing) a prompt typed into the overview
// dispatch bar already takes. The previous session is left entirely alone: it
// keeps running, keeps its journal (repo invariant #4), and stays on the
// roster. /new is NOT /clear — resetting the transcript VIEW of the session you
// are in is a different command, and is not this one.
//
// It takes no arguments, and so declares no ArgHint. A seeded first prompt was
// considered and deliberately dropped: every string is a valid prompt, so a
// prompt argument can never be "unusable", and TestArgHintCommandsConsumeArgs
// (command_args_test.go) requires every ArgHint-declaring command to reject an
// unusable argument with a danger note naming it. Advertising an argument that
// cannot satisfy that guard would mean weakening the guard for every command.
// Typing the prompt into the fresh session's input — one keystroke sequence
// later, through the identical Create/Send path — costs nothing by comparison.
//
// Stray arguments are therefore REPORTED, never swallowed: silently discarding
// "/new fix the flaky test" would drop text the user clearly meant to send.
func runNew(a App, args []string) (App, tea.Cmd) {
	if len(args) > 0 {
		a.setStatus(sevDanger, "/new takes no arguments — it opens an empty session; type the prompt there")
		return a, nil
	}
	return a, a.doCreate("")
}

// runResume is /resume's [Command.Run], with the same bare-opens-the-picker /
// argument-applies-directly shape [runModel] has: bare `/resume` opens the
// command panel on the Resume tab (resumepicker.go) and lists what is on disk,
// while `/resume <id>` reopens that id immediately and never opens the panel.
//
// Both paths land in [App.resumeSession], so a typed id and a picked row
// produce the same op, the same attach, and the same error reporting.
//
// The typed id is admitted on SHAPE alone — non-empty, and usable as the single
// path component that names the journal file. Whether the session actually
// exists is a question only the backend can answer, and it does: an unknown id
// comes back as an error on [resumedMsg] and lands on the same sevDanger status
// line every other failed op does. Guessing here — matching against the roster,
// say — would refuse ids that are perfectly resumable, since the whole point of
// the command is sessions the roster does NOT hold.
func runResume(a App, args []string) (App, tea.Cmd) {
	if len(args) == 0 {
		return openPanel(panelResume)(a, args)
	}
	// parseSlash splits on whitespace and a session id can contain none, so more
	// than one argument is always a mistake — reported by name rather than
	// silently taking args[0], the same rule runModel applies.
	if len(args) > 1 {
		a.setStatus(sevDanger, "/resume takes a single session id — got "+strconv.Itoa(len(args))+" arguments")
		return a, nil
	}
	id := args[0]
	if !validSessionID(id) {
		a.setStatus(sevDanger, "can't resume "+strconv.Quote(id)+": not a valid session id")
		return a, nil
	}
	return a.resumeSession(id, a.cwd)
}

// validSessionID reports whether id can name a session at all. A session id is
// used verbatim as the single path component of its <id>.jsonl journal, so the
// store rejects "."/".."/anything containing a separator (session.ErrInvalidID);
// this is the client-side mirror of that rule, so an id that could never address
// a session is refused here with a message rather than sent to the daemon to be
// refused there.
func validSessionID(id string) bool {
	return id != "" && id != "." && id != ".." &&
		!strings.ContainsRune(id, '/') && !strings.ContainsRune(id, os.PathSeparator)
}

// openPanel returns a [Command.Run] that opens the command panel on tab,
// capturing a's commandEnv, current session snapshot, and resolved default
// model at open time — see [App.currentSessionInfo] and
// [Overview.DefaultModel].
func openPanel(tab commandPanelTab) func(App, []string) (App, tea.Cmd) {
	return func(a App, _ []string) (App, tea.Cmd) {
		p := newCommandPanel(a.theme, tab, a.commandEnv, a.currentSessionInfo(), a.over.DefaultModel(), a.over.Now(), a.over.Roster(), a.registry)
		a.panel = &p
		switch tab {
		case panelModel:
			// Only /model pays for a listing. Opening /status or /config must
			// not issue a vendor request the user never asked for — tabbing
			// across to Model later fetches then (see App.handlePanelKey).
			return a, a.discoverModelsCmd()
		case panelResume:
			// Same rule, same reason: only /resume pays for the store walk.
			return a, a.listSessionsCmd()
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
