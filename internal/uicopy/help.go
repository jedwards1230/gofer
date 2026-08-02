package uicopy

// help.go holds the /help body's own copy (internal/tui/help.go). Everything
// else /help renders comes from the live sources it lists: the command
// summaries in command.go, the key descriptions and section headings in
// keymap.go.

import "fmt"

// HelpCommandsHeading heads the command section. The key sections' headings are
// keymap.go's KeyScope* values.
const HelpCommandsHeading = "Commands"

// HelpMoreAbove reports the rows scrolled off the top, mirroring the
// autocomplete popup's wording.
func HelpMoreAbove(n int) string {
	return fmt.Sprintf("↑ %d more", n)
}

// HelpMoreBelow reports the rows scrolled off the bottom, mirroring the
// autocomplete popup's wording.
func HelpMoreBelow(n int) string {
	return fmt.Sprintf("↓ %d more", n)
}
