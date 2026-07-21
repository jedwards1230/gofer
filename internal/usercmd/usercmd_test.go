package usercmd_test

// usercmd_test.go covers discovery and frontmatter through the real entry
// point, [usercmd.Load], over a temporary directory tree — the same syscalls
// the TUI makes, not a stubbed filesystem.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jedwards1230/gofer/internal/usercmd"
)

// writeCmd creates dir/rel with content, making parents as needed.
func writeCmd(t *testing.T, dir, rel, content string) string {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}

// scopes returns a (root, cwd) pair whose commands directories are freshly
// created and empty, ready for writeCmd.
func scopes(t *testing.T) (root, cwd string) {
	t.Helper()
	root, cwd = t.TempDir(), t.TempDir()
	for _, d := range []string{usercmd.UserDir(root), usercmd.ProjectDir(cwd)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}
	return root, cwd
}

// byName indexes a Load result for assertions.
func byName(cmds []usercmd.Command) map[string]usercmd.Command {
	m := make(map[string]usercmd.Command, len(cmds))
	for _, c := range cmds {
		m[c.Name] = c
	}
	return m
}

// TestLoadMissingDirectories is the overwhelmingly common case — a user with
// no commands at all. It must be silent, not a warning.
func TestLoadMissingDirectories(t *testing.T) {
	cmds, warns := usercmd.Load(t.TempDir(), t.TempDir())
	if len(cmds) != 0 || len(warns) != 0 {
		t.Fatalf("Load(empty roots) = %v, %v; want no commands and no warnings", cmds, warns)
	}
	if cmds, warns := usercmd.Load("", ""); len(cmds) != 0 || len(warns) != 0 {
		t.Fatalf(`Load("", "") = %v, %v; want no commands and no warnings`, cmds, warns)
	}
}

// TestLoadNestingNamespaces covers the directory→`:` namespacing rule and
// that only .md files are taken.
func TestLoadNestingNamespaces(t *testing.T) {
	root, cwd := scopes(t)
	writeCmd(t, usercmd.UserDir(root), "review.md", "review the diff")
	writeCmd(t, usercmd.UserDir(root), "git/review.md", "review the git diff")
	writeCmd(t, usercmd.UserDir(root), "git/deep/audit.md", "audit")
	writeCmd(t, usercmd.UserDir(root), "notes.txt", "not a command")
	writeCmd(t, usercmd.UserDir(root), ".hidden.md", "not a command")
	writeCmd(t, usercmd.UserDir(root), ".git/config.md", "not a command")

	cmds, warns := usercmd.Load(root, cwd)
	if len(warns) != 0 {
		t.Fatalf("warnings = %v; want none", warns)
	}
	var names []string
	for _, c := range cmds {
		names = append(names, c.Name)
	}
	want := []string{"git:deep:audit", "git:review", "review"} // Load sorts by Name
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("names = %v; want %v", names, want)
	}
	if got := byName(cmds)["git:review"].Body; got != "review the git diff" {
		t.Errorf("git:review body = %q; want the nested file's content", got)
	}
}

// TestLoadProjectOverridesUser pins the scope precedence: a project file wins
// a same-name collision, and a user-only command still loads alongside it.
func TestLoadProjectOverridesUser(t *testing.T) {
	root, cwd := scopes(t)
	writeCmd(t, usercmd.UserDir(root), "review.md", "user body")
	writeCmd(t, usercmd.UserDir(root), "only-user.md", "user only")
	writeCmd(t, usercmd.ProjectDir(cwd), "review.md", "project body")

	cmds, warns := usercmd.Load(root, cwd)
	if len(warns) != 0 {
		t.Fatalf("warnings = %v; want none", warns)
	}
	idx := byName(cmds)
	if len(idx) != 2 {
		t.Fatalf("commands = %v; want exactly review + only-user", cmds)
	}
	if got := idx["review"]; got.Body != "project body" || got.Scope != usercmd.ScopeProject {
		t.Errorf("review = %+v; want the PROJECT file to shadow the user one", got)
	}
	if got := idx["only-user"].Scope; got != usercmd.ScopeUser {
		t.Errorf("only-user scope = %q; want %q", got, usercmd.ScopeUser)
	}
}

// TestLoadSameDirectoryOnce covers `--root <cwd>/.gofer`, where both scopes
// resolve to one directory: it must load once, not report every command twice
// or race the two scopes against each other.
func TestLoadSameDirectoryOnce(t *testing.T) {
	cwd := t.TempDir()
	root := filepath.Join(cwd, ".gofer")
	writeCmd(t, usercmd.UserDir(root), "review.md", "body")

	cmds, warns := usercmd.Load(root, cwd)
	if len(warns) != 0 {
		t.Fatalf("warnings = %v; want none", warns)
	}
	if len(cmds) != 1 || cmds[0].Name != "review" || cmds[0].Scope != usercmd.ScopeProject {
		t.Fatalf("commands = %+v; want one project-scope /review", cmds)
	}
}

