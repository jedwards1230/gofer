package uicopy

// effortpicker.go holds the /thinking tab's copy: the per-level blurbs and the
// header naming the model whose capability gates the list. The level SPELLINGS
// ("off"/"low"/"medium"/"high") stay in internal/tui — they are the vocabulary
// `/thinking <level>` parses and the config file stores, not copy.

// Per-level blurbs, in the picker's row order.
const (
	EffortOffBlurb    = "no explicit level; the provider decides"
	EffortLowBlurb    = "least reasoning, fastest turns"
	EffortMediumBlurb = "balanced reasoning"
	EffortHighBlurb   = "most reasoning, slowest and priciest turns"
)

// EffortPickerHeader is the header when no model is named.
const EffortPickerHeader = "Reasoning effort:"

// EffortPickerHeaderFor is the header naming the model under discussion.
func EffortPickerHeaderFor(model string) string {
	return "Reasoning effort for " + model + ":"
}

// EffortPickerUnsupported replaces the header when the registry knows model
// cannot reason. Kept short on purpose: the line is truncated to the panel
// width, and a remedy that falls off the right edge leaves only the complaint.
func EffortPickerUnsupported(model string) string {
	return model + " doesn't support reasoning effort — switch with /model."
}
