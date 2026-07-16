package tui

// command_token_test.go hammers [commandToken] — the pure (buffer, cursor)
// -> active command token parse helper the autocomplete popup's trigger rule
// is built on (command_menu.go). White-box (package tui) since commandToken
// is unexported.

import "testing"

func TestCommandToken(t *testing.T) {
	tests := []struct {
		name        string
		buf         string
		cursor      int
		wantPartial string
		wantStart   int
		wantOK      bool
	}{
		{
			name:        "start of line, bare slash",
			buf:         "/",
			cursor:      1,
			wantPartial: "",
			wantStart:   0,
			wantOK:      true,
		},
		{
			name:        "start of line, partial name",
			buf:         "/sta",
			cursor:      4,
			wantPartial: "sta",
			wantStart:   0,
			wantOK:      true,
		},
		{
			name:        "after a space",
			buf:         "hello /st",
			cursor:      9,
			wantPartial: "st",
			wantStart:   6,
			wantOK:      true,
		},
		{
			name:   "after a backtick — literal, no menu",
			buf:    "`/x",
			cursor: 3,
			wantOK: false,
		},
		{
			name:   "mid-word — literal, no menu",
			buf:    "foo/bar",
			cursor: 7,
			wantOK: false,
		},
		{
			name:   "trailing space closes the menu",
			buf:    "/config ",
			cursor: 8,
			wantOK: false,
		},
		{
			name:        "cursor mid-token uses only the prefix up to the cursor",
			buf:         "/config",
			cursor:      4,
			wantPartial: "con",
			wantStart:   0,
			wantOK:      true,
		},
		{
			name:   "empty buffer",
			buf:    "",
			cursor: 0,
			wantOK: false,
		},
		{
			name:   "cursor at buffer start with text after it",
			buf:    "/status",
			cursor: 0,
			wantOK: false,
		},
		{
			name:        "leading whitespace before the slash",
			buf:         " /x",
			cursor:      3,
			wantPartial: "x",
			wantStart:   1,
			wantOK:      true,
		},
		{
			name:   "slash preceded by a non-space, non-backtick rune",
			buf:    "a/b",
			cursor: 3,
			wantOK: false,
		},
		{
			name:   "cursor beyond buffer length clamps to buffer end",
			buf:    "/config",
			cursor: 99,
			// clamped to len(buf)=7, same as the "start of line, partial name"
			// case but for the full word.
			wantPartial: "config",
			wantStart:   0,
			wantOK:      true,
		},
		{
			name:   "negative cursor clamps to 0",
			buf:    "/config",
			cursor: -1,
			wantOK: false,
		},
		{
			name:        "second slash-token after a completed first one",
			buf:         "/model /st",
			cursor:      10,
			wantPartial: "st",
			wantStart:   7,
			wantOK:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotPartial, gotStart, gotOK := commandToken(tt.buf, tt.cursor)
			if gotOK != tt.wantOK {
				t.Fatalf("commandToken(%q, %d) ok = %v, want %v", tt.buf, tt.cursor, gotOK, tt.wantOK)
			}
			if !gotOK {
				return
			}
			if gotPartial != tt.wantPartial {
				t.Errorf("commandToken(%q, %d) partial = %q, want %q", tt.buf, tt.cursor, gotPartial, tt.wantPartial)
			}
			if gotStart != tt.wantStart {
				t.Errorf("commandToken(%q, %d) start = %d, want %d", tt.buf, tt.cursor, gotStart, tt.wantStart)
			}
		})
	}
}
