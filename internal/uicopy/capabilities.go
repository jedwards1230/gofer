package uicopy

import "strconv"

// Copy for the /mcp and /skills command-panel tabs' shared capability body
// (internal/tui/capabilities.go).

// CapabilitiesUnknown opens the UNKNOWN body, naming the subject that could
// not be answered. The body is deliberately WORDY: the failure it exists to
// prevent is a reader mistaking an unanswered panel for an empty one, which
// the word "UNKNOWN" over a blank area does not prevent on its own.
func CapabilitiesUnknown(subject string) string {
	return subject + ": UNKNOWN — this backend cannot report it."
}

// The rest of the UNKNOWN body, one entry per rendered line.
//
// These read as fragments because they ARE one paragraph hard-wrapped at fixed
// render width — the breaks are layout, not sentence structure. Joining them
// into whole sentences would move the wrap points and rewrite every golden that
// pins this body, so it is deliberately deferred (gofer#290).
const (
	CapabilitiesUnknownNotEmpty = "This is NOT \"none configured\": nothing here has been read."
	CapabilitiesUnknownRouter1  = "An attached `gofer daemon --workers` router owns no supervisor, so"
	CapabilitiesUnknownRouter2  = "no MCP manager and no skill set exist in that process to report;"
	CapabilitiesUnknownRouter3  = "each session's worker owns its own. A daemon older than"
	CapabilitiesUnknownRouter4  = "gofer/capabilities answers the same way."
)

// CapabilitiesLoading is the in-flight body both tabs render.
func CapabilitiesLoading(subject string) string { return "Loading " + subject + "…" }

// CapabilitiesMoreRows is the overflow notice that stands in for the list rows
// a short panel could not fit. It carries its own leading indent, which aligns
// it under the items it replaces.
func CapabilitiesMoreRows(n int) string { return "  +" + strconv.Itoa(n) + " more" }
