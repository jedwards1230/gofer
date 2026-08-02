package uicopy

// config_view.go holds the /config tab's chrome (internal/tui/config_view.go):
// the search box and the trailing error row. The setting rows' own labels live
// in settings.go, beside the registry that declares them.

// ConfigNoMatches replaces the settings list when the filter excludes every
// row.
const ConfigNoMatches = "No settings match."

// ConfigError renders the trailing row a failed config read or write leaves,
// shown until the next successful edit clears it.
func ConfigError(reason string) string {
	return "Error: " + reason
}

// ConfigSearchPlaceholder is the search box's empty state.
const ConfigSearchPlaceholder = "Search settings…"

// ConfigSearchPrefix renders the search box with filter text typed into it.
func ConfigSearchPrefix(filter string) string {
	return "Search: " + filter
}
