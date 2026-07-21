package tui

// settings_test.go covers the setting registry (settings.go, M4 step 3):
// every row's Get/Set round trip and default-value resolution.

import (
	"testing"

	"github.com/jedwards1230/gofer/internal/config"
)

// TestSettingsRegistryDefaults covers each setting's Get against the zero
// Config, matching docs/projects/gofer-m4-command-views-plan.md §3a's
// concrete default table.
func TestSettingsRegistryDefaults(t *testing.T) {
	want := map[string]string{
		"session.model":           "",
		"session.effort":          "off",
		"session.permission_mode": "ask",
		"tui.roster_view":         "flat",
		"tui.autoscroll":          "true",
		"tui.mouse":               "true",
		"telemetry.enabled":       "false",
		"telemetry.endpoint":      "",
	}
	reg := settingsRegistry()
	if len(reg) != len(want) {
		t.Fatalf("registry has %d settings, want %d", len(reg), len(want))
	}
	for _, s := range reg {
		wantVal, ok := want[s.Key]
		if !ok {
			t.Fatalf("unexpected setting key %q", s.Key)
		}
		if got := s.Get(config.Config{}); got != wantVal {
			t.Errorf("%s: Get(zero Config) = %q, want %q", s.Key, got, wantVal)
		}
	}
}

// TestSettingsRegistryRoundTrip covers every setting's Set(Get(...)) == the
// value just set, proving the registry's Get/Set pair is a faithful
// read/write through config.Config regardless of Kind.
func TestSettingsRegistryRoundTrip(t *testing.T) {
	for _, s := range settingsRegistry() {
		t.Run(s.Key, func(t *testing.T) {
			var values []string
			switch {
			case len(s.Options) > 0:
				values = s.Options
			case s.Kind == SettingBool:
				values = []string{"true", "false"}
			default:
				values = []string{"a-value", "another-value"}
			}
			for _, v := range values {
				got := s.Get(s.Set(config.Config{}, v))
				if got != v {
					t.Errorf("Set(%q) then Get = %q, want %q", v, got, v)
				}
			}
		})
	}
}

// TestSettingsRegistryKinds pins each key's [SettingKind] and, for enums,
// its Options — a schema change here is a deliberate, reviewable diff.
func TestSettingsRegistryKinds(t *testing.T) {
	want := map[string]struct {
		kind    SettingKind
		options []string
	}{
		"session.model":           {SettingString, nil},
		"session.effort":          {SettingEnum, []string{"off", "low", "medium", "high"}},
		"session.permission_mode": {SettingEnum, []string{"ask", "yolo"}},
		"tui.roster_view":         {SettingEnum, []string{"flat", "grouped"}},
		"tui.autoscroll":          {SettingBool, nil},
		"tui.mouse":               {SettingBool, nil},
		"telemetry.enabled":       {SettingBool, nil},
		"telemetry.endpoint":      {SettingString, nil},
	}
	for _, s := range settingsRegistry() {
		w, ok := want[s.Key]
		if !ok {
			t.Fatalf("unexpected setting key %q", s.Key)
		}
		if s.Kind != w.kind {
			t.Errorf("%s: Kind = %v, want %v", s.Key, s.Kind, w.kind)
		}
		if len(s.Options) != len(w.options) {
			t.Errorf("%s: Options = %v, want %v", s.Key, s.Options, w.options)
			continue
		}
		for i := range w.options {
			if s.Options[i] != w.options[i] {
				t.Errorf("%s: Options[%d] = %q, want %q", s.Key, i, s.Options[i], w.options[i])
			}
		}
	}
}
