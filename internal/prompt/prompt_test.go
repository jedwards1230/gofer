package prompt

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jedwards1230/gofer/internal/config"
)

// writeFile is a small test helper: it writes contents at dir/name, creating
// any parent directories, and fails the test on error.
func writeFile(t *testing.T, dir, name, contents string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// TestCompose_ResolutionForms exercises every [config.Prompt.Files] entry
// form documented on [config.Prompt]: builtin, absolute, "~/…", and the
// cwd-then-root relative search.
func TestCompose_ResolutionForms(t *testing.T) {
	cwd := t.TempDir()
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)

	abs := writeFile(t, t.TempDir(), "abs.md", "absolute content")
	writeFile(t, home, "house.md", "home content")
	writeFile(t, cwd, "AGENTS.md", "cwd content")
	writeFile(t, root, "root-only.md", "root content")

	tests := []struct {
		name  string
		entry string
		want  string
	}{
		{"builtin", "builtin:system.md", strings.TrimSpace(builtinSystemMD)},
		{"absolute", abs, "absolute content"},
		{"tilde", "~/house.md", "home content"},
		{"cwd-relative", "AGENTS.md", "cwd content"},
		{"root-relative-fallback", "root-only.md", "root content"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, warnings, err := Compose(config.Prompt{Files: []string{tt.entry}}, cwd, root)
			if err != nil {
				t.Fatalf("Compose: %v", err)
			}
			if len(warnings) != 0 {
				t.Fatalf("warnings = %v, want none", warnings)
			}
			if got.Text != tt.want {
				t.Errorf("Text = %q, want %q", got.Text, tt.want)
			}
			if len(got.Sources) != 1 {
				t.Fatalf("Sources = %v, want exactly one entry", got.Sources)
			}
		})
	}
}

// TestCompose_CwdWinsOverRoot asserts the documented precedence: a bare
// relative entry present in BOTH cwd and the store root resolves to the cwd
// copy.
func TestCompose_CwdWinsOverRoot(t *testing.T) {
	cwd := t.TempDir()
	root := t.TempDir()
	writeFile(t, cwd, "AGENTS.md", "cwd wins")
	writeFile(t, root, "AGENTS.md", "root loses")

	got, warnings, err := Compose(config.Prompt{Files: []string{"AGENTS.md"}}, cwd, root)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("Compose: %v, warnings %v", err, warnings)
	}
	if got.Text != "cwd wins" {
		t.Errorf("Text = %q, want %q", got.Text, "cwd wins")
	}
}

// TestCompose_MissingFile covers both configured behaviors for an
// unresolvable source: a warning by default, an error under
// MissingFileIsError.
func TestCompose_MissingFile(t *testing.T) {
	cwd := t.TempDir()
	root := t.TempDir()

	t.Run("warns by default", func(t *testing.T) {
		got, warnings, err := Compose(config.Prompt{Files: []string{"AGENTS.md"}}, cwd, root)
		if err != nil {
			t.Fatalf("Compose: %v", err)
		}
		if len(warnings) != 1 {
			t.Fatalf("warnings = %v, want exactly one", warnings)
		}
		if warnings[0].Entry != "AGENTS.md" {
			t.Errorf("warning entry = %q, want %q", warnings[0].Entry, "AGENTS.md")
		}
		if got.Text != "" || len(got.Sources) != 0 {
			t.Errorf("Composed = %+v, want empty (nothing resolved)", got)
		}
	})

	t.Run("fails under MissingFileIsError", func(t *testing.T) {
		_, _, err := Compose(config.Prompt{Files: []string{"AGENTS.md"}, MissingFileIsError: true}, cwd, root)
		if err == nil {
			t.Fatal("Compose: want error, got nil")
		}
	})
}

