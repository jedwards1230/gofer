package tui

// filemention_test.go covers the `@` file-mention prefix (filemention.go):
// the token grammar's `@` half (including the addresses and mid-word `@`s it
// must NOT hijack), candidate enumeration's two sources and their bounds, and
// the popup rows/completion the shared menu builds from them. White-box
// (package tui) because all of it is unexported.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

func TestActiveTokenMentionGrammar(t *testing.T) {
	tests := []struct {
		name        string
		buf         string
		cursor      int
		wantSigil   rune
		wantPartial string
		wantStart   int
		wantOK      bool
	}{
		{"bare @ at buffer start", "@", 1, '@', "", 0, true},
		{"@ with a partial path", "@internal/tui", 13, '@', "internal/tui", 0, true},
		{"@ after whitespace", "explain @app", 12, '@', "app", 8, true},
		{"slash still resolves to the command sigil", "/mo", 3, '/', "mo", 0, true},
		// The non-triggering cases: an @ that is not at a token boundary is
		// literal text, so an email address never opens a file popup.
		{"email address", "mail sorretin@gmail.com", 23, 0, "", 0, false},
		{"mid-word @", "a@b", 3, 0, "", 0, false},
		{"no token at all", "hello world", 11, 0, "", 0, false},
		{"cursor before the sigil", "@app", 0, 0, "", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sigil, partial, start, ok := activeToken(tt.buf, tt.cursor)
			if ok != tt.wantOK {
				t.Fatalf("activeToken(%q, %d) ok = %v, want %v", tt.buf, tt.cursor, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if sigil != tt.wantSigil || partial != tt.wantPartial || start != tt.wantStart {
				t.Errorf("activeToken(%q, %d) = (%q, %q, %d), want (%q, %q, %d)",
					tt.buf, tt.cursor, sigil, partial, start, tt.wantSigil, tt.wantPartial, tt.wantStart)
			}
		})
	}
}

func TestMatchFilePathsRanksPrefixesFirst(t *testing.T) {
	paths := []string{
		"docs/appendix.md",
		"internal/app/main.go",
		"internal/tui/app.go",
		"vendor/zapp.go",
	}
	// Whole-path prefixes rank first (none here), then base-name prefixes
	// ("appendix.md", "app.go"), then anything merely containing the text
	// ("internal/app/main.go" via its directory, "vendor/zapp.go").
	got := matchFilePaths(paths, "app", 0)
	if len(got) != 4 {
		t.Fatalf("matchFilePaths returned %d rows, want all 4: %v", len(got), got)
	}
	if got[0] != "docs/appendix.md" || got[1] != "internal/tui/app.go" {
		t.Errorf("base-name prefixes should sort first, got %v", got)
	}
	if got[3] != "vendor/zapp.go" {
		t.Errorf("a mere substring match should sort last, got %v", got)
	}
}

func TestMatchFilePathsEmptyPartialReturnsEverythingUpToLimit(t *testing.T) {
	paths := []string{"a.go", "b.go", "c.go"}
	if got := matchFilePaths(paths, "", 0); len(got) != 3 {
		t.Fatalf("empty partial matched %d of 3: %v", len(got), got)
	}
	if got := matchFilePaths(paths, "", 2); len(got) != 2 {
		t.Fatalf("limit ignored: matched %d, want 2", len(got))
	}
}

func TestMatchFilePathsIsCaseInsensitive(t *testing.T) {
	if got := matchFilePaths([]string{"internal/TUI/App.go"}, "tui/app", 0); len(got) != 1 {
		t.Fatalf("case-insensitive match failed: %v", got)
	}
}

// writeTree materializes rel→content files under root.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWalkFilesSkipsGitAndBoundsDepth(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"top.go":              "",
		"a/one.go":            "",
		"a/b/two.go":          "",
		"a/b/c/three.go":      "",
		".git/objects/pack/x": "",
	})

	got := walkFiles(root, 100, 2)
	joined := strings.Join(got, " ")
	if strings.Contains(joined, ".git") {
		t.Fatalf(".git was enumerated: %v", got)
	}
	if !strings.Contains(joined, "top.go") || !strings.Contains(joined, "a/one.go") {
		t.Fatalf("expected the depth-1 and depth-2 files, got %v", got)
	}
	if strings.Contains(joined, "two.go") || strings.Contains(joined, "three.go") {
		t.Fatalf("maxDepth=2 descended too far: %v", got)
	}
}

func TestWalkFilesBoundsEntryCount(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{}
	for i := range 50 {
		files[string(rune('a'+i%26))+string(rune('a'+i/26))+".go"] = ""
	}
	writeTree(t, root, files)

	if got := walkFiles(root, 10, 8); len(got) != 10 {
		t.Fatalf("walkFiles collected %d entries, want the maxEntries bound of 10", len(got))
	}
}

// TestListFileCandidatesHonorsGitignore is the reason the git path exists at
// all: a completion list that offers ignored build output is useless.
func TestListFileCandidatesHonorsGitignore(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		".gitignore":              "node_modules/\n*.log\n",
		"main.go":                 "",
		"node_modules/dep/lib.js": "",
		"debug.log":               "",
	})
	for _, args := range [][]string{{"init"}, {"add", "-A"}} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v failed in the sandbox: %v: %s", args, err, out)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got := listFileCandidates(ctx, root, 100, 8)
	joined := strings.Join(got, " ")

	if !strings.Contains(joined, "main.go") {
		t.Fatalf("expected the tracked file, got %v", got)
	}
	if strings.Contains(joined, "node_modules") || strings.Contains(joined, "debug.log") {
		t.Fatalf("ignored paths reached the completion list: %v", got)
	}
}

