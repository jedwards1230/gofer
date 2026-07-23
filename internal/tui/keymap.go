package tui

// keymap.go is the TUI's single declarative key table — the thing /help
// (help.go) renders its Keys section from.
//
// HOW LIVE IS IT? Deliberately, and visibly, two-tier:
//
//   - [globalKeymap]'s rows are LIVE. Each carries a match predicate and the
//     action it runs, and [App.handleKey] dispatches through
//     [dispatchGlobalKey] before it reaches any per-screen handler. There is
//     exactly one definition of ctrl+c and ctrl+y, and /help reads it.
//   - [screenKeymap]'s rows are DESCRIPTIVE ONLY, and CAN DRIFT. Every screen
//     in this package still owns an inline `switch` on tea.KeyPressMsg
//     (app.go, panel.go, config_view.go, modelpicker.go, dialog.go,
//     command_menu.go, peek's branch of app.go); several of those bindings are
//     conditional on state this table has no way to express — a bare → on the
//     overview attaches the selected session only when the dispatch bar is
//     EMPTY, and edits the buffer otherwise. Routing them through a table would
//     mean rewriting every screen's key handling, which is a refactor of its
//     own and not this change. So they are declared here for /help and
//     asserted only by [TestScreenKeymapRowsAreDocumented]'s shape checks —
//     changing a screen's inline switch without updating its row here will not
//     fail a test. Move a row up to globalKeymap (match + run) the moment its
//     dispatch can live here.
//
// docs/TUI.md's "Input editing" table stays the prose reference for the
// readline keymap; the rows below name the same bindings so /help can show
// them without a second document.

import tea "charm.land/bubbletea/v2"

// keyScope is where a binding applies — the grouping /help renders under.
type keyScope int

const (
	// scopeGlobal is every screen (but not an overlay that has already claimed
	// the key — see [App.Update]'s panel > approval > menu > screen order).
	scopeGlobal keyScope = iota
	scopeOverview
	scopePeek
	scopeAttach
	scopeInput
	scopePrefix
	scopePanel
	scopeMenu
	scopeApproval
	scopeAmend
	scopeDecision
)

// label is the scope's /help section heading.
func (s keyScope) label() string {
	switch s {
	case scopeOverview:
		return "Roster"
	case scopePeek:
		return "Peek"
	case scopeAttach:
		return "Attach"
	case scopeInput:
		return "Text entry"
	case scopePrefix:
		return "Input prefixes"
	case scopePanel:
		return "Command panel"
	case scopeMenu:
		return "Autocomplete popup"
	case scopeApproval:
		return "Approval prompt"
	case scopeAmend:
		return "Amending a tool call"
	case scopeDecision:
		return "Agent question (ask_user)"
	default:
		return "Global"
	}
}

// keyScopeOrder is the order /help renders the scopes in: global first, then
// the screens by navigation depth, then the overlays.
var keyScopeOrder = []keyScope{
	scopeGlobal, scopeOverview, scopePeek, scopeAttach,
	scopeInput, scopePrefix, scopeMenu, scopePanel, scopeApproval, scopeAmend, scopeDecision,
}

// keyBinding is one row of the table: how it's written in /help, where it
// applies, what it does, and — for a global — how it's actually dispatched.
type keyBinding struct {
	Keys  string // display form, e.g. "ctrl+y"
	Scope keyScope
	Desc  string

	// match reports whether key is this binding. Non-nil only on a live
	// (global) row; see the file doc.
	match func(tea.Key) bool
	// run performs the binding's action. Non-nil only on a live row.
	run func(App) (tea.Model, tea.Cmd)
}

// live reports whether this row is dispatched through the table rather than by
// a screen's own inline switch.
func (b keyBinding) live() bool { return b.match != nil && b.run != nil }

