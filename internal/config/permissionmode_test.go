package config_test

// permissionmode_test.go covers [config.Session.Mode] — the resolver every
// consumer of the /yolo toggle reads the raw config string through. Its whole
// job is to fail SAFE, so the unrecognized cases matter as much as the two
// documented ones.

import (
	"testing"

	"github.com/jedwards1230/gofer/internal/config"
)

func TestSessionModeResolvesFailSafe(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want config.PermissionMode
	}{
		{"unset resolves to ask", "", config.PermissionModeAsk},
		{"ask stays ask", "ask", config.PermissionModeAsk},
		{"yolo is the only way to yolo", "yolo", config.PermissionModeYolo},
		{"a typo resolves to ask", "yollo", config.PermissionModeAsk},
		{"case matters — this is a config value, not a word", "YOLO", config.PermissionModeAsk},
		{"a mode from a newer gofer resolves to ask", "plan", config.PermissionModeAsk},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (config.Session{PermissionMode: tt.raw}).Mode(); got != tt.want {
				t.Errorf("Session{PermissionMode: %q}.Mode() = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// TestZeroConfigIsAsk pins the whole-config default: an unconfigured gofer
// runs contain-or-ask, which is the same promise [config.Config.Engine]'s doc
// makes about an empty ruleset.
func TestZeroConfigIsAsk(t *testing.T) {
	if got := (config.Config{}).Session.Mode(); got != config.PermissionModeAsk {
		t.Fatalf("zero Config resolves to %q, want %q — an unconfigured gofer must never start in yolo",
			got, config.PermissionModeAsk)
	}
}
