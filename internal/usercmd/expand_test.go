package usercmd_test

// expand_test.go pins every rule [usercmd.Expand]'s doc comment claims. The
// edge cases are the point: an undocumented, untested answer to "what does
// ${@:0} do?" is a bug report waiting to be filed, and the single-pass
// guarantee is the difference between a template and an injection vector.

import (
	"testing"

	"github.com/jedwards1230/gofer/internal/usercmd"
)

func TestExpand(t *testing.T) {
	tests := []struct {
		name string
		body string
		args []string
		want string
	}{
		// $ARGUMENTS
		{"arguments all", "review $ARGUMENTS please", []string{"a", "b"}, "review a b please"},
		{"arguments none", "review $ARGUMENTS please", nil, "review  please"},
		{"arguments preserves order", "$ARGUMENTS", []string{"c", "a", "b"}, "c a b"},
		{"arguments is an exact literal, trailing text stays", "$ARGUMENTSX", []string{"a"}, "aX"},
		{"arguments lowercase is not a token", "$arguments", []string{"a"}, "$arguments"},

		// $N
		{"positional", "fix $1 in $2", []string{"bug", "file.go"}, "fix bug in file.go"},
		{"positional missing is empty", "fix $1 in $2", []string{"bug"}, "fix bug in "},
		{"positional inside a word", "internal/$1/doc.go", []string{"tui"}, "internal/tui/doc.go"},
		{"positional past nine", "$10", []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "ten"}, "ten"},
		{"digit run is maximal, not $1 then a literal 2", "$12", []string{"one", "two"}, ""},
		{"brace escapes the maximal digit run", "${1}2", []string{"one"}, "one2"},
		{"unrepresentable index expands to empty", "$99999999999999999999", []string{"a"}, ""},
		{"zero index has no argument", "$0", []string{"a"}, ""},

		// ${N:-default}
		{"default used when missing", "${1:-main}", nil, "main"},
		{"default used when empty", "${1:-main}", []string{""}, "main"},
		{"default unused when present", "${1:-main}", []string{"dev"}, "dev"},
		{"empty default is just $1", "[${1:-}]", nil, "[]"},
		{"default may contain spaces", "${1:-the whole branch}", nil, "the whole branch"},
		{"default runs to the first brace", "${1:-a}b}", nil, "ab}"},

		// ${@:N}
		{"tail from two", "${@:2}", []string{"a", "b", "c"}, "b c"},
		{"tail from one is everything", "${@:1}", []string{"a", "b"}, "a b"},
		{"tail from zero clamps to everything", "${@:0}", []string{"a", "b"}, "a b"},
		{"tail past the end is empty", "${@:9}", []string{"a"}, ""},
		// Regression: an overflowing digit run used to resolve to -1, which
		// tail clamped to "from the first argument" — so an absurd index
		// expanded to the ENTIRE list where the doc promises empty.
		{"tail with an unrepresentable index is empty", "${@:99999999999999999999}", []string{"a", "b", "c"}, ""},
		{"braced positional with an unrepresentable index is empty", "${99999999999999999999}", []string{"a"}, ""},
		{"unrepresentable index falls back to its default", "${99999999999999999999:-none}", []string{"a"}, "none"},
		{"tail with no args is empty", "${@:1}", nil, ""},
		{"negative tail is not a token", "${@:-1}", []string{"a"}, "${@:-1}"},
		{"non-numeric tail is not a token", "${@:x}", []string{"a"}, "${@:x}"},

		// literals and non-tokens
		{"double dollar is a literal dollar", "cost: $$5", nil, "cost: $5"},
		{"double dollar shields a token", "$$1", []string{"a"}, "$1"},
		{"unknown word after dollar stays literal", "$foo", []string{"a"}, "$foo"},
		{"unknown brace stays literal", "${bogus}", []string{"a"}, "${bogus}"},
		{"unterminated brace stays literal", "${1", []string{"a"}, "${1"},
		{"trailing dollar stays literal", "100$", nil, "100$"},
		{"bare dollar between words", "a $ b", nil, "a $ b"},
		{"no tokens at all", "plain body\nsecond line", []string{"a"}, "plain body\nsecond line"},
		{"empty body", "", []string{"a"}, ""},

		// multi-token bodies
		{"several tokens in one line", "$1: ${2:-none} / $ARGUMENTS", []string{"x"}, "x: none / x"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := usercmd.Expand(tt.body, tt.args); got != tt.want {
				t.Errorf("Expand(%q, %q) = %q, want %q", tt.body, tt.args, got, tt.want)
			}
		})
	}
}

// TestExpandIsSinglePass is the injection guard: a substituted VALUE must
// never be rescanned for tokens. If Expand ever loops to a fixed point, an
// argument a user (or a tool result) supplies could name another argument, or
// splice the whole argument list into a body that asked for one word.
func TestExpandIsSinglePass(t *testing.T) {
	tests := []struct {
		name string
		body string
		args []string
		want string
	}{
		{
			name: "an argument naming $ARGUMENTS is inserted literally",
			body: "$1",
			args: []string{"$ARGUMENTS", "secret"},
			want: "$ARGUMENTS",
		},
		{
			name: "an argument naming another positional is inserted literally",
			body: "$1",
			args: []string{"$2", "secret"},
			want: "$2",
		},
		{
			name: "$ARGUMENTS carrying a token does not re-expand",
			body: "$ARGUMENTS",
			args: []string{"${1:-x}"},
			want: "${1:-x}",
		},
		{
			name: "an argument's $$ is not unescaped a second time",
			body: "$1",
			args: []string{"$$"},
			want: "$$",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := usercmd.Expand(tt.body, tt.args)
			if got != tt.want {
				t.Errorf("Expand(%q, %q) = %q, want %q — a substituted value was rescanned, "+
					"so an argument can inject tokens into the prompt", tt.body, tt.args, got, tt.want)
			}
		})
	}
}
