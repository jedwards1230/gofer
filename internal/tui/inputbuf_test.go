package tui

// inputbuf_test.go hard-tests every [inputBuffer] operation in isolation:
// insertion mid-string, word boundaries (including leading/trailing spaces
// and punctuation), deletion at both edges, and cursor clamping. The
// App-level keymap wiring (applyInputKey, which keys map to which of these
// ops) is covered separately in app_internal_test.go/input_keymap_test.go.

import "testing"

func TestInputBufferInsertTextMidString(t *testing.T) {
	b := inputBuffer{}
	for _, r := range "helloworld" {
		b = b.InsertRune(r)
	}
	if got := b.String(); got != "helloworld" {
		t.Fatalf("after typing = %q, want %q", got, "helloworld")
	}
	// Move cursor to just after "hello" (index 5) and insert " " + "there ".
	b = inputBuffer{text: b.text, cursor: 5}
	b = b.InsertText(" there")
	if got := b.String(); got != "hello thereworld" {
		t.Errorf("InsertText mid-string = %q, want %q", got, "hello thereworld")
	}
	if got := b.Cursor(); got != 11 {
		t.Errorf("cursor after mid-string insert = %d, want 11 (right after the inserted text)", got)
	}
}

func TestInputBufferInsertRuneAdvancesCursor(t *testing.T) {
	b := inputBuffer{}
	b = b.InsertRune('a')
	b = b.InsertRune('b')
	if got := b.String(); got != "ab" {
		t.Fatalf("String() = %q, want %q", got, "ab")
	}
	if got := b.Cursor(); got != 2 {
		t.Errorf("Cursor() = %d, want 2 (end of buffer after two inserts)", got)
	}
}

func TestInputBufferInsertUTF8DoesNotSplitRunes(t *testing.T) {
	b := inputBuffer{text: "café", cursor: 3} // cursor between "caf" and "é"
	b = b.InsertRune('!')
	if got := b.String(); got != "caf!é" {
		t.Errorf("InsertRune with a multi-byte rune after the cursor = %q, want %q", got, "caf!é")
	}
}

func TestInputBufferBackspaceAtEdges(t *testing.T) {
	// At position 0: no-op.
	b := inputBuffer{text: "abc", cursor: 0}
	got := b.Backspace()
	if got.String() != "abc" || got.Cursor() != 0 {
		t.Errorf("Backspace at cursor 0 = %+v, want unchanged", got)
	}

	// Mid-buffer: removes the rune before the cursor.
	b = inputBuffer{text: "abc", cursor: 2}
	got = b.Backspace()
	if got.String() != "ac" || got.Cursor() != 1 {
		t.Errorf("Backspace mid-buffer = %q cursor=%d, want %q cursor=1", got.String(), got.Cursor(), "ac")
	}

	// At the end: removes the last rune (the pre-cursor append-only behavior,
	// preserved as the degenerate case).
	b = inputBuffer{text: "abc", cursor: 3}
	got = b.Backspace()
	if got.String() != "ab" || got.Cursor() != 2 {
		t.Errorf("Backspace at end = %q cursor=%d, want %q cursor=2", got.String(), got.Cursor(), "ab")
	}
}

func TestInputBufferDeleteForwardAtEdges(t *testing.T) {
	// At the end: no-op.
	b := inputBuffer{text: "abc", cursor: 3}
	got := b.DeleteForward()
	if got.String() != "abc" || got.Cursor() != 3 {
		t.Errorf("DeleteForward at end = %+v, want unchanged", got)
	}

	// Mid-buffer: removes the rune at the cursor, cursor stays put.
	b = inputBuffer{text: "abc", cursor: 1}
	got = b.DeleteForward()
	if got.String() != "ac" || got.Cursor() != 1 {
		t.Errorf("DeleteForward mid-buffer = %q cursor=%d, want %q cursor=1", got.String(), got.Cursor(), "ac")
	}
}

func TestInputBufferMoveLeftRightClampAtEnds(t *testing.T) {
	b := inputBuffer{text: "ab", cursor: 0}
	if got := b.MoveLeft().Cursor(); got != 0 {
		t.Errorf("MoveLeft at cursor 0 = %d, want 0 (clamped)", got)
	}
	if got := b.MoveRight().Cursor(); got != 1 {
		t.Errorf("MoveRight at cursor 0 = %d, want 1", got)
	}

	b = inputBuffer{text: "ab", cursor: 2}
	if got := b.MoveRight().Cursor(); got != 2 {
		t.Errorf("MoveRight at the end = %d, want 2 (clamped)", got)
	}
	if got := b.MoveLeft().Cursor(); got != 1 {
		t.Errorf("MoveLeft at the end = %d, want 1", got)
	}
}

func TestInputBufferMoveHomeEnd(t *testing.T) {
	b := inputBuffer{text: "hello world", cursor: 5}
	if got := b.MoveHome().Cursor(); got != 0 {
		t.Errorf("MoveHome() = %d, want 0", got)
	}
	if got := b.MoveEnd().Cursor(); got != 11 {
		t.Errorf("MoveEnd() = %d, want 11", got)
	}
}

