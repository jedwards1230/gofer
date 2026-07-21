package usercmd

// expand.go is the argument-substitution engine. Every rule it settles is
// documented on [Expand] rather than in a design doc, because the rules are
// the contract a user writes their command file against.

import (
	"strconv"
	"strings"
)

// argumentsToken is the whole-argument-list token, matched as an exact
// literal (see [Expand]'s "inside a word" rule).
const argumentsToken = "$ARGUMENTS"

// Expand substitutes args into a command body and returns the prompt to
// submit. args are the whitespace-separated arguments the dispatcher parsed
// from the submitted line, 1-indexed by the tokens below.
//
// # Tokens
//
//	$ARGUMENTS      every argument, joined with single spaces, in order
//	$N              the Nth argument (1-based); missing → empty
//	${N}            the same, brace-delimited
//	${N:-default}   the Nth argument, or default when it is missing or empty
//	${@:N}          arguments N through the end, joined with single spaces
//	$$              a literal "$"
//
// # Rules this pins down
//
//   - **Tokens are recognized inside a word.** `internal/$1/doc.go` and
//     `fix-$1` both substitute. Requiring a word boundary would make the most
//     common use — pasting an argument into a path — impossible.
//
//   - **`$N` consumes a maximal run of digits**, so `$12` is the twelfth
//     argument, not the first followed by a literal `2`. The alternative
//     silently produces wrong text for anyone with more than nine arguments,
//     which is worse than needing an escape for the rare case. To write the
//     first argument immediately followed by a digit, brace it: `${1}2`.
//     A digit run too large to be an int is a recognized token that expands
//     to empty, the same as any other out-of-range index.
//
//   - **A literal `$` is written `$$`.** Any other `$` that does not begin a
//     recognized token is emitted literally and its following character is
//     rescanned as ordinary text — `$foo`, `${bogus}`, `100$`, and a trailing
//     `$` all survive untouched. So a body only needs `$$` where the very next
//     characters would otherwise parse as a token.
//
//   - **`$ARGUMENTS` is an exact literal**, consistent with the inside-a-word
//     rule: `$ARGUMENTSX` is the argument list followed by `X`. Use `$$` if
//     you meant the literal text.
//
//   - **`${@:0}` means every argument.** Arguments are 1-based here, so N < 1
//     has no meaningful reading other than "from the start"; clamping beats
//     dropping the token or emitting nothing. An N past the end yields the
//     empty string, not an error. `${@:-1}` is not a token at all (only
//     decimal digits are recognized after `@:`) and stays literal.
//
//   - **`${N:-default}`'s default runs to the first `}`** and may be empty
//     (`${1:-}` is exactly `$1`). There is no nesting and no escape inside it;
//     a default that needs a `}` is out of scope for a one-line fallback.
//
//   - **Substitution is single-pass.** The scan walks the body once, left to
//     right, and appends each substituted value straight to the output — a
//     value is NEVER rescanned. An argument that happens to contain
//     `$ARGUMENTS` or `$1` is therefore inserted verbatim, which is the
//     difference between a template and a prompt-injection vector.
func Expand(body string, args []string) string {
	var b strings.Builder
	b.Grow(len(body))
	for i := 0; i < len(body); {
		if body[i] != '$' {
			b.WriteByte(body[i])
			i++
			continue
		}
		val, width, ok := token(body[i:], args)
		if !ok {
			b.WriteByte(body[i])
			i++
			continue
		}
		b.WriteString(val) // appended, never rescanned: the single-pass guarantee
		i += width
	}
	return b.String()
}

// token matches one substitution token at the start of s (which begins with
// '$') and returns its expansion plus how many bytes of s it consumed. ok is
// false when s does not start a recognized token, in which case the caller
// emits the '$' literally.
func token(s string, args []string) (val string, width int, ok bool) {
	if strings.HasPrefix(s, "$$") {
		return "$", 2, true
	}
	if strings.HasPrefix(s, argumentsToken) {
		return strings.Join(args, " "), len(argumentsToken), true
	}
	if n := digitRun(s[1:]); n > 0 {
		return arg(args, atoi(s[1:1+n])), 1 + n, true
	}
	if strings.HasPrefix(s, "${") {
		return braceToken(s, args)
	}
	return "", 0, false
}

// braceToken matches the `${…}` forms. s starts with "${".
func braceToken(s string, args []string) (val string, width int, ok bool) {
	end := strings.IndexByte(s, '}')
	if end < 0 {
		return "", 0, false
	}
	inner, width := s[2:end], end+1

	if rest, found := strings.CutPrefix(inner, "@:"); found {
		n := digitRun(rest)
		if n == 0 || n != len(rest) {
			return "", 0, false // "${@:x}" / "${@:-1}" / "${@:}" are literal text
		}
		return tail(args, atoi(rest)), width, true
	}

	digits := digitRun(inner)
	if digits == 0 {
		return "", 0, false
	}
	idx := atoi(inner[:digits])
	switch rest := inner[digits:]; {
	case rest == "": // ${N}
		return arg(args, idx), width, true
	case strings.HasPrefix(rest, ":-"): // ${N:-default}
		if v := arg(args, idx); v != "" {
			return v, width, true
		}
		return rest[len(":-"):], width, true
	default:
		return "", 0, false
	}
}

// digitRun returns the length of the leading run of ASCII decimal digits in s
// (0 when it does not start with one).
func digitRun(s string) int {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return i
}

// atoi parses a digit run, mapping an overflowing one to an index no argument
// list can have. Out-of-range is already a defined, non-erroring outcome
// (empty), so an unrepresentable index takes the same path rather than
// inventing a second failure mode.
func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	return n
}

// arg returns the 1-based idx'th argument, or "" when there is none.
func arg(args []string, idx int) string {
	if idx < 1 || idx > len(args) {
		return ""
	}
	return args[idx-1]
}

// tail joins the 1-based idx'th argument through the last one. idx < 1 clamps
// to the first argument (see [Expand]'s `${@:0}` rule); idx past the end is
// the empty string.
func tail(args []string, idx int) string {
	if idx < 1 {
		idx = 1
	}
	if idx > len(args) {
		return ""
	}
	return strings.Join(args[idx-1:], " ")
}
