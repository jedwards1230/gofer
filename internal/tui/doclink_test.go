package tui

// doclink_test.go catches dead godoc links to [App] members.
//
// The class it exists for: a method gets renamed or replaced (here,
// reloadUserCommands → loadUserCommandsCmd when the load moved off the Update
// loop) and every doc link left behind pointing at the old name silently
// degrades to plain bracketed text. Nothing else notices — the compiler has
// no opinion on comments, and golangci-lint does not resolve doc-link targets.
// Two survived a full review round and a mutation-checked test pass before a
// human reader caught them.
//
// Scope is deliberately narrow: only App-member links, only against App's
// own methods and fields. A general doc-link resolver would have to model
// every package, type, const, and dot-import godoc can reach, and would spend
// its life reporting things that are fine. This one question — "does App
// actually have that member?" — is the one that has actually broken.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"
)

// appDocLink matches a godoc link to a member of App — the App-dot-name form,
// bracketed, as godoc spells a link to a type member.
var appDocLink = regexp.MustCompile(`\[App\.([A-Za-z_][A-Za-z0-9_]*)\]`)

// TestAppDocLinksResolve asserts every App-member doc link in this package
// names a real method or field of App.
func TestAppDocLinksResolve(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	fset := token.NewFileSet()
	members := map[string]bool{}
	type link struct {
		name string
		pos  token.Position
	}
	var links []link

	// Every .go file in the directory, parsed individually — not
	// parser.ParseDir, which is deprecated, and not go/packages, which would
	// pull a dependency in for a comment scan. Build tags are deliberately
	// ignored: a dead link in a tagged file is just as dead.
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		file, err := parser.ParseFile(fset, e.Name(), nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("ParseFile(%s): %v", e.Name(), err)
		}
		collectAppMembers(file, members)
		for _, group := range file.Comments {
			for _, c := range group.List {
				for _, m := range appDocLink.FindAllStringSubmatch(c.Text, -1) {
					links = append(links, link{name: m[1], pos: fset.Position(c.Pos())})
				}
			}
		}
	}

	// Guard the guard: if the collectors ever silently stop finding anything,
	// every assertion below passes vacuously.
	if len(members) == 0 {
		t.Fatal("found no members of App at all; the collector is broken, not the docs")
	}
	if len(links) == 0 {
		t.Fatal("found no App-member doc links at all; the scanner is broken, not the docs")
	}

	for _, l := range links {
		if !members[l.name] {
			t.Errorf("%s: doc link [App.%s] names no method or field of App — "+
				"it renders as dead plain text (renamed or removed?)", l.pos, l.name)
		}
	}
}

// collectAppMembers adds App's methods and struct fields from one file.
func collectAppMembers(file *ast.File, into map[string]bool) {
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil || len(d.Recv.List) == 0 {
				continue
			}
			if recvTypeName(d.Recv.List[0].Type) == "App" {
				into[d.Name.Name] = true
			}
		case *ast.GenDecl:
			if d.Tok != token.TYPE {
				continue
			}
			for _, spec := range d.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name.Name != "App" {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				for _, f := range st.Fields.List {
					for _, name := range f.Names {
						into[name.Name] = true
					}
				}
			}
		}
	}
}

// recvTypeName returns a receiver's bare type name, unwrapping a pointer.
func recvTypeName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}