// TestCompose_DedupFirstWins asserts that listing the same source twice
// contributes it once, and that the FIRST listed spelling — not a later
// duplicate — decides what shows up in Sources.
func TestCompose_DedupFirstWins(t *testing.T) {
	cwd := t.TempDir()
	abs := writeFile(t, cwd, "AGENTS.md", "one copy only")

	got, warnings, err := Compose(config.Prompt{Files: []string{"AGENTS.md", abs}}, cwd, "")
	if err != nil || len(warnings) != 0 {
		t.Fatalf("Compose: %v, warnings %v", err, warnings)
	}
	if got.Text != "one copy only" {
		t.Errorf("Text = %q, want no duplication", got.Text)
	}
	if len(got.Sources) != 1 {
		t.Errorf("Sources = %v, want exactly one (deduped)", got.Sources)
	}
}

// TestCompose_ByteCap asserts [config.Prompt.MaxFileBytes] is enforced (as a
// warning, not a truncation) and that an explicit 0 lifts the cap entirely.
func TestCompose_ByteCap(t *testing.T) {
	cwd := t.TempDir()
	writeFile(t, cwd, "big.md", "0123456789")

	small := 5
	got, warnings, err := Compose(config.Prompt{Files: []string{"big.md"}, MaxFileBytes: &small}, cwd, "")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one (over cap)", warnings)
	}
	if got.Text != "" {
		t.Errorf("Text = %q, want empty (skipped, not truncated)", got.Text)
	}

	noLimit := 0
	got, warnings, err = Compose(config.Prompt{Files: []string{"big.md"}, MaxFileBytes: &noLimit}, cwd, "")
	if err != nil || len(warnings) != 0 {
		t.Fatalf("Compose: %v, warnings %v", err, warnings)
	}
	if got.Text != "0123456789" {
		t.Errorf("Text = %q, want the full file (explicit 0 = no limit)", got.Text)
	}
}

// TestCompose_OrderAndJoin asserts sources compose in list order, joined by
// exactly one blank line, each trimmed of surrounding whitespace first.
func TestCompose_OrderAndJoin(t *testing.T) {
	cwd := t.TempDir()
	writeFile(t, cwd, "first.md", "  first line  \n")
	writeFile(t, cwd, "second.md", "\nsecond line\n\n")

	got, warnings, err := Compose(config.Prompt{Files: []string{"first.md", "second.md"}}, cwd, "")
	if err != nil || len(warnings) != 0 {
		t.Fatalf("Compose: %v, warnings %v", err, warnings)
	}
	want := "first line\n\nsecond line"
	if got.Text != want {
		t.Errorf("Text = %q, want %q", got.Text, want)
	}
	if len(got.Sources) != 2 {
		t.Fatalf("Sources = %v, want two entries in order", got.Sources)
	}
}

// TestCompose_DefaultIsBuiltin asserts the zero-value [config.Prompt]
// resolves to the embedded default asset alone — the fail-safe floor a fresh
// install relies on.
func TestCompose_DefaultIsBuiltin(t *testing.T) {
	got, warnings, err := Compose(config.Prompt{}, t.TempDir(), t.TempDir())
	if err != nil || len(warnings) != 0 {
		t.Fatalf("Compose: %v, warnings %v", err, warnings)
	}
	if got.Text != strings.TrimSpace(builtinSystemMD) {
		t.Errorf("Text = %q, want the embedded default", got.Text)
	}
	if got.Sources[0] != config.DefaultPromptAsset {
		t.Errorf("Sources[0] = %q, want %q", got.Sources[0], config.DefaultPromptAsset)
	}
	if got.SHA256 == "" {
		t.Error("SHA256 is empty")
	}
}

// TestCompose_UnknownBuiltin asserts an unshipped builtin name behaves like
// any other unresolvable source (warn, or error under MissingFileIsError) —
// a config typo, not a panic or a distinct error shape.
func TestCompose_UnknownBuiltin(t *testing.T) {
	_, warnings, err := Compose(config.Prompt{Files: []string{"builtin:nope.md"}}, "", "")
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", warnings)
	}
	if warnings[0].Entry != "builtin:nope.md" {
		t.Errorf("warning entry = %q, want %q", warnings[0].Entry, "builtin:nope.md")
	}
	if errors.Unwrap(warnings[0]) == nil {
		t.Error("Unwrap() = nil, want the underlying resolution error")
	}
}
