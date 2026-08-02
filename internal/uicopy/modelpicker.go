package uicopy

// modelpicker.go holds the /model tab's copy: the free-text entry line, the
// zero-auth warning, and the "unknown" spellings of the metadata segments a
// model's description line is built from.

import "fmt"

// Model picker chrome.
const (
	// ModelPickerNoProviders replaces the row list when no provider is
	// authenticated.
	ModelPickerNoProviders = "No providers authenticated. Run /login (or 'gofer login <anthropic|openai>')."
	// ModelPickerEntryPrompt is the muted placeholder in the empty entry box.
	ModelPickerEntryPrompt = "Model id: (type any id, listed or not)"
	// ModelPickerEntryPrefix labels the entry box once something is typed.
	ModelPickerEntryPrefix = "Model id: "
)

// The metadata segments rendered when a value is not trustworthy — never a zero
// value dressed up as fact.
const (
	ModelPickerContextUnknown = "context unknown"
	ModelPickerPricingUnknown = "pricing unknown"
)

// ModelPickerContextSegment renders a known context window, e.g. "1M context".
func ModelPickerContextSegment(window string) string {
	return window + " context"
}

// ModelPickerContextOnly renders a catalog entry whose backend cannot be
// resolved: its head (name and id) plus a known context window, with pricing
// reported unknown.
func ModelPickerContextOnly(head, window string) string {
	return fmt.Sprintf("%s · %s context · pricing unknown", head, window)
}

// ModelPickerPricing renders per-Mtok input/output rates.
func ModelPickerPricing(input, output string) string {
	return fmt.Sprintf("$%s/$%s per Mtok", input, output)
}
