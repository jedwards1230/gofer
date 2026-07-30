package config_test

import (
	"testing"

	"github.com/jedwards1230/gofer/internal/config"
)

// TestToolsSchemasFailSafe pins the fail-safe polarity: only the exact
// spelling "index" opts in. Everything else — including a typo or a value
// written by a newer gofer — must resolve to preload, the mode whose worst
// case is wasted context, never incapacity.
func TestToolsSchemasFailSafe(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want config.ToolSchemaMode
	}{
		{"unset resolves to preload", "", config.ToolSchemaModePreload},
		{"index opts in", "index", config.ToolSchemaModeIndex},
		{"preload stays preload", "preload", config.ToolSchemaModePreload},
		{"a typo resolves to preload", "indexx", config.ToolSchemaModePreload},
		{"case matters — resolves to preload", "INDEX", config.ToolSchemaModePreload},
		{"a mode from a newer gofer resolves to preload", "streamed", config.ToolSchemaModePreload},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := (config.Tools{SchemaMode: tt.in}).Schemas()
			if got != tt.want {
				t.Errorf("Tools{SchemaMode: %q}.Schemas() = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestToolsResidentToolsReturnsACopy pins that mutating the returned slice
// never mutates the package default — the whole reason ResidentTools copies
// rather than returning DefaultResidentTools directly.
func TestToolsResidentToolsReturnsACopy(t *testing.T) {
	before := make([]string, len(config.DefaultResidentTools))
	copy(before, config.DefaultResidentTools)

	got := (config.Tools{}).ResidentTools()
	if len(got) == 0 {
		t.Fatal("ResidentTools() on zero value = empty, want DefaultResidentTools")
	}
	got[0] = "MUTATED"

	for i, v := range config.DefaultResidentTools {
		if v != before[i] {
			t.Fatalf("DefaultResidentTools mutated via ResidentTools()'s return: %v", config.DefaultResidentTools)
		}
	}

	// Explicit config wins verbatim.
	explicit := config.Tools{Resident: []string{"bash"}}
	if got := explicit.ResidentTools(); len(got) != 1 || got[0] != "bash" {
		t.Fatalf("ResidentTools() with explicit config = %v, want [bash]", got)
	}
}

func TestToolsSummaryLimitBytes(t *testing.T) {
	n := func(v int) *int { return &v }
	tests := []struct {
		name string
		in   *int
		want int
	}{
		{"unset resolves to default", nil, config.DefaultToolSummaryBytes},
		{"negative resolves to default", n(-1), config.DefaultToolSummaryBytes},
		{"zero is no limit", n(0), 0},
		{"explicit value is the cap", n(80), 80},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tl := config.Tools{SummaryBytes: tt.in}
			if got := tl.SummaryLimitBytes(); got != tt.want {
				t.Fatalf("SummaryLimitBytes() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestToolsSearchResultLimit(t *testing.T) {
	n := func(v int) *int { return &v }
	tests := []struct {
		name string
		in   *int
		want int
	}{
		{"unset resolves to default", nil, config.DefaultToolSearchResults},
		{"zero resolves to default", n(0), config.DefaultToolSearchResults},
		{"negative resolves to default", n(-1), config.DefaultToolSearchResults},
		{"explicit value wins", n(20), 20},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tl := config.Tools{SearchResults: tt.in}
			if got := tl.SearchResultLimit(); got != tt.want {
				t.Fatalf("SearchResultLimit() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestToolsInlineIndexLimit(t *testing.T) {
	n := func(v int) *int { return &v }
	tests := []struct {
		name string
		in   *int
		want int
	}{
		{"unset resolves to default", nil, config.DefaultToolInlineIndexMax},
		{"zero resolves to default", n(0), config.DefaultToolInlineIndexMax},
		{"negative resolves to default", n(-1), config.DefaultToolInlineIndexMax},
		{"explicit value wins", n(50), 50},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tl := config.Tools{InlineIndexMax: tt.in}
			if got := tl.InlineIndexLimit(); got != tt.want {
				t.Fatalf("InlineIndexLimit() = %d, want %d", got, tt.want)
			}
		})
	}
}
