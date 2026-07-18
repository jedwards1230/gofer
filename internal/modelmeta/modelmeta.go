// Package modelmeta is gofer's single source of truth for the short, friendly
// display name it shows per model id. The SDK's provider registry
// (provider.Lookup) carries a model's limits, pricing, and capabilities but no
// human label, so gofer keeps its own here. It is a leaf package (stdlib only)
// so both the TUI (internal/tui) and the daemon (internal/daemon) can share the
// one table without an import cycle.
package modelmeta

// displayNames is gofer's own short display name per model id, keyed by the SDK
// catalog's id — provider.Lookup carries limits/pricing but no friendly name
// (docs/projects/gofer-m4-command-views-plan.md §4a). A model id absent from
// this table (a newly registered SDK model gofer hasn't labeled yet) falls back
// to the raw id, never a blank name (see [DisplayName]).
var displayNames = map[string]string{
	"claude-fable-5":   "Fable 5",
	"claude-opus-4-8":  "Opus 4.8",
	"claude-sonnet-5":  "Sonnet 5",
	"claude-haiku-4-5": "Haiku 4.5",
	"gpt-5":            "GPT-5",
	"gpt-5-mini":       "GPT-5 mini",
	"gpt-5-nano":       "GPT-5 nano",
	"o4-mini":          "o4-mini",
}

// DisplayName returns id's short display name, falling back to id itself when
// the model isn't in the table.
func DisplayName(id string) string {
	if name, ok := displayNames[id]; ok {
		return name
	}
	return id
}
