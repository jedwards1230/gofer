package uicopy

// Copy for [gofer/internal/tui]'s declarative key table (internal/tui/keymap.go)
// — the /help Keys section. Only the descriptions and the section headings live
// here; the Keys column ("ctrl+c", "↑/↓", "pgup/pgdn") is display chrome naming
// physical keys, never translated, and stays in the table.
//
// The constants below are in the same order as the bindings they describe, so
// the two files read side by side.

// Section headings /help groups the bindings under, one per key scope.
const (
	KeyScopeRoster        = "Roster"
	KeyScopePeek          = "Peek"
	KeyScopeAttach        = "Attach"
	KeyScopeTextEntry     = "Text entry"
	KeyScopeInputPrefixes = "Input prefixes"
	KeyScopeCommandPanel  = "Command panel"
	KeyScopeAutocomplete  = "Autocomplete popup"
	KeyScopeApproval      = "Approval prompt"
	KeyScopeAmend         = "Amending a tool call"
	KeyScopeDecision      = "Agent question (ask_user)"
	KeyScopeGlobal        = "Global"
)

// Global bindings — the live half of the table, dispatched on every screen.
const (
	KeyQuit             = "Quit gofer (press twice)"
	KeyToggleGuardrails = "Toggle guardrails for new sessions (same as /yolo)"
	KeySelectAll        = "Select the whole screen and copy it (empty input bar)"
	KeyCopyTranscript   = "Copy the WHOLE transcript, scrolled-off content included (attach, empty input)"
	KeyToggleShellReply = "Toggle reply-on-run for shell commands (reply now / queue)"
)

// Roster (overview) bindings. KeyRosterMove and KeyRosterDelete are shared with
// peek, which offers the same two actions under the same words.
const (
	KeyRosterMove          = "Move the roster selection"
	KeyRosterOpenOrRun     = "Open the selected session, or run what's typed"
	KeyRosterOpen          = "Open the selected session (empty dispatch bar)"
	KeyRosterPeek          = "Peek the selected session (empty dispatch bar)"
	KeyRosterToggleView    = "Switch flat / grouped roster view"
	KeyRosterClearDispatch = "Clear the dispatch bar"
	KeyRosterDelete        = "Delete the selected session (press twice to confirm)"
	KeyRosterStopSubagents = "Stop every subagent under the selected session"
	KeyRosterRestartDaemon = "Restart a stale daemon (shown only when the daemon is out of date)"
	KeyRosterScroll        = "Scroll the roster"
	KeyRosterHelp          = "Open this help (empty dispatch bar)"
)

// Peek bindings.
const (
	KeyPeekOpenOrReply = "Open the session, or send the typed reply"
	KeyPeekCloseEmpty  = "Close back to the roster (empty reply)"
	KeyPeekClose       = "Close back to the roster"
)

// Attach bindings.
const (
	KeyAttachBack           = "Back out to the parent session, else the roster (empty input)"
	KeyAttachDrillSubagents = "Drill into this session's subagents (empty input)"
	KeyAttachSend           = "Send the prompt, or run a /command"
	KeyAttachInterrupt      = "Interrupt the running turn"
	KeyAttachScroll         = "Scroll the transcript"
)

// Text-entry (readline) bindings, shared by every input bar.
const (
	KeyInputMoveChar          = "Move one character"
	KeyInputMoveWord          = "Move one word"
	KeyInputLineStart         = "Move to line start (ctrl+a selects the screen when the bar is empty)"
	KeyInputLineEnd           = "Move to line end"
	KeyInputDeleteCharBefore  = "Delete the character before the cursor"
	KeyInputDeleteCharAt      = "Delete the character at the cursor"
	KeyInputDeleteWordBefore  = "Delete the word before the cursor"
	KeyInputDeleteToLineStart = "Delete to line start"
	KeyInputDeleteToLineEnd   = "Delete to line end"
)

// Input prefixes — a submit-time grammar rather than keys, listed beside the
// bindings because it is the least discoverable part of the input surface.
const (
	KeyPrefixSlash      = "Run a slash command (see Commands above)"
	KeyPrefixShell      = "Run a shell command; its output joins the model's context"
	KeyPrefixShellQuiet = "Run a shell command, keeping its output OUT of context"
	KeyPrefixPath       = "Complete a file path into the prompt (the path, not its contents)"
)

// Autocomplete popup bindings.
const (
	KeyMenuMove     = "Move the highlighted entry"
	KeyMenuComplete = "Complete the highlighted entry"
	KeyMenuRun      = "Run the highlighted command"
	KeyMenuClose    = "Close the popup, keep the typed text"
)

// Command panel bindings.
const (
	KeyPanelSwitchTabs = "Switch tabs"
	KeyPanelMove       = "Move within the active tab"
	KeyPanelCommit     = "Commit the active tab's selection"
	KeyPanelClose      = "Clear the tab's state, then close the panel"
)

// Approval prompt bindings.
const (
	KeyApprovalAllow    = "Allow the tool call"
	KeyApprovalDeny     = "Deny the tool call"
	KeyApprovalRemember = "Toggle remember"
	KeyApprovalExplain  = "Ask the agent why it wants this call"
	KeyApprovalAmend    = "Edit the tool input before approving"
	KeyApprovalDismiss  = "Dismiss without replying"
)

// Amend-editor bindings.
const (
	KeyAmendApprove = "Approve the call with the edited input"
	KeyAmendCancel  = "Cancel the edit, back to the prompt"
)

// Agent-question (ask_user) bindings.
const (
	KeyDecisionMoveAnswer   = "Move between the offered answers"
	KeyDecisionNumbered     = "Answer with that numbered option"
	KeyDecisionSubmit       = "Answer with the focused row, or submit"
	KeyDecisionMoveQuestion = "Move between questions (multi-question only)"
	KeyDecisionAnnotate     = "Annotate the focused answer (multi-question only)"
	KeyDecisionCancel       = "Close an editor, else cancel the question"
)
