package config_test

// schema_test.go covers the M7 Round A+B config sections as a WHOLE: the
// round trip through Save/Load with every section populated, and that the
// zero Config still resolves every field to its documented default.

import (
	"reflect"
	"testing"

	"github.com/jedwards1230/gofer/internal/config"
)

// TestSchemaSectionsRoundTrip pins that a Config with all six new sections
// populated marshals and unmarshals unchanged — the same contract
// [TestSaveLoadRoundTrip] already pins for the M3/M4 sections, extended to
// the M7 ones.
func TestSchemaSectionsRoundTrip(t *testing.T) {
	n := func(v int) *int { return &v }
	enabled := true

	want := config.Config{
		Prompt: config.Prompt{
			Files:              []string{"AGENTS.md", "CLAUDE.md"},
			MissingFileIsError: true,
			MaxFileBytes:       n(1024),
		},
		Tools: config.Tools{
			SchemaMode:     "index",
			Resident:       []string{"bash", "read"},
			SummaryBytes:   n(200),
			SearchResults:  n(15),
			InlineIndexMax: n(30),
		},
		MCP: config.MCP{
			Servers: []config.MCPServer{
				{
					Name:          "wiki",
					TransportMode: "stdio",
					Enabled:       &enabled,
					Command:       "npx",
					Args:          []string{"-y", "wiki-mcp"},
					Env:           map[string]config.SecretRef{"TOKEN": "env:WIKI_TOKEN"},
					Allow:         []string{"read_*"},
				},
				{
					Name:    "grafana",
					URL:     "https://grafana.internal/mcp",
					Headers: map[string]config.SecretRef{"Authorization": "file:/run/secrets/grafana"},
					Auth:    "env:GRAFANA_TOKEN",
					Deny:    []string{"delete_*"},
				},
			},
			ConnectTimeoutMS:   n(5000),
			CallTimeoutMS:      n(10000),
			ReadyTimeoutMS:     n(1000),
			RetryMaxIntervalMS: n(30000),
		},
		Search: config.Search{
			Provider:   "brave",
			Brave:      config.Brave{APIKey: "env:BRAVE_API_KEY", BaseURL: "https://proxy.internal"},
			SearXNG:    config.SearXNG{BaseURL: "https://searxng.internal", APIKey: "file:/run/secrets/searxng"},
			TimeoutMS:  n(5000),
			MaxResults: n(8),
		},
		Skills: config.Skills{
			Dirs:             []string{"/opt/skills"},
			Disabled:         []string{"experimental"},
			MaxFileBytes:     n(2048),
			DescriptionBytes: n(400),
		},
		LSP: config.LSP{
			Enabled:        &enabled,
			TimeoutMS:      n(3000),
			MaxDiagnostics: n(20),
			Servers: map[string]config.LSPServer{
				"go": {Enabled: &enabled, Command: []string{"gopls"}},
			},
		},
	}

	path := config.DefaultPath(t.TempDir())
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

// TestSchemaSectionsZeroValueResolvesEveryDefault pins that Config{} is
// valid and every M7 section resolves to its documented default — the
// "zero value is fully valid" contract every section's doc comment claims.
func TestSchemaSectionsZeroValueResolvesEveryDefault(t *testing.T) {
	c := config.Config{}
	if err := (func() error { _, err := config.Load(config.DefaultPath(t.TempDir())); return err })(); err != nil {
		t.Fatalf("Load(missing): %v", err)
	}

	if got := c.Prompt.Sources(); len(got) != 1 || got[0] != config.DefaultPromptAsset {
		t.Errorf("Prompt.Sources() = %v, want [%s]", got, config.DefaultPromptAsset)
	}
	if got := c.Prompt.FileLimitBytes(); got != config.DefaultPromptMaxFileBytes {
		t.Errorf("Prompt.FileLimitBytes() = %d, want %d", got, config.DefaultPromptMaxFileBytes)
	}

	if got := c.Tools.Schemas(); got != config.ToolSchemaModePreload {
		t.Errorf("Tools.Schemas() = %q, want preload", got)
	}
	if got := c.Tools.ResidentTools(); len(got) != len(config.DefaultResidentTools) {
		t.Errorf("Tools.ResidentTools() = %v, want %v", got, config.DefaultResidentTools)
	}
	if got := c.Tools.SummaryLimitBytes(); got != config.DefaultToolSummaryBytes {
		t.Errorf("Tools.SummaryLimitBytes() = %d, want %d", got, config.DefaultToolSummaryBytes)
	}
	if got := c.Tools.SearchResultLimit(); got != config.DefaultToolSearchResults {
		t.Errorf("Tools.SearchResultLimit() = %d, want %d", got, config.DefaultToolSearchResults)
	}
	if got := c.Tools.InlineIndexLimit(); got != config.DefaultToolInlineIndexMax {
		t.Errorf("Tools.InlineIndexLimit() = %d, want %d", got, config.DefaultToolInlineIndexMax)
	}

	if got := c.MCP.ConnectTimeout(); got != config.DefaultMCPConnectTimeout {
		t.Errorf("MCP.ConnectTimeout() = %s, want %s", got, config.DefaultMCPConnectTimeout)
	}
	if got := c.MCP.CallTimeout(); got != config.DefaultMCPCallTimeout {
		t.Errorf("MCP.CallTimeout() = %s, want %s", got, config.DefaultMCPCallTimeout)
	}
	if got := c.MCP.ReadyTimeout(); got != config.DefaultMCPReadyTimeout {
		t.Errorf("MCP.ReadyTimeout() = %s, want %s", got, config.DefaultMCPReadyTimeout)
	}
	if got := c.MCP.RetryMaxInterval(); got != config.DefaultMCPRetryMaxInterval {
		t.Errorf("MCP.RetryMaxInterval() = %s, want %s", got, config.DefaultMCPRetryMaxInterval)
	}
	if got := len(c.MCP.EnabledServers()); got != 0 {
		t.Errorf("MCP.EnabledServers() on zero value = %d servers, want 0", got)
	}

	if got := c.Search.Selected(); got != config.SearchProviderNone {
		t.Errorf("Search.Selected() = %q, want none", got)
	}
	if got := c.Search.Timeout(); got != config.DefaultSearchTimeout {
		t.Errorf("Search.Timeout() = %s, want %s", got, config.DefaultSearchTimeout)
	}
	if got := c.Search.ResultLimit(); got != config.DefaultSearchMaxResults {
		t.Errorf("Search.ResultLimit() = %d, want %d", got, config.DefaultSearchMaxResults)
	}
	if got := c.Search.Brave.Endpoint(); got != config.DefaultBraveBaseURL {
		t.Errorf("Search.Brave.Endpoint() = %q, want %q", got, config.DefaultBraveBaseURL)
	}

	if got := c.Skills.FileLimitBytes(); got != config.DefaultSkillMaxFileBytes {
		t.Errorf("Skills.FileLimitBytes() = %d, want %d", got, config.DefaultSkillMaxFileBytes)
	}
	if got := c.Skills.DescriptionLimitBytes(); got != config.DefaultSkillDescriptionBytes {
		t.Errorf("Skills.DescriptionLimitBytes() = %d, want %d", got, config.DefaultSkillDescriptionBytes)
	}
	if got := c.Skills.Directories("/root", "/cwd"); len(got) != 2 {
		t.Errorf("Skills.Directories() = %v, want 2 conventional dirs", got)
	}

	if !c.LSP.IsEnabled() {
		t.Error("LSP.IsEnabled() = false, want true (default on)")
	}
	if got := c.LSP.Timeout(); got != config.DefaultLSPTimeout {
		t.Errorf("LSP.Timeout() = %s, want %s", got, config.DefaultLSPTimeout)
	}
	if got := c.LSP.DiagnosticLimit(); got != config.DefaultLSPMaxDiagnostics {
		t.Errorf("LSP.DiagnosticLimit() = %d, want %d", got, config.DefaultLSPMaxDiagnostics)
	}
}
