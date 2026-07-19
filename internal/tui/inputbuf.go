package tui

// inputbuf.go implements [inputBuffer]: the cursor-aware text buffer shared
// by the overview dispatch bar (Overview.input) and the attach input
// (Model.input) — the two text-entry surfaces docs/TUI.md's slash-command
// grammar covers (command_menu.go). Before this, both were append-only
// (TypeRune appended, Backspace dropped the last rune, no cursor concept at
// all); this replaces that with a real position — text plus a cursor index
// — so the native readline/macOS editing keymap (see applyInputKey, app.go)
// can insert, delete, and move mid-buffer, not just at the end. Like every
// other TUI value it is copy-on-write: every method returns an updated
// copy.

import "unicode"

// inputBuffer is text plus a cursor index — a rune offset into text, never a
// byte offset, so multi-byte UTF-8 content (a pasted emoji, non-ASCII
// prompt text) never splits mid-rune. The zero value is an empty buffer with
// the cursor at position 0.
type inputBuffer struct {
	text   string
	cursor int // rune index into text, always kept in [0, len(runes(text))]
}

// runes returns text as a rune slice — the unit every operation below
// indexes by.
func (b inputBuffer) runes() []rune { return []rune(b.text) }

// clampCursor returns cur clamped to [0, n].
func clampCursor(cur, n int) int {
	if cur < 0 {
		return 0
	}
	if cur > n {
		return n
	}
	return cur
}

// String returns the buffer's text.
func (b inputBuffer) String() string { return b.text }

// Cursor returns the buffer's cursor position (a rune index).
func (b inputBuffer) Cursor() int { return b.cursor }

// Empty reports whether the buffer holds no text.
func (b inputBuffer) Empty() bool { return b.text == "" }

// SetText replaces the buffer outright, moving the cursor to the end — the
// "wholesale replacement with nothing more specific to place the cursor at"
// case (an Enter-select clearing the buffer to "", the escape-key clear-all
// on the overview dispatch bar).
func (b inputBuffer) SetText(s string) inputBuffer {
	return inputBuffer{text: s, cursor: len([]rune(s))}
}

// SetTextCursor replaces the buffer and places the cursor at an explicit
// rune index, clamped to the new text's length — used by the command menu's
// Tab-complete (command_menu.go's completionCursor), which splices a
// completion in place of the active token and wants the cursor right after
// it, not at the end of any trailing text the splice left in place.
func (b inputBuffer) SetTextCursor(s string, cursor int) inputBuffer {
	return inputBuffer{text: s, cursor: clampCursor(cursor, len([]rune(s)))}
}

// InsertText inserts s at the cursor, advancing the cursor past it. This is
// the general case behind [inputBuffer.InsertRune] — key.Text can in
// principle carry more than one rune (an IME commit), so both route through
// here.
func (b inputBuffer) InsertText(s string) inputBuffer {
	if s == "" {
		return b
	}
	r := b.runes()
	ins := []rune(s)
	cur := clampCursor(b.cursor, len(r))
	out := make([]rune, 0, len(r)+len(ins))
	out = append(out, r[:cur]...)
	out = append(out, ins...)
	out = append(out, r[cur:]...)
	return inputBuffer{text: string(out), cursor: cur + len(ins)}
}

// InsertRune inserts r at the cursor — the common single-typed-character
// case.
func (b inputBuffer) InsertRune(r rune) inputBuffer { return b.InsertText(string(r)) }

// Backspace deletes the rune immediately before the cursor, if any (a no-op
// at position 0).
func (b inputBuffer) Backspace() inputBuffer {
	r := b.runes()
	cur := clampCursor(b.cursor, len(r))
	if cur == 0 {
		return inputBuffer{text: b.text, cursor: cur}
	}
	out := make([]rune, 0, len(r)-1)
	out = append(out, r[:cur-1]...)
	out = append(out, r[cur:]...)
	return inputBuffer{text: string(out), cursor: cur - 1}
}

// DeleteForward deletes the rune at the cursor, if any (a no-op at the
// buffer's end) — Delete/Ctrl+D.
func (b inputBuffer) DeleteForward() inputBuffer {
	r := b.runes()
	cur := clampCursor(b.cursor, len(r))
	if cur >= len(r) {
		return inputBuffer{text: b.text, cursor: cur}
	}
	out := make([]rune, 0, len(r)-1)
	out = append(out, r[:cur]...)
	out = append(out, r[cur+1:]...)
	return inputBuffer{text: string(out), cursor: cur}
}

