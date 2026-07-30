package supervisor

import (
	"testing"

	"github.com/jedwards1230/agent-sdk-go/provider"
)

// TestShouldAutoCompact is the table-driven pin for the trigger's pure
// decision function: under/at/over the threshold, plus the unknown-window
// and cache-token cases. See [maybeAutoCompact]'s doc for why this reads
// InputTokens + CacheReadTokens and never divides by an unknown window.
func TestShouldAutoCompact(t *testing.T) {
	tests := []struct {
		name          string
		usage         provider.Usage
		contextWindow int
		threshold     float64
		want          bool
	}{
		{"well under threshold", provider.Usage{InputTokens: 10_000}, 200_000, 0.85, false},
		{"just under threshold", provider.Usage{InputTokens: 169_999}, 200_000, 0.85, false},
		{"exactly at threshold triggers", provider.Usage{InputTokens: 170_000}, 200_000, 0.85, true},
		{"over threshold", provider.Usage{InputTokens: 190_000}, 200_000, 0.85, true},
		{"cache-read tokens count toward the footprint", provider.Usage{InputTokens: 90_000, CacheReadTokens: 80_000}, 200_000, 0.85, true},
		{"cache-write tokens do NOT count", provider.Usage{InputTokens: 10_000, CacheWriteTokens: 190_000}, 200_000, 0.85, false},
		{"unknown context window never triggers, however large usage is", provider.Usage{InputTokens: 10_000_000}, 0, 0.85, false},
		{"negative context window never triggers", provider.Usage{InputTokens: 10_000_000}, -1, 0.85, false},
		{"zero usage never triggers", provider.Usage{}, 200_000, 0.85, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldAutoCompact(tt.usage, tt.contextWindow, tt.threshold)
			if got != tt.want {
				t.Errorf("shouldAutoCompact(%+v, %d, %v) = %v, want %v",
					tt.usage, tt.contextWindow, tt.threshold, got, tt.want)
			}
		})
	}
}
