package config_test

import (
	"testing"
	"time"

	"github.com/jedwards1230/gofer/internal/config"
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
