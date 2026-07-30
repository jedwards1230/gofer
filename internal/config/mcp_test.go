package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/gofer/internal/config"
)

// TestMCPServerTransportFailSafe pins the fail-safe polarity: an
// unrecognized explicit transport must resolve to Unsupported, never to a
// transport that happens to be wired up, even when Command or URL is also
// set.
func TestMCPServerTransportFailSafe(t *testing.T) {
	tests := []struct {
		name string
		in   config.MCPServer
		want config.MCPTransport
	}{
		{"unset with a command infers stdio", config.MCPServer{Command: "npx"}, config.MCPTransportStdio},
		{"unset with a url infers http", config.MCPServer{URL: "https://x"}, config.MCPTransportHTTP},
		{"unset with neither is unsupported", config.MCPServer{}, config.MCPTransportUnsupported},
		{"explicit stdio wins", config.MCPServer{TransportMode: "stdio"}, config.MCPTransportStdio},
		{"explicit http wins", config.MCPServer{TransportMode: "http"}, config.MCPTransportHTTP},
		{
			"unrecognized explicit value fails safe to unsupported, even with a command set",
			config.MCPServer{TransportMode: "websocket", Command: "npx"},
			config.MCPTransportUnsupported,
		},
		{
			"a typo fails safe to unsupported",
			config.MCPServer{TransportMode: "sdtio"},
			config.MCPTransportUnsupported,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.Transport(); got != tt.want {
				t.Errorf("Transport() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestMCPServerIsEnabled(t *testing.T) {
	yes, no := true, false
	tests := []struct {
		name string
		in   *bool
		want bool
	}{
		{"unset defaults to enabled", nil, true},
		{"explicit true", &yes, true},
		{"explicit false", &no, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (config.MCPServer{Enabled: tt.in}).IsEnabled(); got != tt.want {
				t.Errorf("IsEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMCPEnabledServersSkipsDisabledAndUnsupported(t *testing.T) {
	no := false
	m := config.MCP{Servers: []config.MCPServer{
		{Name: "a", Command: "npx"},
		{Name: "b", Command: "npx", Enabled: &no},
		{Name: "c", TransportMode: "carrier-pigeon"},
	}}

	got := m.EnabledServers()
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("EnabledServers() = %+v, want only server %q", got, "a")
	}
}

func TestMCPTimeoutResolvers(t *testing.T) {
	ms := func(v int) *int { return &v }

	if got := (config.MCP{}).ConnectTimeout(); got != config.DefaultMCPConnectTimeout {
		t.Errorf("ConnectTimeout() unset = %s, want %s", got, config.DefaultMCPConnectTimeout)
	}
	if got := (config.MCP{ConnectTimeoutMS: ms(1500)}).ConnectTimeout(); got != 1500*time.Millisecond {
		t.Errorf("ConnectTimeout() explicit = %s, want 1500ms", got)
	}
	if got := (config.MCP{ConnectTimeoutMS: ms(-1)}).ConnectTimeout(); got != config.DefaultMCPConnectTimeout {
		t.Errorf("ConnectTimeout() negative = %s, want default", got)
	}

	if got := (config.MCP{}).CallTimeout(); got != config.DefaultMCPCallTimeout {
		t.Errorf("CallTimeout() unset = %s, want %s", got, config.DefaultMCPCallTimeout)
	}
	if got := (config.MCP{}).ReadyTimeout(); got != config.DefaultMCPReadyTimeout {
		t.Errorf("ReadyTimeout() unset = %s, want %s", got, config.DefaultMCPReadyTimeout)
	}
	if got := (config.MCP{}).RetryMaxInterval(); got != config.DefaultMCPRetryMaxInterval {
		t.Errorf("RetryMaxInterval() unset = %s, want %s", got, config.DefaultMCPRetryMaxInterval)
	}
}

// TestMCPServerTimeoutOverride pins the per-server override precedence: a
// positive per-server value wins, else the section default applies.
func TestMCPServerTimeoutOverride(t *testing.T) {
	ms := func(v int) *int { return &v }
	section := config.MCP{ConnectTimeoutMS: ms(5000), CallTimeoutMS: ms(9000)}

	overridden := config.MCPServer{ConnectTimeoutMS: ms(1000), CallTimeoutMS: ms(2000)}
	if got := overridden.ConnectTimeout(section); got != 1000*time.Millisecond {
		t.Errorf("ConnectTimeout(section) with override = %s, want 1000ms", got)
	}
	if got := overridden.CallTimeout(section); got != 2000*time.Millisecond {
		t.Errorf("CallTimeout(section) with override = %s, want 2000ms", got)
	}

	fallthroughSrv := config.MCPServer{}
	if got := fallthroughSrv.ConnectTimeout(section); got != 5000*time.Millisecond {
		t.Errorf("ConnectTimeout(section) fallthrough = %s, want section default 5000ms", got)
	}
	if got := fallthroughSrv.CallTimeout(section); got != 9000*time.Millisecond {
		t.Errorf("CallTimeout(section) fallthrough = %s, want section default 9000ms", got)
	}
}

func TestMCPValidateAcceptsGoodConfig(t *testing.T) {
	m := config.MCP{Servers: []config.MCPServer{
		{Name: "wiki", Command: "npx", Env: map[string]config.SecretRef{"TOKEN": "env:WIKI_TOKEN"}},
		{Name: "grafana-01", URL: "https://x", Auth: "file:/run/secrets/grafana"},
	}}
	if _, err := config.Load(writeMCPConfig(t, m)); err != nil {
		t.Fatalf("Load: want a valid config to load cleanly, got %v", err)
	}
}

func TestMCPValidateRejectsViolations(t *testing.T) {
	tests := []struct {
		name    string
		servers []config.MCPServer
		wantErr string
	}{
		{
			"empty name",
			[]config.MCPServer{{Command: "npx"}},
			"name is required",
		},
		{
			"invalid name characters",
			[]config.MCPServer{{Name: "My Server!", Command: "npx"}},
			"must match",
		},
		{
			"duplicate name",
			[]config.MCPServer{{Name: "wiki", Command: "npx"}, {Name: "wiki", URL: "https://x"}},
			"duplicate server name",
		},
		{
			"both command and url",
			[]config.MCPServer{{Name: "wiki", Command: "npx", URL: "https://x"}},
			"mutually exclusive",
		},
		{
			"inline secret in auth",
			[]config.MCPServer{{Name: "wiki", URL: "https://x", Auth: "sk-inlined-token"}},
			"secrets are referenced, never inlined",
		},
		{
			"inline secret in env",
			[]config.MCPServer{{Name: "wiki", Command: "npx", Env: map[string]config.SecretRef{"TOKEN": "sk-inlined"}}},
			"secrets are referenced, never inlined",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := config.MCP{Servers: tt.servers}
			_, err := config.Load(writeMCPConfig(t, m))
			if err == nil {
				t.Fatalf("Load: want error containing %q, got nil", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Load error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

// writeMCPConfig marshals a Config carrying m and writes it to a temp
// config.json, returning the path for config.Load.
func writeMCPConfig(t *testing.T, m config.MCP) string {
	t.Helper()
	dir := t.TempDir()
	path := config.DefaultPath(dir)
	// Save doesn't validate; Load does — write via Save then read back
	// through Load so validate() actually runs.
	if err := config.Save(path, config.Config{MCP: m}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	return path
}