func TestListFileCandidatesFallsBackOutsideGit(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"notes.md": "", "src/main.go": ""})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got := listFileCandidates(ctx, root, 100, 8)

	if len(got) != 2 {
		t.Fatalf("walk fallback returned %v, want both files", got)
	}
}

func TestListFileCandidatesEmptyCwd(t *testing.T) {
	if got := listFileCandidates(context.Background(), "", 100, 8); got != nil {
		t.Fatalf("an empty cwd enumerated %v; it must never fall back to the process's own directory", got)
	}
}

// TestFileMenuCompletesPathWithTrailingSpace covers the accept behavior: the
// path replaces the whole @token and leaves the cursor ready to keep typing
// the rest of the prompt.
func TestFileMenuCompletesPathWithTrailingSpace(t *testing.T) {
	files := []string{"internal/tui/app.go", "internal/tui/shell.go"}
	m := newInputMenu(theme.Test(), Registry{}, files, "explain @app", 12)
	if !m.open() {
		t.Fatal("expected the mention popup open")
	}
	got, ok := m.complete("explain @app", 12)
	if !ok {
		t.Fatal("complete reported ok=false on an open menu")
	}
	if want := "explain @internal/tui/app.go "; got != want {
		t.Fatalf("complete = %q, want %q", got, want)
	}
	cursor, _ := m.completionCursor()
	if cursor != len([]rune(got)) {
		t.Errorf("completionCursor = %d, want %d (right after the inserted path)", cursor, len([]rune(got)))
	}
}

// TestFileMenuHasNoCommandToRun: Enter on a mention row must insert, never
// dispatch — there is no Command behind it.
func TestFileMenuHasNoCommandToRun(t *testing.T) {
	m := newInputMenu(theme.Test(), Registry{}, []string{"a.go"}, "@a", 2)
	if _, ok := m.selected(); ok {
		t.Fatal("selected() returned a Command for a file-mention row")
	}
	if _, ok := m.selectedRow(); !ok {
		t.Fatal("selectedRow() found no highlighted row")
	}
}

func TestFileMenuClosedWithNoCandidates(t *testing.T) {
	if m := newInputMenu(theme.Test(), Registry{}, nil, "@a", 2); m.open() {
		t.Fatal("expected the popup closed with no candidates loaded yet")
	}
	if m := newInputMenu(theme.Test(), Registry{}, []string{"a.go"}, "@zzz", 4); m.open() {
		t.Fatal("expected the popup closed when nothing matches")
	}
}

func TestGoldenFileMentionMenu(t *testing.T) {
	files := []string{"cmd/gofer/main.go", "internal/tui/app.go", "internal/tui/shell.go"}
	m := newInputMenu(theme.Test(), Registry{}, files, "@internal", 9)
	testkit.AssertGolden(t, "file_mention_menu", strings.Join(m.Lines(testkit.Width), "\n"))
}

func TestGoldenFileMentionMenuStyled(t *testing.T) {
	files := []string{"cmd/gofer/main.go", "internal/tui/app.go", "internal/tui/shell.go"}
	m := newInputMenu(testkit.ColorTheme(), Registry{}, files, "@internal", 9)
	testkit.AssertGoldenStyled(t, "file_mention_menu", strings.Join(m.Lines(testkit.Width), "\n"))
}

// TestSyncFileCandidatesDispatchesOncePerMention: the enumeration is per
// mention, not per keystroke, and it re-arms once the token goes away.
func TestSyncFileCandidatesDispatchesOncePerMention(t *testing.T) {
	a := App{commandEnv: CommandEnv{Cwd: t.TempDir()}}

	a, cmd := a.syncFileCandidates(inputBuffer{}.SetText("@a"))
	if cmd == nil {
		t.Fatal("the first @ keystroke did not dispatch an enumeration")
	}
	if !a.files.loading {
		t.Fatal("loading was not latched, so every keystroke would re-dispatch")
	}

	if _, again := a.syncFileCandidates(inputBuffer{}.SetText("@ap")); again != nil {
		t.Fatal("a second keystroke inside the same mention re-dispatched the enumeration")
	}

	a.files = fileCandidates{paths: []string{"a.go"}, loaded: true}
	if _, cached := a.syncFileCandidates(inputBuffer{}.SetText("@a")); cached != nil {
		t.Fatal("a loaded cache re-dispatched the enumeration")
	}

	// The token going away re-arms the cache, so the next mention sees files
	// created since (an agent writing one mid-session, say).
	a, _ = a.syncFileCandidates(inputBuffer{}.SetText("plain text"))
	if _, rearmed := a.syncFileCandidates(inputBuffer{}.SetText("@a")); rearmed == nil {
		t.Fatal("the next mention did not re-enumerate")
	}
}

// TestApplyFilesLoadedOpensThePopup covers the landing half: the result
// resyncs the menu against the still-typed mention, which is what makes the
// popup appear without another keystroke.
func TestApplyFilesLoadedOpensThePopup(t *testing.T) {
	a := App{theme: theme.Test(), commandEnv: CommandEnv{Cwd: t.TempDir()}}
	a.over = NewOverview(a.theme, GoldenMeta())
	a.over = a.over.SetInput("@app")
	a.files.loading = true

	a, _ = a.applyFilesLoaded(filesLoadedMsg{paths: []string{"internal/tui/app.go", "docs/TUI.md"}})

	if !a.files.loaded {
		t.Fatal("the cache was not marked loaded, so the next keystroke would re-enumerate")
	}
	if !a.menu.open() {
		t.Fatal("the popup did not open once the candidates landed")
	}
	if row, _ := a.menu.selectedRow(); row.insert != "@internal/tui/app.go " {
		t.Fatalf("highlighted row inserts %q, want the matching path", row.insert)
	}
}
