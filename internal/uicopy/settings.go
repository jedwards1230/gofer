package uicopy

// settings.go holds the display labels for the setting registry the /config
// view renders. A setting's Key ("session.model") and its enum Options
// ("preload"/"index") are config values rather than copy and stay in
// internal/tui.

// Setting row labels, in registry order.
const (
	SettingsDefaultModel         = "Default model"
	SettingsReasoningEffort      = "Reasoning effort"
	SettingsPermissionMode       = "Permission mode"
	SettingsRosterView           = "Roster view"
	SettingsAutoscrollTranscript = "Auto-scroll transcript"
	SettingsMouseCapture         = "Mouse capture (scroll + selection)"
	SettingsShellReplyMode       = "Shell reply mode"
	SettingsAutomaticCompaction  = "Automatic compaction"
	SettingsLSPDiagnostics       = "LSP diagnostics"
	SettingsToolSchemaMode       = "Tool schema mode"
	SettingsWebSearchProvider    = "Web search provider"
	SettingsTelemetryEnabled     = "Telemetry enabled"
	SettingsTelemetryEndpoint    = "Telemetry endpoint"
)
