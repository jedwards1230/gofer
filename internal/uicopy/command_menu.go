package uicopy

// command_menu.go holds the command-autocomplete popup's copy: the two
// overflow indicators that say how many candidates are scrolled out of view.

import "fmt"

// CommandMenuMoreAbove reports n candidates scrolled off the top of the popup.
func CommandMenuMoreAbove(n int) string {
	return fmt.Sprintf("↑ %d more", n)
}

// CommandMenuMoreBelow reports n candidates scrolled off the bottom of the
// popup.
func CommandMenuMoreBelow(n int) string {
	return fmt.Sprintf("↓ %d more", n)
}
