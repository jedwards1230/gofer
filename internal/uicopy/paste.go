package uicopy

import "fmt"

// Copy for bracketed paste (internal/tui/paste.go).

// PasteClipped is the caveat a paste that hit tui.max_paste_bytes posts. It
// names the knob so the operator can raise it, and reads as a caveat rather
// than a failure — the paste DID land, just truncated.
func PasteClipped(limit int) string {
	return fmt.Sprintf("paste clipped to %d bytes (tui.max_paste_bytes)", limit)
}
