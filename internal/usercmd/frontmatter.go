package usercmd

// frontmatter.go is the deliberately tiny `---`-delimited header parser. It
// recognizes exactly two keys and is not a YAML implementation: two keys do
// not justify a dependency, and a strict `key: value` reader makes the
// failure mode ("that line isn't a key/value pair") obvious to whoever wrote
// the file. Unknown keys are ignored so adding a third key later is not a
// breaking change for files written today.

import (
	"errors"
	"fmt"
	"strings"
)

// fmDelim is the frontmatter fence. Both the opening and closing line must be
// exactly this, ignoring trailing spaces/tabs and a CRLF carriage return.
const fmDelim = "---"

// frontmatter holds the recognized keys. The zero value is "no frontmatter",
// which is what every degraded path returns.
type frontmatter struct {
	description  string
	argumentHint string
}

// parseFrontmatter splits src into its recognized frontmatter fields and the
// body below the closing fence.
//
// A non-nil error means the header was malformed. The parse still SUCCEEDS in
// that case, degrading to "this file has no frontmatter": meta is the zero
// value and body is the whole file. Losing a whole command because its header
// had a typo would be a worse outcome than rendering four extra lines into the
// prompt, and the returned error is what the caller reports as a warning.
func parseFrontmatter(src string) (meta frontmatter, body string, err error) {
	// A UTF-8 BOM ahead of the fence is common from editors on Windows and
	// would otherwise make the first line BOM+"---" — not a fence.
	src = strings.TrimPrefix(src, "\ufeff")

	first, rest, found := strings.Cut(src, "\n")
	if fenceLine(first) != fmDelim {
		return frontmatter{}, src, nil // no frontmatter at all: the common case
	}
	if !found {
		return frontmatter{}, src, errors.New("frontmatter opens with --- but the file has no further lines")
	}

	lines := strings.Split(rest, "\n")
	for i, ln := range lines {
		if fenceLine(ln) != fmDelim {
			continue
		}
		meta, err := parseFrontmatterKeys(lines[:i])
		if err != nil {
			return frontmatter{}, src, err
		}
		return meta, strings.Join(lines[i+1:], "\n"), nil
	}
	return frontmatter{}, src, errors.New("frontmatter opened with --- but was never closed")
}

// fenceLine normalizes one line for the fence comparison: CRLF's carriage
// return and trailing horizontal whitespace are not meaningful in a fence.
func fenceLine(s string) string { return strings.TrimRight(s, " \t\r") }

// parseFrontmatterKeys reads the lines between the fences. Blank lines and
// `#` comments are skipped; a recognized key sets its field; an unrecognized
// key is ignored; anything that is not `key: value` is an error, which the
// caller turns into "no frontmatter" plus a warning.
func parseFrontmatterKeys(lines []string) (frontmatter, error) {
	var meta frontmatter
	for i, raw := range lines {
		ln := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		key, val, ok := strings.Cut(ln, ":")
		if !ok {
			return frontmatter{}, fmt.Errorf("frontmatter line %d (%q) is not a `key: value` pair", i+1, ln)
		}
		switch strings.ToLower(strings.TrimSpace(key)) {
		case "description":
			meta.description = unquote(strings.TrimSpace(val))
		case "argument-hint":
			meta.argumentHint = unquote(strings.TrimSpace(val))
		default:
			// Forward compatibility: a key this version doesn't know is not an
			// error, so a file using a future key still loads here.
		}
	}
	return meta, nil
}

// unquote strips one layer of matching single or double quotes, so both
// `description: Review the diff` and `description: "Review the diff"` mean the
// same thing. It does no escape processing — there is nothing inside a
// one-line summary that needs escaping.
func unquote(s string) string {
	if len(s) < 2 {
		return s
	}
	q := s[0]
	if (q == '"' || q == '\'') && s[len(s)-1] == q {
		return s[1 : len(s)-1]
	}
	return s
}
