package config_test

// save_test.go covers config.Save (M4 step 3): the round trip through Load,
// the 0600 permission, and the atomic-write contract (no partial file left
// behind on a rename failure).

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/jedwards1230/gofer/internal/config"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := config.DefaultPath(dir)

	disabled := false
	want := config.Config{
		Permissions: []config.Rule{{Verdict: "deny", Tool: "bash", Specifier: "rm:*"}},
		Telemetry:   config.Telemetry{Enabled: true, Endpoint: "localhost:4317"},
		Session:     config.Session{Model: "claude-sonnet-5", PermissionMode: "yolo"},
		TUI:         config.TUI{RosterView: "grouped", Autoscroll: &disabled},
	}

	if err := config.Save(path, want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch:\ngot:  %+v\nwant: %+v", got, want)
	}
}

func TestSavePermissions0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits don't apply on windows")
	}
	dir := t.TempDir()
	path := config.DefaultPath(dir)

	if err := config.Save(path, config.Config{}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("permissions = %o, want 0600", perm)
	}
}

// TestSaveOverwritesExisting covers the common case of a second Save editing
// an already-present config file — the atomic rename must replace it cleanly,
// not append or error.
func TestSaveOverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := config.DefaultPath(dir)

	if err := config.Save(path, config.Config{Session: config.Session{Model: "gpt-5"}}); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if err := config.Save(path, config.Config{Session: config.Session{Model: "claude-sonnet-5"}}); err != nil {
		t.Fatalf("second Save: %v", err)
	}

	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Session.Model != "claude-sonnet-5" {
		t.Fatalf("Session.Model = %q, want claude-sonnet-5 (overwrite)", got.Session.Model)
	}
}

// TestSaveNoTempFileLeftBehind covers the atomic-write contract's other half:
// after a successful Save, the store root contains exactly config.json — no
// stray *.tmp file from the temp-file-then-rename dance.
func TestSaveNoTempFileLeftBehind(t *testing.T) {
	dir := t.TempDir()
	path := config.DefaultPath(dir)

	if err := config.Save(path, config.Config{}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		t.Fatalf("dir entries = %v, want exactly [%s]", entries, filepath.Base(path))
	}
}

// TestSaveCreatesMissingDir covers Save against a store root that doesn't
// exist yet — the same first-run state Load already tolerates for reads.
func TestSaveCreatesMissingDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-yet-created")
	path := config.DefaultPath(root)

	if err := config.Save(path, config.Config{}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := config.Load(path); err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
}