// globalKeymap is the live half: bindings that apply on every screen and are
// dispatched from this table by [dispatchGlobalKey].
func globalKeymap() []keyBinding {
	return []keyBinding{
		{
			Keys:  "ctrl+c",
			Scope: scopeGlobal,
			Desc:  "Quit gofer",
			match: func(k tea.Key) bool { return k.Mod.Contains(tea.ModCtrl) && k.Code == 'c' },
			run:   func(a App) (tea.Model, tea.Cmd) { return a, tea.Quit },
		},
		{
			Keys:  "ctrl+y",
			Scope: scopeGlobal,
			Desc:  "Toggle guardrails for new sessions (same as /yolo)",
			match: func(k tea.Key) bool { return k.Mod.Contains(tea.ModCtrl) && k.Code == 'y' },
			run: func(a App) (tea.Model, tea.Cmd) {
				next, cmd := a.applyPermissionMode(yoloToggle)
				return next, cmd
			},
		},
		{
			Keys:  "ctrl+r",
			Scope: scopeGlobal,
			Desc:  "Toggle reply-on-run for shell commands (reply now / queue)",
			match: func(k tea.Key) bool { return k.Mod.Contains(tea.ModCtrl) && k.Code == 'r' },
			run: func(a App) (tea.Model, tea.Cmd) {
				a.shellQueue = !a.shellQueue
				if a.shellQueue {
					a.setStatus(sevOK, "shell: queue mode — `!` commands wait for your next message")
				} else {
					a.setStatus(sevOK, "shell: reply mode — a `!` command sends and gets a reply")
				}
				return a, nil
			},
		},
	}
}

