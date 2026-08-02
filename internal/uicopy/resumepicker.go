package uicopy

// resumepicker.go holds the /resume tab's copy: the search box, the four state
// lines that replace the row list, and the unknown-title/unknown-age
// fallbacks.

// The state lines that replace the row list. Each is a distinct fact —
// collapsing "not loaded yet" into "no sessions" would assert something the
// picker does not know.
const (
	ResumePickerLoading    = "Loading sessions…"
	ResumePickerNoSessions = "No sessions on disk yet."
	ResumePickerNoMatches  = "No sessions match."
)

// Search box copy: the muted placeholder, and the prefix once something is
// typed.
const (
	ResumePickerSearchPrompt = "Search sessions…"
	ResumePickerSearchPrefix = "Search: "
)

// ResumePickerUntitled stands in for a session whose journal carried no title,
// keeping the title column occupied rather than opening the row on its id.
const ResumePickerUntitled = "(untitled)"

// ResumePickerAgeUnknown stands in for a session whose journal carried no
// usable timestamp.
const ResumePickerAgeUnknown = "age unknown"

// ResumePickerListFailed reports that the session listing failed, quoting
// reason verbatim.
func ResumePickerListFailed(reason string) string {
	return "Couldn't list sessions: " + reason
}