// TestWordLeftRightBoundaries covers the word-boundary contract directly:
// only whitespace is a boundary (punctuation stays part of the word), and
// leading/trailing whitespace runs are skipped before landing on the word
// itself.
func TestWordLeftRightBoundaries(t *testing.T) {
	cases := []struct {
		name string
		text string
		cur  int
		want int
	}{
		{"left from end of last word", "foo.bar baz", 11, 8},
		{"left is punctuation-inclusive (one word, not two)", "foo.bar baz", 8, 0},
		{"left skips trailing whitespace before the word", "foo   ", 6, 0},
		{"left from buffer start stays put", "foo", 0, 0},
		{"left from end of a word preceded by leading whitespace stops at the word start", "  foo", 5, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := wordLeft([]rune(tc.text), tc.cur); got != tc.want {
				t.Errorf("wordLeft(%q, %d) = %d, want %d", tc.text, tc.cur, got, tc.want)
			}
		})
	}

	rightCases := []struct {
		name string
		text string
		cur  int
		want int
	}{
		{"right to end of punctuation-inclusive word", "foo.bar baz", 0, 7},
		{"right skips leading whitespace before the word", "   foo", 0, 6},
		{"right from buffer end stays put", "foo", 3, 3},
		{"right across trailing whitespace inside text", "foo   bar", 3, 9},
	}
	for _, tc := range rightCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := wordRight([]rune(tc.text), tc.cur); got != tc.want {
				t.Errorf("wordRight(%q, %d) = %d, want %d", tc.text, tc.cur, got, tc.want)
			}
		})
	}
}

func TestInputBufferMoveWordLeftRight(t *testing.T) {
	b := inputBuffer{text: "foo bar baz", cursor: 11}
	b = b.MoveWordLeft()
	if got := b.Cursor(); got != 8 {
		t.Fatalf("first MoveWordLeft = %d, want 8 (start of \"baz\")", got)
	}
	b = b.MoveWordLeft()
	if got := b.Cursor(); got != 4 {
		t.Fatalf("second MoveWordLeft = %d, want 4 (start of \"bar\")", got)
	}
	b = b.MoveWordRight()
	if got := b.Cursor(); got != 7 {
		t.Fatalf("MoveWordRight = %d, want 7 (end of \"bar\")", got)
	}
}

func TestInputBufferDeleteWordBackward(t *testing.T) {
	// Ordinary case: deletes the word immediately before the cursor.
	b := inputBuffer{text: "foo bar baz", cursor: 11}
	got := b.DeleteWordBackward()
	if got.String() != "foo bar " || got.Cursor() != 8 {
		t.Errorf("DeleteWordBackward = %q cursor=%d, want %q cursor=8", got.String(), got.Cursor(), "foo bar ")
	}

	// Trailing-whitespace case: cursor sits after trailing spaces — the
	// whitespace run is consumed along with the word before it, not treated
	// as its own deletable unit.
	b = inputBuffer{text: "foo   ", cursor: 6}
	got = b.DeleteWordBackward()
	if got.String() != "" || got.Cursor() != 0 {
		t.Errorf("DeleteWordBackward with trailing whitespace = %q cursor=%d, want empty/0", got.String(), got.Cursor())
	}

	// Cursor at 0: no-op.
	b = inputBuffer{text: "foo", cursor: 0}
	got = b.DeleteWordBackward()
	if got.String() != "foo" || got.Cursor() != 0 {
		t.Errorf("DeleteWordBackward at cursor 0 = %+v, want unchanged", got)
	}

	// Punctuation stays part of the word (not its own boundary).
	b = inputBuffer{text: "foo.bar", cursor: 7}
	got = b.DeleteWordBackward()
	if got.String() != "" || got.Cursor() != 0 {
		t.Errorf("DeleteWordBackward across punctuation = %q cursor=%d, want empty/0 (one word)", got.String(), got.Cursor())
	}
}

func TestInputBufferDeleteToLineStartEnd(t *testing.T) {
	b := inputBuffer{text: "hello world", cursor: 5}
	got := b.DeleteToLineStart()
	if got.String() != " world" || got.Cursor() != 0 {
		t.Errorf("DeleteToLineStart = %q cursor=%d, want %q cursor=0", got.String(), got.Cursor(), " world")
	}

	b = inputBuffer{text: "hello world", cursor: 5}
	got = b.DeleteToLineEnd()
	if got.String() != "hello" || got.Cursor() != 5 {
		t.Errorf("DeleteToLineEnd = %q cursor=%d, want %q cursor=5", got.String(), got.Cursor(), "hello")
	}
}

func TestInputBufferSetTextCursorClamps(t *testing.T) {
	b := inputBuffer{}.SetTextCursor("hi", 100)
	if got := b.Cursor(); got != 2 {
		t.Errorf("SetTextCursor with an oversized cursor = %d, want clamped to 2", got)
	}
	b = inputBuffer{}.SetTextCursor("hi", -5)
	if got := b.Cursor(); got != 0 {
		t.Errorf("SetTextCursor with a negative cursor = %d, want clamped to 0", got)
	}
}

func TestInputBufferSetTextMovesCursorToEnd(t *testing.T) {
	b := inputBuffer{text: "old", cursor: 1}.SetText("new text")
	if got := b.Cursor(); got != len([]rune("new text")) {
		t.Errorf("SetText cursor = %d, want end of the new text (%d)", got, len([]rune("new text")))
	}
}

func TestInputBufferRenderSplicesCursorMidText(t *testing.T) {
	b := inputBuffer{text: "hello", cursor: 2}
	if got := b.Render("|"); got != "he|llo" {
		t.Errorf("Render with a mid-text cursor = %q, want %q", got, "he|llo")
	}

	b = inputBuffer{text: "hello", cursor: 5}
	if got := b.Render("|"); got != "hello|" {
		t.Errorf("Render with the cursor at the end = %q, want %q", got, "hello|")
	}

	b = inputBuffer{}
	if got := b.Render("|"); got != "|" {
		t.Errorf("Render of an empty buffer = %q, want %q", got, "|")
	}
}

func TestInputBufferEmpty(t *testing.T) {
	if !(inputBuffer{}).Empty() {
		t.Error("zero-value inputBuffer.Empty() = false, want true")
	}
	if (inputBuffer{text: "x"}).Empty() {
		t.Error("non-empty buffer.Empty() = true, want false")
	}
}
