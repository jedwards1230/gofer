package supervisor

import (
	"strings"
	"testing"
)

// TestSnippet covers the first-prompt title heuristic: first non-empty line,
// internal-whitespace collapse, trim, and word-boundary truncation with an
// ellipsis when over budget. It is an internal test because snippet is
// unexported (the app's title-generation business logic, not exported surface).
func TestSnippet(t *testing.T) {
	// maxTitle mirrors snippet's own cap; the truncation cases derive their
	// expectations from it so a future retune of the constant updates in one
	// place.
	const maxTitle = 60

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"whitespace only", "   \n\t  \n ", ""},
		{"simple", "hello", "hello"},
		{"trims ends", "  hello world  ", "hello world"},
		{"first non-empty line", "\n\n  find the bug\nsecond line here", "find the bug"},
		{"collapses internal whitespace", "fix\tthe   flaky    build", "fix the flaky build"},
		{"collapses across the whole first line", "investigate   the\t\tflaky build", "investigate the flaky build"},
		{
			"truncates on a word boundary with an ellipsis",
			"refactor the supervisor pump so the queue drains predictably under load and pressure",
			"refactor the supervisor pump so the queue drains predictably" + "…",
		},
		{
			"hard cut when the first word exceeds the budget",
			strings.Repeat("x", 80),
			strings.Repeat("x", maxTitle) + "…",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := snippet(tt.in)
			if got != tt.want {
				t.Errorf("snippet(%q) = %q, want %q", tt.in, got, tt.want)
			}
			// Invariants that must hold for every derived title: single-line and
			// never wider than the budget plus the one-rune ellipsis.
			if strings.ContainsRune(got, '\n') {
				t.Errorf("snippet(%q) = %q contains a newline", tt.in, got)
			}
			if r := []rune(got); len(r) > maxTitle+1 {
				t.Errorf("snippet(%q) = %q is %d runes, over the %d+1 budget", tt.in, got, len(r), maxTitle)
			}
		})
	}
}

// TestSnippetTruncationBacksOffToWordBoundary asserts the over-budget case
// never severs a word mid-token: the last rune before the ellipsis is a word
// character, not a partial split, and the result is a prefix of the collapsed
// input.
func TestSnippetTruncationBacksOffToWordBoundary(t *testing.T) {
	in := "one two three four five six seven eight nine ten eleven twelve thirteen"
	got := snippet(in)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("snippet(%q) = %q, want a trailing ellipsis", in, got)
	}
	body := strings.TrimSuffix(got, "…")
	if !strings.HasPrefix(in, body) {
		t.Errorf("snippet body %q is not a prefix of the input %q", body, in)
	}
	if strings.HasSuffix(body, " ") {
		t.Errorf("snippet body %q ends on whitespace, want a word boundary", body)
	}
	// The input has no run past the cut that would merge into the last kept
	// word: body must end exactly where a word ends in the source.
	if next := in[len(body)]; next != ' ' {
		t.Errorf("snippet cut at a non-boundary: input rune after body is %q, want a space", string(next))
	}
}
