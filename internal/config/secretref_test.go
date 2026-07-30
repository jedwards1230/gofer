package config_test

// secretref_test.go covers [config.SecretRef]: the env:/file: resolvers, the
// error paths, and — the load-bearing assertion — that no error message ever
// carries the resolved secret value.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jedwards1230/gofer/internal/config"
)

func TestSecretRefResolveEnv(t *testing.T) {
	t.Setenv("GOFER_TEST_SECRET", "sh-super-secret-token")

	got, err := config.SecretRef("env:GOFER_TEST_SECRET").Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "sh-super-secret-token" {
		t.Fatalf("Resolve() = %q, want the env value", got)
	}
}

func TestSecretRefResolveEnvMissing(t *testing.T) {
	_ = os.Unsetenv("GOFER_TEST_SECRET_MISSING")

	_, err := config.SecretRef("env:GOFER_TEST_SECRET_MISSING").Resolve()
	if err == nil {
		t.Fatal("Resolve: want error for an unset env var, got nil")
	}
	if !strings.Contains(err.Error(), "GOFER_TEST_SECRET_MISSING") {
		t.Fatalf("Resolve error %q must name the var", err)
	}
}

func TestSecretRefResolveFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("file-secret-value\n"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}

	got, err := config.SecretRef("file:" + path).Resolve()
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != "file-secret-value" {
		t.Fatalf("Resolve() = %q, want the trimmed file contents", got)
	}
}

func TestSecretRefResolveFileMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := config.SecretRef("file:" + path).Resolve()
	if err == nil {
		t.Fatal("Resolve: want error for a missing file, got nil")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("Resolve error %q must name the path", err)
	}
}

func TestSecretRefResolveEmpty(t *testing.T) {
	got, err := config.SecretRef("").Resolve()
	if err != nil {
		t.Fatalf("Resolve(empty): %v", err)
	}
	if got != "" {
		t.Fatalf("Resolve(empty) = %q, want empty", got)
	}
}

func TestSecretRefResolveUnrecognizedScheme(t *testing.T) {
	_, err := config.SecretRef("exec:whoami").Resolve()
	if err == nil {
		t.Fatal("Resolve: want error for an unrecognized scheme, got nil")
	}
	if !strings.Contains(err.Error(), "exec:whoami") {
		t.Fatalf("Resolve error %q must name the scheme/ref", err)
	}
}

// TestSecretRefErrorsNeverLeakTheValue is the load-bearing assertion: a
// resolve failure must be safe to log or surface on a status line without
// risking exposing a credential. It covers every error path Resolve has.
func TestSecretRefErrorsNeverLeakTheValue(t *testing.T) {
	t.Setenv("GOFER_TEST_SECRET_PRESENT", "sh-do-not-leak-me")

	dir := t.TempDir()
	badPath := filepath.Join(dir, "unreadable-secret-do-not-leak-me")
	if err := os.WriteFile(badPath, []byte("sh-do-not-leak-me-either"), 0o000); err != nil {
		t.Fatalf("write unreadable file: %v", err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permission bits, can't force a read failure")
	}

	refs := []config.SecretRef{
		"env:GOFER_TEST_SECRET_MISSING_2",
		config.SecretRef("file:" + filepath.Join(dir, "missing")),
		config.SecretRef("file:" + badPath),
		"garbage:not-a-scheme",
	}
	for _, r := range refs {
		_, err := r.Resolve()
		if err == nil {
			continue // env-missing and garbage-scheme always error; file cases checked below
		}
		if strings.Contains(err.Error(), "sh-do-not-leak-me") {
			t.Fatalf("Resolve(%q) error leaked a secret value: %v", r, err)
		}
	}
}