// MoveLeft/MoveRight move the cursor one rune, clamped at the buffer's ends.
func (b inputBuffer) MoveLeft() inputBuffer {
	r := b.runes()
	cur := clampCursor(b.cursor, len(r))
	if cur > 0 {
		cur--
	}
	return inputBuffer{text: b.text, cursor: cur}
}

func (b inputBuffer) MoveRight() inputBuffer {
	r := b.runes()
	cur := clampCursor(b.cursor, len(r))
	if cur < len(r) {
		cur++
	}
	return inputBuffer{text: b.text, cursor: cur}
}

// MoveHome/MoveEnd jump the cursor to the buffer's start/end — Home/Ctrl+A
// and End/Ctrl+E.
func (b inputBuffer) MoveHome() inputBuffer { return inputBuffer{text: b.text, cursor: 0} }

func (b inputBuffer) MoveEnd() inputBuffer {
	return inputBuffer{text: b.text, cursor: len(b.runes())}
}

// wordLeft returns the rune index a word-wise move/delete lands on moving
// left from cur: skip any run of whitespace immediately to the left first,
// then skip the run of non-whitespace beyond it. This is the standard
// readline/macOS "nearest word start" contract — a cursor sitting in
// trailing whitespace jumps past the whitespace before the word itself,
// rather than stopping at the edge of the space run. Only whitespace is a
// word boundary (not punctuation): "foo.bar" is one word, matching the
// common terminal Ctrl+W/Alt+Backspace convention (bash, zsh, readline)
// rather than an editor's finer-grained punctuation-splitting.
func wordLeft(r []rune, cur int) int {
	i := clampCursor(cur, len(r))
	for i > 0 && unicode.IsSpace(r[i-1]) {
		i--
	}
	for i > 0 && !unicode.IsSpace(r[i-1]) {
		i--
	}
	return i
}

// wordRight is wordLeft's mirror moving right from cur.
func wordRight(r []rune, cur int) int {
	i := clampCursor(cur, len(r))
	for i < len(r) && unicode.IsSpace(r[i]) {
		i++
	}
	for i < len(r) && !unicode.IsSpace(r[i]) {
		i++
	}
	return i
}

// MoveWordLeft/MoveWordRight move the cursor by one word — Alt+Left and
// Alt+Right.
func (b inputBuffer) MoveWordLeft() inputBuffer {
	return inputBuffer{text: b.text, cursor: wordLeft(b.runes(), b.cursor)}
}

func (b inputBuffer) MoveWordRight() inputBuffer {
	return inputBuffer{text: b.text, cursor: wordRight(b.runes(), b.cursor)}
}

// DeleteWordBackward deletes from the word-left boundary through the
// cursor — Alt+Backspace / Ctrl+W.
func (b inputBuffer) DeleteWordBackward() inputBuffer {
	r := b.runes()
	cur := clampCursor(b.cursor, len(r))
	start := wordLeft(r, cur)
	out := make([]rune, 0, len(r)-(cur-start))
	out = append(out, r[:start]...)
	out = append(out, r[cur:]...)
	return inputBuffer{text: string(out), cursor: start}
}

// DeleteToLineStart deletes from the buffer start through the cursor —
// Ctrl+U.
func (b inputBuffer) DeleteToLineStart() inputBuffer {
	r := b.runes()
	cur := clampCursor(b.cursor, len(r))
	return inputBuffer{text: string(r[cur:]), cursor: 0}
}

// DeleteToLineEnd deletes from the cursor through the buffer end — Ctrl+K.
func (b inputBuffer) DeleteToLineEnd() inputBuffer {
	r := b.runes()
	cur := clampCursor(b.cursor, len(r))
	return inputBuffer{text: string(r[:cur]), cursor: cur}
}

// Render splices cursorGlyph into text at the cursor's rune position —
// mid-text when the cursor sits mid-buffer, not always appended at the end
// the way the pre-cursor append-only buffer rendered it.
//
// Each half is passed through [displaySafe] (paste.go) so a control
// character a paste put in the buffer — a newline above all — renders as a
// visible one-cell picture instead of breaking a single-line input out of
// its row. The buffer's own text is untouched: what gets submitted is what
// was pasted. Sanitizing the halves separately keeps the splice exact,
// since displaySafe is one rune per rune either way.
func (b inputBuffer) Render(cursorGlyph string) string {
	r := b.runes()
	cur := clampCursor(b.cursor, len(r))
	return displaySafe(string(r[:cur])) + cursorGlyph + displaySafe(string(r[cur:]))
}
