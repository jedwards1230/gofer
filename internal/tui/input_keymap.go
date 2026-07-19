package tui

// input_keymap.go maps a key press to the [inputBuffer] editing operation it
// performs — the one keymap definition [App.handleOverviewKey] and
// [App.handleAttachKey] both fall through to for whichever key their own
// nav-specific switch doesn't already claim (see applyInputKey's doc). This
// keeps the actual readline/macOS-style keymap in one place rather than
// duplicated across the two screens.

import tea "charm.land/bubbletea/v2"

// applyInputKey applies key to buf as an editing operation, returning the
// updated buffer and whether the key was consumed. It is the shared keymap
// behind both text-entry surfaces (the overview dispatch bar, the attach
// input): character/word/line movement, insertion at the cursor, and
// character/word/line deletion in both directions — bound to the modifiers
// bubbletea v2.0.8 actually delivers (Option/Alt reliably reaches the app as
// [tea.ModAlt] on terminals that forward it at all, e.g. Ghostty; Cmd/Super
// does not reliably reach a terminal program at all, so Home/End/Ctrl-A/
// Ctrl-E are the dependable "jump to line start/end" bindings, not a Cmd+
// pairing).
//
// Callers run their own nav-specific switch FIRST and only fall through to
// this for keys it doesn't already bind to navigation: Enter/Escape/Ctrl-C
// are always screen-level, and each screen's one conditional-nav arrow is
// handled by the caller rather than here — a bare (unmodified) Right on the
// overview (attach the selected session when the dispatch bar is empty, else
// move the cursor right — see handleOverviewKey) and a bare Left on attach
// (back out to the overview when the input is empty, else move the cursor
// left — see handleAttachKey). Both branches of both arrows are the caller's;
// this function is never reached for either.
func applyInputKey(buf inputBuffer, key tea.Key) (inputBuffer, bool) {
	switch {
	case key.Code == tea.KeyLeft && key.Mod.Contains(tea.ModAlt):
		return buf.MoveWordLeft(), true
	case key.Code == tea.KeyRight && key.Mod.Contains(tea.ModAlt):
		return buf.MoveWordRight(), true
	case key.Code == tea.KeyLeft:
		return buf.MoveLeft(), true
	case key.Code == tea.KeyRight:
		return buf.MoveRight(), true
	case key.Code == tea.KeyHome:
		return buf.MoveHome(), true
	case key.Code == tea.KeyEnd:
		return buf.MoveEnd(), true
	case key.Mod.Contains(tea.ModCtrl) && key.Code == 'a':
		return buf.MoveHome(), true
	case key.Mod.Contains(tea.ModCtrl) && key.Code == 'e':
		return buf.MoveEnd(), true
	case key.Code == tea.KeyBackspace && key.Mod.Contains(tea.ModAlt):
		return buf.DeleteWordBackward(), true
	case key.Mod.Contains(tea.ModCtrl) && key.Code == 'w':
		return buf.DeleteWordBackward(), true
	case key.Mod.Contains(tea.ModCtrl) && key.Code == 'u':
		return buf.DeleteToLineStart(), true
	case key.Mod.Contains(tea.ModCtrl) && key.Code == 'k':
		return buf.DeleteToLineEnd(), true
	case key.Code == tea.KeyBackspace:
		return buf.Backspace(), true
	case key.Code == tea.KeyDelete:
		return buf.DeleteForward(), true
	case key.Mod.Contains(tea.ModCtrl) && key.Code == 'd':
		return buf.DeleteForward(), true
	case key.Text != "":
		return buf.InsertText(key.Text), true
	}
	return buf, false
}
