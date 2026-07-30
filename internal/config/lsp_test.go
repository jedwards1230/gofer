package config_test

import (
	"testing"
	"time"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/lspdiag"
)

func TestLSPIsEnabled(t *testing.T) {
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
			if got := (config.LSP{Enabled: tt.in}).IsEnabled(); got != tt.want {
				t.Errorf("IsEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLSPTimeout(t *testing.T) {
	ms := func(v int) *int { return &v }
	tests := []struct {
		name string
		in   *int
		want time.Duration
	}{
		{"unset resolves to default", nil, config.DefaultLSPTimeout},
		{"zero resolves to default", ms(0), config.DefaultLSPTimeout},
		{"negative resolves to default", ms(-1), config.DefaultLSPTimeout},
		{"explicit value wins", ms(1000), time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (config.LSP{TimeoutMS: tt.in}).Timeout(); got != tt.want {
				t.Fatalf("Timeout() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestLSPDiagnosticLimit(t *testing.T) {
	n := func(v int) *int { return &v }
	tests := []struct {
		name string
		in   *int
		want int
	}{
		{"unset resolves to default", nil, config.DefaultLSPMaxDiagnostics},
		{"zero resolves to default", n(0), config.DefaultLSPMaxDiagnostics},
		{"negative resolves to default", n(-1), config.DefaultLSPMaxDiagnostics},
		{"explicit value wins", n(25), 25},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (config.LSP{MaxDiagnostics: tt.in}).DiagnosticLimit(); got != tt.want {
				t.Fatalf("DiagnosticLimit() = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestLSPZeroValueIsEnabled pins the section-level default-on posture: an
// unconfigured gofer runs LSP diagnostics, unlike the opt-in posture of
// e.g. search or index-mode tool schemas — see [config.LSP]'s doc for why
// this asymmetry is safe (the layer degrades silently with no server on
// PATH).
func TestLSPZeroValueIsEnabled(t *testing.T) {
	if !(config.Config{}).LSP.IsEnabled() {
		t.Fatal("zero Config LSP.IsEnabled() = false, want true")
	}
}

// TestLSPDefaultsMatchLspdiag guards against silent drift between this
// package's LSP defaults and internal/lspdiag's own zero-value fallbacks
// (DefaultTimeout / DefaultMaxDiagnostics). The two packages cannot share one
// constant — this section landed the config SHAPE before internal/lspdiag
// existed on this branch (see [LSP]'s doc) — so they are two independent
// sources of truth that a doc comment alone cannot keep in sync: nothing
// stops one from changing without the other. If this test ever fails, an
// operator's config.json (e.g. an explicit lsp.timeout_ms matching what they
// believe is "the default") and gofer's actual runtime behavior have quietly
// diverged — fix the constant that moved, don't adjust this test to match.
func TestLSPDefaultsMatchLspdiag(t *testing.T) {
	if config.DefaultLSPTimeout != lspdiag.DefaultTimeout {
		t.Errorf("config.DefaultLSPTimeout = %s, lspdiag.DefaultTimeout = %s — these are two independent constants (config predates lspdiag on this branch) and MUST agree, or config.json's documented default silently disagrees with actual diagnostics behavior",
			config.DefaultLSPTimeout, lspdiag.DefaultTimeout)
	}
	if config.DefaultLSPMaxDiagnostics != lspdiag.DefaultMaxDiagnostics {
		t.Errorf("config.DefaultLSPMaxDiagnostics = %d, lspdiag.DefaultMaxDiagnostics = %d — these are two independent constants (config predates lspdiag on this branch) and MUST agree, or config.json's documented default silently disagrees with actual diagnostics behavior",
			config.DefaultLSPMaxDiagnostics, lspdiag.DefaultMaxDiagnostics)
	}
}
