package tui_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// allowedInlineCopy is the complete set of prose string literals permitted to
// stay inline in internal/tui, each with the reason it is not copy.
//
// Every entry here is MODEL-FACING: text the agent reads, not text the operator
// reads. Moving one into internal/uicopy would put it in the translatable set,
// and the day a second locale exists, translating it would silently change how
// gofer behaves rather than how it reads. See the audience split in
// internal/uicopy's package doc, and the comment above the group in shell.go.
//
// Adding to this map is how the operator/model boundary gets redrawn, so it
// should be a deliberate, reviewed act. If you are adding an entry because a
// string is genuinely read by the model, say so in its reason. If you are
// adding one to silence this test, the string belongs in internal/uicopy.
var allowedInlineCopy = map[string]string{
	"[The shell command(s) below were run by the USER in their terminal, not by you — output is shown for your reference; do not re-run them.]": "shell.go: fold framing that stops the agent re-running the user's commands",
	"[no output]":          "shell.go: tells the model a command completed silently rather than still needing to be run",
	"[exit %d]\n":          "shell.go: written into the model's context by contextBlock",
	"[output truncated]\n": "shell.go: written into the model's context by contextBlock",
	"timed out after ":     "shell.go: dual-audience — the same shell run note is put in the model's context AND rendered to the operator",
}

// TestNoInlineOperatorCopy fails when operator-facing copy is written inline in
// internal/tui instead of in internal/uicopy.
//
// The point of the catalog is that a phrase can be changed in one place. That
// property does not survive on convention: it decays the first time someone in a
// hurry types a sentence at the call site, and nothing else in CI would notice —
// the goldens happily pin an inline string. So the rule is enforced here, inside
// go test, which is a merge gate. (The VHS lanes render this package's output
// but are advisory and never block.)
//
// A literal counts as prose when it has a space and three consecutive ASCII
// letters. That deliberately ignores glyphs, separators, column padding, key
// names like "ctrl+c", config keys, and format skeletons like "%s · %s" — none
// of which a translator would touch. It also means single words ("Idle",
// "Status") slip through: a bare word is indistinguishable from a map key or a
// wire value by syntax alone, and a check that guessed would either cry wolf or
// need an allowlist nobody maintains. This test is therefore a floor, not a
// ceiling — single-word labels still belong in the catalog, they just are not
// mechanically enforced.
func TestNoInlineOperatorCopy(t *testing.T) {
	for _, lit := range prosein(t, ".") {
		if _, ok := allowedInlineCopy[lit.val]; ok {
			continue
		}
		t.Errorf("%s:%d: operator-facing copy written inline: %s\n"+
			"\tMove it to internal/uicopy and reference it from here.\n"+
			"\tIf the model reads this string rather than the operator, add it to\n"+
			"\tallowedInlineCopy with the reason instead.",
			lit.file, lit.line, strconv.Quote(lit.val))
	}
}

// TestAllowedInlineCopyIsAllUsed keeps allowedInlineCopy honest. An entry that
// no longer matches any literal is a stale exemption, and a stale exemption is
// how an allowlist quietly grows into a second home for copy.
func TestAllowedInlineCopyIsAllUsed(t *testing.T) {
	found := map[string]bool{}
	for _, lit := range prosein(t, ".") {
		found[lit.val] = true
	}
	for lit := range allowedInlineCopy {
		if !found[lit] {
			t.Errorf("allowedInlineCopy exempts a literal that no longer appears in internal/tui: %s\n"+
				"\tDelete the entry.", strconv.Quote(lit))
		}
	}
}

// TestCatalogEntriesAreNotEmpty fails on an empty string constant in
// internal/uicopy.
//
// Constants already make a mistyped entry a compile error, which is why the
// catalog is constants and not a keyed map. The one failure a constant cannot
// catch is an entry that exists but holds nothing: it compiles, renders a blank
// line, and reads as a layout bug rather than a missing message.
func TestCatalogEntriesAreNotEmpty(t *testing.T) {
	fset := token.NewFileSet()
	for _, f := range parseNonTestFiles(t, fset, "../uicopy") {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, s := range gd.Specs {
				vs, ok := s.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if !name.IsExported() || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					if v, err := strconv.Unquote(lit.Value); err == nil && v == "" {
						pos := fset.Position(name.Pos())
						t.Errorf("%s:%d: catalog entry %s is empty — it would render as a blank line",
							filepath.Base(pos.Filename), pos.Line, name.Name)
					}
				}
			}
		}
	}
}

type proseLit struct {
	file string
	line int
	val  string
}

// parseNonTestFiles parses every non-test Go file directly in dir, sorted by
// name so a reported position is stable across runs. It is spelled out rather
// than delegating to go/parser's ParseDir because that is deprecated (it
// ignores build tags when grouping files into packages) — grouping is exactly
// what neither caller here wants: both walk files, not packages.
func parseNonTestFiles(t *testing.T, fset *token.FileSet, dir string) []*ast.File {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", filepath.Join(dir, name), err)
		}
		files = append(files, f)
	}
	return files
}

// prosein returns every prose string literal in the non-test Go files of dir.
// Doc comments are excluded by parsing without ParseComments; struct tags are
// excluded explicitly, since a tag is a BasicLit like any other.
func prosein(t *testing.T, dir string) []proseLit {
	t.Helper()

	fset := token.NewFileSet()

	var out []proseLit
	for _, f := range parseNonTestFiles(t, fset, dir) {
		tags := map[token.Pos]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			if fld, ok := n.(*ast.Field); ok && fld.Tag != nil {
				tags[fld.Tag.Pos()] = true
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || tags[lit.Pos()] {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil || !prose(v) {
				return true
			}
			pos := fset.Position(lit.Pos())
			out = append(out, proseLit{filepath.Base(pos.Filename), pos.Line, v})
			return true
		})
	}
	return out
}

// prose reports whether s reads as operator-facing copy rather than as syntax:
// at least one space and at least three consecutive ASCII letters.
func prose(s string) bool {
	if !strings.Contains(s, " ") {
		return false
	}
	run := 0
	for _, r := range s {
		if r <= unicode.MaxASCII && unicode.IsLetter(r) {
			if run++; run >= 3 {
				return true
			}
			continue
		}
		run = 0
	}
	return false
}
