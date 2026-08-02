package tui

// keymap.go is the TUI's single declarative key table — the thing /help
// (help.go) renders its Keys section from.
//
// HOW LIVE IS IT? Deliberately, and visibly, two-tier:
//
//   - [globalKeymap]'s rows are LIVE. Each carries a match predicate and the
//     action it runs, and [App.handleKey] dispatches through
//     [dispatchGlobalKey] before it reaches any per-screen handler. There is
//     exactly one definition of ctrl+c and ctrl+y, and /help reads it. ctrl+c
//     is also claimed early by three overlays this table sits below (the
//     command panel, the pending-approval prompt, the pending-decision
//     prompt — see App.Update's dispatch precedence and panel.go/dialog.go),
//     so "one definition" means those sites all call the row's own action,
//     [App.confirmQuit], rather than each carrying its own copy of the quit
//     behavior (gofer#314) — the table's run closure and the overlays'
//     ctrl+c cases are the same function, not three definitions that happen
//     to agree today.
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

import (
	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/uicopy"
)

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
		return uicopy.KeyScopeRoster
	case scopePeek:
		return uicopy.KeyScopePeek
	case scopeAttach:
		return uicopy.KeyScopeAttach
	case scopeInput:
		return uicopy.KeyScopeTextEntry
	case scopePrefix:
		return uicopy.KeyScopeInputPrefixes
	case scopePanel:
		return uicopy.KeyScopeCommandPanel
	case scopeMenu:
		return uicopy.KeyScopeAutocomplete
	case scopeApproval:
		return uicopy.KeyScopeApproval
	case scopeAmend:
		return uicopy.KeyScopeAmend
	case scopeDecision:
		return uicopy.KeyScopeDecision
	default:
		return uicopy.KeyScopeGlobal
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
	// enabled, when non-nil, gates a matched binding on app state: false
	// makes dispatchGlobalKey report the key UNHANDLED, so it falls through
	// to the per-screen switch and the shared input keymap exactly as if
	// this row did not exist.
	//
	// It exists so a global row can share a key with a text-entry binding
	// without stealing it. ctrl+a is the case: on an empty dispatch bar it
	// selects the frame, and with text in the bar it stays "move to line
	// start" (applyInputKey). That is the same "empty dispatch bar" idiom
	// space / ? / → already use on the overview, just expressed here rather
	// than in a screen's switch, because this binding applies on every
	// screen.
	enabled func(App) bool
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
			Desc:  uicopy.KeyQuit,
			match: func(k tea.Key) bool { return k.Mod.Contains(tea.ModCtrl) && k.Code == 'c' },
			run:   func(a App) (tea.Model, tea.Cmd) { return a.confirmQuit() },
		},
		{
			Keys:  "ctrl+y",
			Scope: scopeGlobal,
			Desc:  uicopy.KeyToggleGuardrails,
			match: func(k tea.Key) bool { return k.Mod.Contains(tea.ModCtrl) && k.Code == 'y' },
			run: func(a App) (tea.Model, tea.Cmd) {
				next, cmd := a.applyPermissionMode(yoloToggle)
				return next, cmd
			},
		},
		{
			Keys:  "ctrl+a",
			Scope: scopeGlobal,
			Desc:  uicopy.KeySelectAll,
			match: func(k tea.Key) bool { return k.Mod.Contains(tea.ModCtrl) && k.Code == 'a' },
			// Only on an empty input bar — with text in it, ctrl+a stays
			// "move to line start" (see keyBinding.enabled).
			enabled: func(a App) bool { return a.inputEmpty() },
			run: func(a App) (tea.Model, tea.Cmd) {
				next, cmd := a.selectAll()
				return next, cmd
			},
		},
		{
			Keys:  "alt+a",
			Scope: scopeGlobal,
			Desc:  uicopy.KeyCopyTranscript,
			match: func(k tea.Key) bool { return k.Mod.Contains(tea.ModAlt) && k.Code == 'a' },
			// Attach only — it is the only screen with a transcript — and only on
			// an empty input bar, matching ctrl+a's gate so the two select/copy
			// keys behave consistently rather than one stealing a key the other
			// yields.
			enabled: func(a App) bool { return a.scr == screenAttach && a.inputEmpty() },
			run: func(a App) (tea.Model, tea.Cmd) {
				next, cmd := a.copyTranscript()
				return next, cmd
			},
		},
		{
			Keys:  "ctrl+r",
			Scope: scopeGlobal,
			Desc:  uicopy.KeyToggleShellReply,
			match: func(k tea.Key) bool { return k.Mod.Contains(tea.ModCtrl) && k.Code == 'r' },
			run: func(a App) (tea.Model, tea.Cmd) {
				// No status note: the shell-mode rule label already flips between
				// reply-now and queue on the next frame, and that IS the feedback
				// (round-4 ask — a status line here was redundant noise).
				a.shellQueue = !a.shellQueue
				return a, nil
			},
		},
	}
}

