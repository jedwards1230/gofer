package tui

// paste_internal_test.go unit-tests paste.go's pure helpers in isolation:
// line-ending normalization, the byte cap's rune-boundary cut, and the
// render-only control-character substitution (including the one-cell,
// one-rune invariants inputBuffer.Render's cursor splice depends on). The
// App-level wiring is covered black-box in paste_test.go.

import (
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestNormalizeNewlines(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"crlf", "a\r\nb", "a\nb"},
		{"lone cr", "a\rb", "a\nb"},
		{"mixed", "a\r\nb\rc\nd", "a\nb\nc\nd"},
		{"already lf", "a\nb", "a\nb"},
		{"none", "abc", "abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeNewlines(tc.in); got != tc.want {
				t.Fatalf("normalizeNewlines(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestClipPaste(t *testing.T) {
	for _, tc := range []struct {
		name        string
		in          string
		limit       int
		want        string
		wantClipped bool
	}{
		{"under the cap", "abc", 8, "abc", false},
		{"exactly at the cap", "abcd", 4, "abcd", false},
		{"over the cap", "abcdef", 4, "abcd", true},
		{"no cap", strings.Repeat("x", 1000), 0, strings.Repeat("x", 1000), false},
		{"negative cap treated as none", "abcdef", -1, "abcdef", false},
		// "é" is two bytes: a cap of 3 must cut BEFORE it rather than
		// leaving half a rune in the buffer.
		{"cuts on a rune boundary", "abécd", 3, "ab", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, clipped := clipPaste(tc.in, tc.limit)
			if got != tc.want || clipped != tc.wantClipped {
				t.Fatalf("clipPaste(%q, %d) = (%q, %v), want (%q, %v)", tc.in, tc.limit, got, clipped, tc.want, tc.wantClipped)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("clipPaste(%q, %d) returned invalid UTF-8 %q", tc.in, tc.limit, got)
			}
		})
	}
}

func TestDisplaySafeSubstitutesControlCharacters(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"newline", "a\nb", "a␊b"},
		{"tab", "a\tb", "a␉b"},
		{"carriage return", "a\rb", "a␍b"},
		{"escape", "a\x1bb", "a␛b"},
		{"nul", "a\x00b", "a␀b"},
		{"del", "a\x7fb", "a␡b"},
		{"plain text untouched", "hello world", "hello world"},
		{"non-ascii untouched", "héllo ✓", "héllo ✓"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := displaySafe(tc.in); got != tc.want {
				t.Fatalf("displaySafe(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestDisplaySafePreservesRuneCountAndWidth pins the two invariants
// inputBuffer.Render leans on: the substitution is one rune per rune (so a
// cursor rune index still splits the rendered text where it splits the
// buffer) and one cell per cell (so the line's measured display width — what
// truncate clips against — doesn't move either).
func TestDisplaySafePreservesRuneCountAndWidth(t *testing.T) {
	const in = "a\nb\tc\x1bd\x7fe"
	got := displaySafe(in)

	if want := len([]rune(in)); len([]rune(got)) != want {
		t.Fatalf("displaySafe rune count = %d, want %d (%q -> %q)", len([]rune(got)), want, in, got)
	}
	if w := ansi.StringWidth(got); w != len([]rune(in)) {
		t.Fatalf("displaySafe display width = %d, want %d (%q)", w, len([]rune(in)), got)
	}
	if strings.ContainsFunc(got, isC0Control) {
		t.Fatalf("displaySafe left a control character in %q", got)
	}
}

// TestInputBufferRenderSanitizesAroundTheCursor pins that the cursor splice
// survives the substitution on both sides of the cursor.
func TestInputBufferRenderSanitizesAroundTheCursor(t *testing.T) {
	b := inputBuffer{}.InsertText("one\ntwo")
	b = b.MoveHome().MoveRight().MoveRight().MoveRight().MoveRight()

	if got, want := b.Render("▏"), "one␊▏two"; got != want {
		t.Fatalf("Render = %q, want %q", got, want)
	}
	if got, want := b.String(), "one\ntwo"; got != want {
		t.Fatalf("buffer text = %q, want the newline preserved (%q)", got, want)
	}
}

// TestPasteIgnoredWhilePendingApproval covers handlePaste's other overlay
// guard, the one paste_test.go can't reach from tui_test: an attach screen
// with a pending permission prompt is a modal answer gate where a typed rune
// does nothing either, so a paste must not slip into the input behind it.
// Lives here because seeding the pending approval needs the unexported
// sessEventMsg (see app_internal_test.go's requestApproval).
func TestPasteIgnoredWhilePendingApproval(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDialogTest(t, sup)
	a = requestApproval(t, a, "perm-1")
	if !a.sess.HasPendingApproval() {
		t.Fatal("expected a pending approval to set up the guard")
	}

	mdl, _ := a.Update(tea.PasteMsg{Content: "leaked"})
	got := mdl.(App)
	if txt := got.sess.input.String(); txt != "" {
		t.Fatalf("attach input = %q, want the paste ignored behind the approval prompt", txt)
	}
}
