package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/gofer/internal/config"
)

// TestSearchSelectedFailSafe pins the fail-safe polarity: only "brave" and
// "searxng" opt in; everything else — unset, a typo, a provider from a
// newer gofer — resolves to none, so an upgrade never silently starts
// making outbound search calls.
func TestSearchSelectedFailSafe(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want config.SearchProvider
	}{
		{"unset resolves to none", "", config.SearchProviderNone},
		{"brave opts in", "brave", config.SearchProviderBrave},
		{"searxng opts in", "searxng", config.SearchProviderSearXNG},
		{"none stays none", "none", config.SearchProviderNone},
		{"a typo resolves to none", "brve", config.SearchProviderNone},
		{"case matters — resolves to none", "BRAVE", config.SearchProviderNone},
		{"a provider from a newer gofer resolves to none", "duckduckgo", config.SearchProviderNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := (config.Search{Provider: tt.in}).Selected()
			if got != tt.want {
				t.Errorf("Search{Provider: %q}.Selected() = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSearchTimeout(t *testing.T) {
	ms := func(v int) *int { return &v }
	tests := []struct {
		name string
		in   *int
		want time.Duration
	}{
		{"unset resolves to default", nil, config.DefaultSearchTimeout},
		{"zero resolves to default", ms(0), config.DefaultSearchTimeout},
		{"negative resolves to default", ms(-1), config.DefaultSearchTimeout},
		{"explicit value wins", ms(2500), 2500 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (config.Search{TimeoutMS: tt.in}).Timeout(); got != tt.want {
				t.Fatalf("Timeout() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestSearchResultLimit(t *testing.T) {
	n := func(v int) *int { return &v }
	tests := []struct {
		name string
		in   *int
		want int
	}{
		{"unset resolves to default", nil, config.DefaultSearchMaxResults},
		{"zero resolves to default", n(0), config.DefaultSearchMaxResults},
		{"negative resolves to default", n(-1), config.DefaultSearchMaxResults},
		{"explicit value wins", n(3), 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (config.Search{MaxResults: tt.in}).ResultLimit(); got != tt.want {
				t.Fatalf("ResultLimit() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestBraveEndpoint(t *testing.T) {
	if got := (config.Brave{}).Endpoint(); got != config.DefaultBraveBaseURL {
		t.Fatalf("Endpoint() unset = %q, want %q", got, config.DefaultBraveBaseURL)
	}
	custom := "https://proxy.internal/brave"
	if got := (config.Brave{BaseURL: custom}).Endpoint(); got != custom {
		t.Fatalf("Endpoint() explicit = %q, want %q", got, custom)
	}
}

func TestSearchValidateAcceptsGoodConfig(t *testing.T) {
	tests := []config.Search{
		{},
		{Provider: "brave", Brave: config.Brave{APIKey: "env:BRAVE_API_KEY"}},
		{Provider: "searxng", SearXNG: config.SearXNG{BaseURL: "https://searxng.internal"}},
	}
	for i, s := range tests {
		dir := t.TempDir()
		path := config.DefaultPath(dir)
		if err := config.Save(path, config.Config{Search: s}); err != nil {
			t.Fatalf("Save[%d]: %v", i, err)
		}
		if _, err := config.Load(path); err != nil {
			t.Fatalf("Load[%d]: want valid config to load cleanly, got %v", i, err)
		}
	}
}

func TestSearchValidateRejectsViolations(t *testing.T) {
	tests := []struct {
		name    string
		in      config.Search
		wantErr string
	}{
		{
			"searxng selected with no base_url",
			config.Search{Provider: "searxng"},
			"searxng selected but searxng.base_url",
		},
		{
			"brave selected with no api_key",
			config.Search{Provider: "brave"},
			"brave selected but brave.api_key",
		},
		{
			"brave api_key inlined instead of referenced",
			config.Search{Provider: "brave", Brave: config.Brave{APIKey: "sk-inline-token"}},
			"secrets are referenced, never inlined",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := config.DefaultPath(dir)
			if err := config.Save(path, config.Config{Search: tt.in}); err != nil {
				t.Fatalf("Save: %v", err)
			}
			_, err := config.Load(path)
			if err == nil {
				t.Fatalf("Load: want error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}
