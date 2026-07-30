package config_test

import (
	"path/filepath"
	"testing"

	"github.com/jedwards1230/gofer/internal/config"
)

func TestSkillsDirectories(t *testing.T) {
	root, cwd := "/store/root", "/work/cwd"

	got := (config.Skills{}).Directories(root, cwd)
	// Project (cwd) FIRST: skill.Load is first-directory-wins (PATH-style),
	// so the project directory must lead the list for a same-named project
	// skill to beat a global one — see Directories' doc for why this order
	// is precedence, not just a search list.
	want := []string{
		filepath.Join(cwd, ".gofer", "skills"),
		filepath.Join(root, "skills"),
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("Directories() unset = %v, want %v", got, want)
	}

	explicit := config.Skills{Dirs: []string{"/custom/skills"}}
	if got := explicit.Directories(root, cwd); len(got) != 1 || got[0] != "/custom/skills" {
		t.Fatalf("Directories() explicit = %v, want [/custom/skills]", got)
	}
}

func TestSkillsFileLimitBytes(t *testing.T) {
	n := func(v int) *int { return &v }
	tests := []struct {
		name string
		in   *int
		want int
	}{
		{"unset resolves to default", nil, config.DefaultSkillMaxFileBytes},
		{"negative resolves to default", n(-1), config.DefaultSkillMaxFileBytes},
		{"zero is no limit", n(0), 0},
		{"explicit value is the cap", n(1024), 1024},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := config.Skills{MaxFileBytes: tt.in}
			if got := s.FileLimitBytes(); got != tt.want {
				t.Fatalf("FileLimitBytes() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSkillsDescriptionLimitBytes(t *testing.T) {
	n := func(v int) *int { return &v }
	tests := []struct {
		name string
		in   *int
		want int
	}{
		{"unset resolves to default", nil, config.DefaultSkillDescriptionBytes},
		{"negative resolves to default", n(-1), config.DefaultSkillDescriptionBytes},
		{"zero is honored", n(0), 0},
		{"explicit value wins", n(160), 160},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := config.Skills{DescriptionBytes: tt.in}
			if got := s.DescriptionLimitBytes(); got != tt.want {
				t.Fatalf("DescriptionLimitBytes() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSkillsIsDisabled(t *testing.T) {
	s := config.Skills{Disabled: []string{"experimental-thing"}}
	if !s.IsDisabled("experimental-thing") {
		t.Fatal("IsDisabled(experimental-thing) = false, want true")
	}
	if s.IsDisabled("normal-thing") {
		t.Fatal("IsDisabled(normal-thing) = true, want false")
	}
}
