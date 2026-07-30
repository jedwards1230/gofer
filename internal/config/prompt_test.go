package config_test

import (
	"testing"

	"github.com/jedwards1230/gofer/internal/config"
)

func TestPromptSources(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"unset resolves to the builtin asset", nil, []string{config.DefaultPromptAsset}},
		{"empty slice resolves to the builtin asset", []string{}, []string{config.DefaultPromptAsset}},
		{"explicit list wins", []string{"AGENTS.md", "CLAUDE.md"}, []string{"AGENTS.md", "CLAUDE.md"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := config.Prompt{Files: tt.in}
			got := p.Sources()
			if len(got) != len(tt.want) {
				t.Fatalf("Sources() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("Sources() = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestPromptFileLimitBytes(t *testing.T) {
	n := func(v int) *int { return &v }
	tests := []struct {
		name string
		in   *int
		want int
	}{
		{"unset resolves to default", nil, config.DefaultPromptMaxFileBytes},
		{"negative resolves to default", n(-1), config.DefaultPromptMaxFileBytes},
		{"zero is no limit", n(0), 0},
		{"explicit value is the cap", n(4096), 4096},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := config.Prompt{MaxFileBytes: tt.in}
			if got := p.FileLimitBytes(); got != tt.want {
				t.Fatalf("FileLimitBytes() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestPromptZeroValueIsValid pins that Config{} still yields a usable
// prompt source list — the fail-safe floor a fresh install relies on.
func TestPromptZeroValueIsValid(t *testing.T) {
	got := config.Config{}.Prompt.Sources()
	if len(got) != 1 || got[0] != config.DefaultPromptAsset {
		t.Fatalf("zero Config Prompt.Sources() = %v, want [%s]", got, config.DefaultPromptAsset)
	}
}
