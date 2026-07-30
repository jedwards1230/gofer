package prompt

import "testing"

// TestAppendHint covers the join discipline: empty hint is a no-op, empty
// text with a hint returns the hint alone, and the ordinary case joins on a
// blank line with both sides trimmed.
func TestAppendHint(t *testing.T) {
	cases := []struct {
		name       string
		text, hint string
		want       string
	}{
		{"empty hint is a no-op", "system prompt", "", "system prompt"},
		{"empty hint on empty text stays empty", "", "", ""},
		{"empty text returns hint alone", "", "tool index: …", "tool index: …"},
		{"ordinary join", "system prompt", "tool index: …", "system prompt\n\ntool index: …"},
		{"hint is trimmed", "system prompt", "  tool index: …  \n", "system prompt\n\ntool index: …"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := AppendHint(c.text, c.hint); got != c.want {
				t.Errorf("AppendHint(%q, %q) = %q, want %q", c.text, c.hint, got, c.want)
			}
		})
	}
}