// screenKeymap is the descriptive-only half — see the file doc for why these
// rows are not dispatched from here and what that costs.
func screenKeymap() []keyBinding {
	return []keyBinding{
		{Keys: "↑/↓", Scope: scopeOverview, Desc: "Move the roster selection"},
		{Keys: "enter", Scope: scopeOverview, Desc: "Open the selected session, or run what's typed"},
		{Keys: "space", Scope: scopeOverview, Desc: "Peek the selected session (empty dispatch bar)"},
		{Keys: "tab", Scope: scopeOverview, Desc: "Switch flat / grouped roster view"},
		{Keys: "esc", Scope: scopeOverview, Desc: "Clear the dispatch bar"},
		{Keys: "ctrl+x", Scope: scopeOverview, Desc: "Kill a running session, archive a finished one"},
		{Keys: "ctrl+t", Scope: scopeOverview, Desc: "Stop every subagent under the selected session"},
		{Keys: "pgup/pgdn", Scope: scopeOverview, Desc: "Scroll the roster"},
		{Keys: "?", Scope: scopeOverview, Desc: "Open this help (empty dispatch bar)"},

		{Keys: "enter", Scope: scopePeek, Desc: "Open the session, or send the typed reply"},
		{Keys: "space", Scope: scopePeek, Desc: "Close back to the roster (empty reply)"},
		{Keys: "esc", Scope: scopePeek, Desc: "Close back to the roster"},
		{Keys: "↑/↓", Scope: scopePeek, Desc: "Move the roster selection"},
		{Keys: "ctrl+x", Scope: scopePeek, Desc: "Kill a running session, archive a finished one"},

		{Keys: "←", Scope: scopeAttach, Desc: "Back out to the parent session, else the roster (empty input)"},
		{Keys: "↓", Scope: scopeAttach, Desc: "Drill into this session's subagents (empty input)"},
		{Keys: "enter", Scope: scopeAttach, Desc: "Send the prompt, or run a /command"},
		{Keys: "esc", Scope: scopeAttach, Desc: "Interrupt the running turn"},
		{Keys: "pgup/pgdn", Scope: scopeAttach, Desc: "Scroll the transcript"},

		{Keys: "←/→", Scope: scopeInput, Desc: "Move one character"},
		{Keys: "alt+←/→", Scope: scopeInput, Desc: "Move one word"},
		{Keys: "home/ctrl+a", Scope: scopeInput, Desc: "Move to line start"},
		{Keys: "end/ctrl+e", Scope: scopeInput, Desc: "Move to line end"},
		{Keys: "backspace", Scope: scopeInput, Desc: "Delete the character before the cursor"},
		{Keys: "delete/ctrl+d", Scope: scopeInput, Desc: "Delete the character at the cursor"},
		{Keys: "alt+backspace/ctrl+w", Scope: scopeInput, Desc: "Delete the word before the cursor"},
		{Keys: "ctrl+u", Scope: scopeInput, Desc: "Delete to line start"},
		{Keys: "ctrl+k", Scope: scopeInput, Desc: "Delete to line end"},

		// The input prefixes are a SUBMIT-TIME grammar rather than keys (see
		// App.dispatchInput, shell.go), but they are the part of the input
		// surface a user is least likely to discover unaided, so /help carries
		// them beside the bindings.
		{Keys: "/name", Scope: scopePrefix, Desc: "Run a slash command (see Commands above)"},
		{Keys: "!cmd", Scope: scopePrefix, Desc: "Run a shell command; its output joins the model's context"},
		{Keys: "!!cmd", Scope: scopePrefix, Desc: "Run a shell command, keeping its output OUT of context"},
		{Keys: "@path", Scope: scopePrefix, Desc: "Complete a file path into the prompt (the path, not its contents)"},

		{Keys: "↑/↓", Scope: scopeMenu, Desc: "Move the highlighted entry"},
		{Keys: "tab", Scope: scopeMenu, Desc: "Complete the highlighted entry"},
		{Keys: "enter", Scope: scopeMenu, Desc: "Run the highlighted command"},
		{Keys: "esc", Scope: scopeMenu, Desc: "Close the popup, keep the typed text"},

		{Keys: "←/→", Scope: scopePanel, Desc: "Switch tabs"},
		{Keys: "↑/↓", Scope: scopePanel, Desc: "Move within the active tab"},
		{Keys: "enter", Scope: scopePanel, Desc: "Commit the active tab's selection"},
		{Keys: "esc", Scope: scopePanel, Desc: "Clear the tab's state, then close the panel"},

		{Keys: "a/y/1", Scope: scopeApproval, Desc: "Allow the tool call"},
		{Keys: "d/n/2", Scope: scopeApproval, Desc: "Deny the tool call"},
		{Keys: "r", Scope: scopeApproval, Desc: "Toggle remember"},
		{Keys: "ctrl+e", Scope: scopeApproval, Desc: "Ask the agent why it wants this call"},
		{Keys: "tab", Scope: scopeApproval, Desc: "Edit the tool input before approving"},
		{Keys: "esc", Scope: scopeApproval, Desc: "Dismiss without replying"},

		// The amend editor swallows almost every key into the text buffer (it
		// reuses the shared input keymap), so only its two escapes are bindings
		// in the sense this table means — including ctrl+e, which in here is the
		// keymap's jump-to-line-end rather than the prompt's explain.
		{Keys: "ctrl+s", Scope: scopeAmend, Desc: "Approve the call with the edited input"},
		{Keys: "esc", Scope: scopeAmend, Desc: "Cancel the edit, back to the prompt"},

		// The rows gated on a MULTI-question request (tab/shift+tab, ←/→, n) are
		// listed unconditionally: this table has no way to express "only when the
		// agent asked more than one thing", and a user reading /help outside a
		// prompt has no request in front of them either way.
		{Keys: "↑/↓", Scope: scopeDecision, Desc: "Move between the offered answers"},
		{Keys: "1-9", Scope: scopeDecision, Desc: "Answer with that numbered option"},
		{Keys: "enter", Scope: scopeDecision, Desc: "Answer with the focused row, or submit"},
		{Keys: "tab/shift+tab", Scope: scopeDecision, Desc: "Move between questions (multi-question only)"},
		{Keys: "n", Scope: scopeDecision, Desc: "Annotate the focused answer (multi-question only)"},
		{Keys: "esc", Scope: scopeDecision, Desc: "Close an editor, else cancel the question"},
	}
}

// keymap is the whole table in /help's display order.
func keymap() []keyBinding {
	global := globalKeymap()
	out := make([]keyBinding, 0, len(global)+len(screenKeymap()))
	out = append(out, global...)
	return append(out, screenKeymap()...)
}

// dispatchGlobalKey runs the first [globalKeymap] row matching key, reporting
// whether one did. [App.handleKey] calls it ahead of the per-screen handlers,
// which is the precedence the previous per-screen `ctrl+c` cases had (each was
// the first case in its own switch), so routing them through the table changed
// no behavior.
//
// It sits BELOW the overlays on purpose: an open command panel, a pending
// approval, and the autocomplete popup each claim keys in [App.Update] before
// handleKey is reached, and each keeps its own ctrl+c. A global binding is
// "global across the screens", not "steals keys from a prompt the user is
// answering" — which for ctrl+y is also the conservative reading, since the
// prompt in front of the user is the one asking about the very tool call the
// toggle would stop gating.
func dispatchGlobalKey(a App, key tea.Key) (tea.Model, tea.Cmd, bool) {
	for _, b := range globalKeymap() {
		if b.match(key) {
			next, cmd := b.run(a)
			return next, cmd, true
		}
	}
	return a, nil, false
}
