package uicopy

// context.go holds the /context tab's copy (internal/tui/context.go): the
// context-window pressure rows and the two states that have no figures to
// report.
//
// The fill bar itself and its percentage are drawn, not written, and stay in
// internal/tui.

import (
	"fmt"
	"strconv"
)

// The two states with nothing measured to show. Both are single lines rather
// than a wall of zeros or a percentage divided by nothing.
const (
	ContextNoSession = "No active session — attach to see its context usage."
	ContextNoTurn    = "No turn has completed yet — nothing measured."
)

// ContextModelRow names the model the measurement belongs to.
func ContextModelRow(model string) string {
	return "Model: " + model
}

// ContextWindowUnknown reports an unregistered model, whose window size gofer
// does not know — so no percentage is shown and nothing is divided by it.
const ContextWindowUnknown = "Context window: unknown for this model"

// ContextLastMeasured reports the last turn's input count on its own, the only
// honest row when the window size is unknown.
func ContextLastMeasured(tokens int) string {
	return "Last measured input: " + strconv.Itoa(tokens) + " tokens"
}

// ContextWindowRow reports the active model's registered capacity.
func ContextWindowRow(tokens int) string {
	return "Context window: " + strconv.Itoa(tokens) + " tokens"
}

// ContextInUse reports the measured fill — a real number the provider computed
// for the last turn, not an estimate.
func ContextInUse(tokens int) string {
	return "In use: " + strconv.Itoa(tokens) + " tokens (measured — this session's last turn)"
}

// ContextFree reports what is left of the window.
func ContextFree(tokens int) string {
	return "Free: " + strconv.Itoa(tokens) + " tokens"
}

// ContextAutoCompactionOff reports that automatic compaction is disabled, and
// that the manual command remains.
const ContextAutoCompactionOff = "Auto-compaction: off (/compact still available)"

// ContextAutoCompactsAt reports the configured automatic-compaction threshold —
// a POLICY value, unlike every measured row above it. pct is a percentage.
func ContextAutoCompactsAt(pct float64) string {
	return fmt.Sprintf("Auto-compacts at: %.0f%% (/compact to do it now)", pct)
}
