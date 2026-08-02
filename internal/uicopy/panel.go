package uicopy

// panel.go holds the command panel's own copy (internal/tui/panel.go): the tab
// bar's labels, each tab's footer key hint, and the status notes a committed
// /model or /thinking select leaves behind.
//
// The status notes are deliberately STANDALONE strings rather than a base note
// plus a composed suffix, and the daemon-attached ones name no model id — the
// status line is truncated to the terminal width, so a qualification that gets
// cut off leaves exactly the unqualified overclaim behind. See
// [gofer/internal/tui.App.withDefaultReach] for the full reasoning; keep any
// reworded note inside the same width budget.

// Tab-bar labels, in left-to-right panel order.
const (
	PanelTabStatus   = "Status"
	PanelTabConfig   = "Config"
	PanelTabModel    = "Model"
	PanelTabThinking = "Thinking"
	PanelTabUsage    = "Usage"
	PanelTabStats    = "Stats"
	PanelTabContext  = "Context"
	PanelTabMCP      = "MCP"
	PanelTabSkills   = "Skills"
	PanelTabResume   = "Resume"
	PanelTabHelp     = "Help"
)

// Footer key hints — one per tab with its own key contract, plus the default
// the read-only tabs show.
const (
	PanelFooterConfig  = "Type to filter · Enter/↓ to select · ↑ to tabs · Esc to clear"
	PanelFooterModel   = "Type a model id · ↑/↓ to browse · Enter to select · Esc to clear"
	PanelFooterResume  = "Type to filter · ↑/↓ to browse · Enter to resume · Esc to clear"
	PanelFooterEffort  = "↑/↓ to choose · Enter to select · Esc to close"
	PanelFooterHelp    = "↑/↓ to scroll · ←/→ to switch tabs · esc to close"
	PanelFooterDefault = "←/→ to switch tabs · esc to close"
)

// PanelConfigLoadFailed reports that the config read behind a /model or
// /thinking commit failed, so nothing was written.
func PanelConfigLoadFailed(reason string) string {
	return "couldn't load config: " + reason
}

// PanelSaveDefaultModelFailed reports that persisting session.model failed.
func PanelSaveDefaultModelFailed(reason string) string {
	return "couldn't save default model: " + reason
}

// PanelSaveDefaultEffortFailed reports that persisting session.effort failed.
func PanelSaveDefaultEffortFailed(reason string) string {
	return "couldn't save default reasoning effort: " + reason
}

// PanelDefaultModelSet reports a default-only /model select from the overview,
// where no session was running to swap. model is the display name.
func PanelDefaultModelSet(model string) string {
	return "Default model set to " + model + "."
}

// PanelModelSet reports a /model select that also hot-swapped the attached
// session's live model. model is the display name.
func PanelModelSet(model string) string {
	return "Model set to " + model + "."
}

// The synchronous halves of a committed /model select: what is true before
// [gofer/internal/tui.App.probeDaemonDefaultCmd]'s answer lands. Each pair is
// local backend first, daemon-attached second.
const (
	PanelDefaultSavedDaemonHedged     = "Default saved; attached daemon adopts it unless pinned."
	PanelLiveSwapNeedsSameProvider    = "Live model swap needs the same provider — this session keeps its model."
	PanelProviderDiffersDefaultSaved  = "Provider differs — session keeps its model; default saved."
	PanelLiveSwapNeedsSameProviderNew = "Live model swap needs the same provider — default set for new sessions; this session keeps its model."
	PanelModelSetDaemonHedged         = "Model set for this session; daemon adopts the default unless pinned."
)

// The definitive notes that replace the hedged ones once the daemon has
// reported its CURRENT default: adopted (the write reached it) or pinned (it
// was started with an explicit --model).
const (
	PanelProviderDiffersDaemonAdopted = "Provider differs — session keeps its model; daemon took the default."
	PanelProviderDiffersDaemonPinned  = "Provider differs — session keeps its model; daemon is pinned."
	PanelModelSetDaemonAdopted        = "Model set for this session; the daemon took the new default."
	PanelModelSetDaemonPinned         = "Model set for this session; the daemon is pinned to another default."
	PanelDefaultSavedDaemonAdopted    = "Default model saved; the attached daemon adopted it."
	PanelDefaultSavedDaemonPinned     = "Default saved; the attached daemon is pinned to another model."
)

// PanelEffortUnsupported refuses a reasoning-effort level for a model the
// registry says cannot reason. model is the display name.
func PanelEffortUnsupported(model string) string {
	return model + " doesn't support reasoning effort — switch with /model."
}

// PanelDefaultEffortSaved reports a default-only /thinking commit from the
// overview. level is the effort's display label.
func PanelDefaultEffortSaved(level string) string {
	return "Default reasoning effort saved: " + level + "."
}

// PanelEffortSet reports a /thinking commit that also changed the attached
// session. level is the effort's display label.
func PanelEffortSet(level string) string {
	return "Reasoning effort set to " + level + " for this session."
}
