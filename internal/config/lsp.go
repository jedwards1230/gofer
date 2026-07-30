package config

import "time"

const (
	// DefaultLSPTimeout is [LSP.TimeoutMS]'s default: 4s, long enough for
	// gopls to publish diagnostics after a save and short enough that an
	// unresponsive server never visibly slows a tool call. MUST stay equal
	// to internal/lspdiag's own DefaultTimeout — two independent constants,
	// checked by internal/config's TestLSPDefaultsMatchLspdiag so a drift
	// fails loudly instead of silently disagreeing with runtime behavior.
	DefaultLSPTimeout = 4 * time.Second
	// DefaultLSPMaxDiagnostics is [LSP.MaxDiagnostics]'s default: 10. The
	// first handful of errors are what the model must act on; the rest is
	// context spend on noise. MUST stay equal to internal/lspdiag's own
	// DefaultMaxDiagnostics — same note as above.
	DefaultLSPMaxDiagnostics = 10
)

// LSP configures gofer's language-server diagnostics integration. The zero
// value is fully valid and ON: [LSP.IsEnabled] defaults to true, because the
// layer degrades silently when no server is on PATH for a given language —
// there is no failure mode a default-off posture would protect against, only
// a capability an operator would otherwise have to remember to turn on.
//
// Enabled/TimeoutMS/MaxDiagnostics are read by internal/supervisor's session
// wiring (see [LSP.IsEnabled], [LSP.Timeout], [LSP.DiagnosticLimit]). Servers
// remains a forward-compatible stub: internal/lspdiag owns the per-language
// server launch details and defines its own zero-value fallbacks over this
// shape, but does not yet consume an operator override here.
type LSP struct {
	// Enabled defaults to true (nil or true); see [LSP.IsEnabled].
	Enabled *bool `json:"enabled,omitempty"`
	// TimeoutMS bounds one diagnostics fetch; see [LSP.Timeout].
	TimeoutMS *int `json:"timeout_ms,omitempty"`
	// MaxDiagnostics caps how many diagnostic lines reach context; see
	// [LSP.DiagnosticLimit].
	MaxDiagnostics *int `json:"max_diagnostics,omitempty"`
	// Servers overrides the built-in per-language server table, keyed by
	// LSP languageId (e.g. "go", "python"). See [LSPServer].
	Servers map[string]LSPServer `json:"servers,omitempty"`
}

// LSPServer overrides one language's server: whether it runs at all, and/or
// its launch command. The owning workstream defines how an empty Command
// falls back to its own built-in table.
type LSPServer struct {
	Enabled *bool    `json:"enabled,omitempty"`
	Command []string `json:"command,omitempty"`
}

// IsEnabled resolves [LSP.Enabled]'s effective value: true (the default)
// when nil, else the explicit stored value.
func (l LSP) IsEnabled() bool {
	return l.Enabled == nil || *l.Enabled
}

// Timeout resolves [LSP.TimeoutMS]'s effective value: [DefaultLSPTimeout]
// when unset or non-positive.
func (l LSP) Timeout() time.Duration {
	if l.TimeoutMS == nil || *l.TimeoutMS <= 0 {
		return DefaultLSPTimeout
	}
	return time.Duration(*l.TimeoutMS) * time.Millisecond
}

// DiagnosticLimit resolves [LSP.MaxDiagnostics]'s effective value:
// [DefaultLSPMaxDiagnostics] when unset or non-positive.
func (l LSP) DiagnosticLimit() int {
	if l.MaxDiagnostics == nil || *l.MaxDiagnostics <= 0 {
		return DefaultLSPMaxDiagnostics
	}
	return *l.MaxDiagnostics
}
