package uicopy

// command.go holds the slash-command dispatcher's operator copy: the builtin
// registry's one-line summaries and argument hints (internal/tui's /help and
// the command popup render them), plus the argument-validation prose the
// dispatcher puts on the status line.
//
// Command NAMES are deliberately absent — "/resume" is the syntax a user
// types, not copy, and translating it would break the command.

import "strconv"

// Builtin slash-command summaries — one line each, as listed by /help and the
// command popup.
const (
	CommandStatusSummary   = "Show session, cwd, and provider status"
	CommandUsageSummary    = "Show this session's token and cost consumption"
	CommandStatsSummary    = "Show session lifecycle and roster-wide totals"
	CommandConfigSummary   = "View and edit settings"
	CommandModelSummary    = "Pick the active/default model"
	CommandNewSummary      = "Start a new session and attach to it"
	CommandResumeSummary   = "Reopen a session from disk"
	CommandQuitSummary     = "Quit gofer"
	CommandThinkingSummary = "Set the reasoning effort for this session"
	CommandCompactSummary  = "Summarize this session's history and replace it with the summary"
	CommandContextSummary  = "Show how full this session's context window is"
	CommandMCPSummary      = "Show configured MCP servers and their connection state"
	CommandSkillsSummary   = "Show discovered SKILL.md skills and loader diagnostics"
	CommandYoloSummary     = "Toggle guardrails for new sessions"
	CommandHelpSummary     = "List commands and keys"
)

// Argument hints for the builtin commands that take one, shown beside the name
// in the command popup.
const (
	CommandModelArgHint    = "[id]"
	CommandResumeArgHint   = "[session-id]"
	CommandThinkingArgHint = "[low|medium|high|off]"
	CommandYoloArgHint     = "[on|off]"
)

// CommandNewTakesNoArgs is /new's rejection of stray arguments.
const CommandNewTakesNoArgs = "/new takes no arguments — it opens an empty session; type the prompt there"

// CommandResumeWantsOneID reports that /resume was given got arguments where a
// single session id was expected.
func CommandResumeWantsOneID(got int) string {
	return "/resume takes a single session id — got " + strconv.Itoa(got) + " arguments"
}

// CommandResumeInvalidID reports that id cannot name a session at all.
func CommandResumeInvalidID(id string) string {
	return "can't resume " + strconv.Quote(id) + ": not a valid session id"
}

// CommandModelWantsOneID reports that /model was given got arguments where a
// single model id was expected.
func CommandModelWantsOneID(got int) string {
	return "/model takes a single model id — got " + strconv.Itoa(got) + " arguments"
}

// CommandModelUnusable reports that model id cannot be routed, quoting reason —
// the provider's own explanation — verbatim.
func CommandModelUnusable(id, reason string) string {
	return "can't use model " + strconv.Quote(id) + ": " + reason
}

// CommandThinkingWantsOneLevel reports that /thinking was given got arguments
// where a single effort level was expected.
func CommandThinkingWantsOneLevel(got int) string {
	return "/thinking takes a single level — got " + strconv.Itoa(got) + " arguments"
}

// CommandThinkingUnusableLevel reports that level is outside the reasoning-effort
// vocabulary.
func CommandThinkingUnusableLevel(level string) string {
	return "can't use reasoning effort " + strconv.Quote(level) + ": want low, medium, high, or off"
}

// CommandUnknown reports that name resolves to no registered command. name
// arrives without its leading slash, which this restores.
func CommandUnknown(name string) string {
	return "unknown command: /" + name
}