// TestLoadSkipsIllegalNames covers a filename that could never be typed as a
// command: it is skipped WITH a warning naming the file, and the legal
// commands beside it still load.
func TestLoadSkipsIllegalNames(t *testing.T) {
	root, cwd := scopes(t)
	writeCmd(t, usercmd.UserDir(root), "my review.md", "whitespace in the name")
	writeCmd(t, usercmd.UserDir(root), "ns/has:colon.md", "colon is the namespace separator")
	writeCmd(t, usercmd.UserDir(root), "fine.md", "legal")

	cmds, warns := usercmd.Load(root, cwd)
	if len(cmds) != 1 || cmds[0].Name != "fine" {
		t.Fatalf("commands = %+v; want only the legal one", cmds)
	}
	if len(warns) != 2 {
		t.Fatalf("warnings = %v; want one per skipped file", warns)
	}
	joined := warns[0].Error() + "\n" + warns[1].Error()
	for _, want := range []string{"my review.md", "has:colon.md"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings %q do not name %q — a skipped file must say which file", joined, want)
		}
	}
}

func TestLoadFrontmatter(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		wantDesc string
		wantHint string
		wantBody string
		wantWarn bool
	}{
		{
			name:     "both keys",
			content:  "---\ndescription: Review a pull request\nargument-hint: [pr-number]\n---\nreview PR $1\n",
			wantDesc: "Review a pull request",
			wantHint: "[pr-number]",
			wantBody: "review PR $1\n",
		},
		{
			name:     "absent",
			content:  "review the diff\n",
			wantDesc: "user markdown command",
			wantBody: "review the diff\n",
		},
		{
			name:     "unknown keys are ignored, not an error",
			content:  "---\nmodel: claude-opus-4-8\ndescription: Ship it\nallowed-tools: bash\n---\nbody\n",
			wantDesc: "Ship it",
			wantBody: "body\n",
		},
		{
			name:     "quoted values are unquoted",
			content:  "---\ndescription: \"Review: the diff\"\nargument-hint: '[id]'\n---\nbody\n",
			wantDesc: "Review: the diff",
			wantHint: "[id]",
			wantBody: "body\n",
		},
		{
			name:     "comments and blank lines are skipped",
			content:  "---\n# a note\n\ndescription: Ship it\n---\nbody\n",
			wantDesc: "Ship it",
			wantBody: "body\n",
		},
		{
			name:     "empty frontmatter block",
			content:  "---\n---\nbody\n",
			wantDesc: "user markdown command",
			wantBody: "body\n",
		},
		{
			// A body that merely CONTAINS "---" further down is not a header.
			name:     "a mid-file rule is not frontmatter",
			content:  "intro\n---\ndescription: not a header\n---\n",
			wantDesc: "user markdown command",
			wantBody: "intro\n---\ndescription: not a header\n---\n",
		},
		{
			name:     "malformed: never closed",
			content:  "---\ndescription: Ship it\nbody with no closing fence\n",
			wantDesc: "user markdown command",
			wantBody: "---\ndescription: Ship it\nbody with no closing fence\n",
			wantWarn: true,
		},
		{
			name:     "malformed: not a key/value line",
			content:  "---\ndescription: Ship it\njust a sentence\n---\nbody\n",
			wantDesc: "user markdown command",
			wantBody: "---\ndescription: Ship it\njust a sentence\n---\nbody\n",
			wantWarn: true,
		},
		{
			name:     "CRLF fences still parse",
			content:  "---\r\ndescription: Ship it\r\n---\r\nbody\r\n",
			wantDesc: "Ship it",
			wantBody: "body\r\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root, cwd := scopes(t)
			writeCmd(t, usercmd.UserDir(root), "c.md", tt.content)

			cmds, warns := usercmd.Load(root, cwd)
			if len(cmds) != 1 {
				t.Fatalf("commands = %+v; want exactly one — malformed frontmatter must degrade, never drop the command", cmds)
			}
			got := cmds[0]
			if got.Description != tt.wantDesc {
				t.Errorf("Description = %q, want %q", got.Description, tt.wantDesc)
			}
			if got.ArgumentHint != tt.wantHint {
				t.Errorf("ArgumentHint = %q, want %q", got.ArgumentHint, tt.wantHint)
			}
			if got.Body != tt.wantBody {
				t.Errorf("Body = %q, want %q", got.Body, tt.wantBody)
			}
			if gotWarn := len(warns) > 0; gotWarn != tt.wantWarn {
				t.Errorf("warnings = %v; want any = %v", warns, tt.wantWarn)
			}
		})
	}
}

// TestLoadDefaultSummaryNamesTheScope checks the no-frontmatter default says
// where the file came from, since that is the only thing the loader can
// honestly say about a file that describes itself not at all.
func TestLoadDefaultSummaryNamesTheScope(t *testing.T) {
	root, cwd := scopes(t)
	writeCmd(t, usercmd.ProjectDir(cwd), "p.md", "body")

	cmds, _ := usercmd.Load(root, cwd)
	if len(cmds) != 1 || cmds[0].Description != "project markdown command" {
		t.Fatalf("commands = %+v; want a project-scope default summary", cmds)
	}
}