// screenKeymap is the descriptive-only half — see the file doc for why these
// rows are not dispatched from here and what that costs.
func screenKeymap() []keyBinding {
	return []keyBinding{
		{Keys: "↑/↓", Scope: scopeOverview, Desc: uicopy.KeyRosterMove},
		{Keys: "enter", Scope: scopeOverview, Desc: uicopy.KeyRosterOpenOrRun},
		{Keys: "→", Scope: scopeOverview, Desc: uicopy.KeyRosterOpen},
		{Keys: "space", Scope: scopeOverview, Desc: uicopy.KeyRosterPeek},
		{Keys: "tab", Scope: scopeOverview, Desc: uicopy.KeyRosterToggleView},
		{Keys: "esc", Scope: scopeOverview, Desc: uicopy.KeyRosterClearDispatch},
		{Keys: "ctrl+x", Scope: scopeOverview, Desc: uicopy.KeyRosterDelete},
		{Keys: "ctrl+t", Scope: scopeOverview, Desc: uicopy.KeyRosterStopSubagents},
		{Keys: "R", Scope: scopeOverview, Desc: uicopy.KeyRosterRestartDaemon},
		{Keys: "pgup/pgdn", Scope: scopeOverview, Desc: uicopy.KeyRosterScroll},
		{Keys: "?", Scope: scopeOverview, Desc: uicopy.KeyRosterHelp},

		{Keys: "enter", Scope: scopePeek, Desc: uicopy.KeyPeekOpenOrReply},
		{Keys: "space", Scope: scopePeek, Desc: uicopy.KeyPeekCloseEmpty},
		{Keys: "esc", Scope: scopePeek, Desc: uicopy.KeyPeekClose},
		{Keys: "↑/↓", Scope: scopePeek, Desc: uicopy.KeyRosterMove},
		{Keys: "ctrl+x", Scope: scopePeek, Desc: uicopy.KeyRosterDelete},

		{Keys: "←", Scope: scopeAttach, Desc: uicopy.KeyAttachBack},
		{Keys: "↓", Scope: scopeAttach, Desc: uicopy.KeyAttachDrillSubagents},
		{Keys: "enter", Scope: scopeAttach, Desc: uicopy.KeyAttachSend},
		{Keys: "esc", Scope: scopeAttach, Desc: uicopy.KeyAttachInterrupt},
		{Keys: "pgup/pgdn", Scope: scopeAttach, Desc: uicopy.KeyAttachScroll},

		{Keys: "←/→", Scope: scopeInput, Desc: uicopy.KeyInputMoveChar},
		{Keys: "alt+←/→", Scope: scopeInput, Desc: uicopy.KeyInputMoveWord},
		{Keys: "home/ctrl+a", Scope: scopeInput, Desc: uicopy.KeyInputLineStart},
		{Keys: "end/ctrl+e", Scope: scopeInput, Desc: uicopy.KeyInputLineEnd},
		{Keys: "backspace", Scope: scopeInput, Desc: uicopy.KeyInputDeleteCharBefore},
		{Keys: "delete/ctrl+d", Scope: scopeInput, Desc: uicopy.KeyInputDeleteCharAt},
		{Keys: "alt+backspace/ctrl+w", Scope: scopeInput, Desc: uicopy.KeyInputDeleteWordBefore},
		{Keys: "ctrl+u", Scope: scopeInput, Desc: uicopy.KeyInputDeleteToLineStart},
		{Keys: "ctrl+k", Scope: scopeInput, Desc: uicopy.KeyInputDeleteToLineEnd},

		// The input prefixes are a SUBMIT-TIME grammar rather than keys (see
		// App.dispatchInput, shell.go), but they are the part of the input
		// surface a user is least likely to discover unaided, so /help carries
		// them beside the bindings.
		{Keys: "/name", Scope: scopePrefix, Desc: uicopy.KeyPrefixSlash},
		{Keys: "!cmd", Scope: scopePrefix, Desc: uicopy.KeyPrefixShell},
		{Keys: "!!cmd", Scope: scopePrefix, Desc: uicopy.KeyPrefixShellQuiet},
		{Keys: "@path", Scope: scopePrefix, Desc: uicopy.KeyPrefixPath},

		{Keys: "↑/↓", Scope: scopeMenu, Desc: uicopy.KeyMenuMove},
		{Keys: "tab", Scope: scopeMenu, Desc: uicopy.KeyMenuComplete},
		{Keys: "enter", Scope: scopeMenu, Desc: uicopy.KeyMenuRun},
		{Keys: "esc", Scope: scopeMenu, Desc: uicopy.KeyMenuClose},

		{Keys: "←/→", Scope: scopePanel, Desc: uicopy.KeyPanelSwitchTabs},
		{Keys: "↑/↓", Scope: scopePanel, Desc: uicopy.KeyPanelMove},
		{Keys: "enter", Scope: scopePanel, Desc: uicopy.KeyPanelCommit},
		{Keys: "esc", Scope: scopePanel, Desc: uicopy.KeyPanelClose},

		{Keys: "a/y/1", Scope: scopeApproval, Desc: uicopy.KeyApprovalAllow},
		{Keys: "d/n/2", Scope: scopeApproval, Desc: uicopy.KeyApprovalDeny},
		{Keys: "r", Scope: scopeApproval, Desc: uicopy.KeyApprovalRemember},
		{Keys: "ctrl+e", Scope: scopeApproval, Desc: uicopy.KeyApprovalExplain},
		{Keys: "tab", Scope: scopeApproval, Desc: uicopy.KeyApprovalAmend},
		{Keys: "esc", Scope: scopeApproval, Desc: uicopy.KeyApprovalDismiss},

		// The amend editor swallows almost every key into the text buffer (it
		// reuses the shared input keymap), so only its two escapes are bindings
		// in the sense this table means — including ctrl+e, which in here is the
		// keymap's jump-to-line-end rather than the prompt's explain.
		{Keys: "ctrl+s", Scope: scopeAmend, Desc: uicopy.KeyAmendApprove},
		{Keys: "esc", Scope: scopeAmend, Desc: uicopy.KeyAmendCancel},

		// The rows gated on a MULTI-question request (tab/shift+tab, ←/→, n) are
		// listed unconditionally: this table has no way to express "only when the
		// agent asked more than one thing", and a user reading /help outside a
		// prompt has no request in front of them either way.
		{Keys: "↑/↓", Scope: scopeDecision, Desc: uicopy.KeyDecisionMoveAnswer},
		{Keys: "1-9", Scope: scopeDecision, Desc: uicopy.KeyDecisionNumbered},
		{Keys: "enter", Scope: scopeDecision, Desc: uicopy.KeyDecisionSubmit},
		{Keys: "tab/shift+tab", Scope: scopeDecision, Desc: uicopy.KeyDecisionMoveQuestion},
		{Keys: "n", Scope: scopeDecision, Desc: uicopy.KeyDecisionAnnotate},
		{Keys: "esc", Scope: scopeDecision, Desc: uicopy.KeyDecisionCancel},
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
		if !b.match(key) {
			continue
		}
		if b.enabled != nil && !b.enabled(a) {
			// Gated off: report UNHANDLED so the key falls through to the
			// per-screen switch / input keymap. Returning `true` here would
			// swallow it into a no-op instead.
			return a, nil, false
		}
		next, cmd := b.run(a)
		return next, cmd, true
	}
	return a, nil, false
}
