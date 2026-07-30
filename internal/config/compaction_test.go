package config_test

import (
	"testing"

	"github.com/jedwards1230/gofer/internal/config"
)

func floatPtr(f float64) *float64 { return &f }

// TestCompactionThresholdResolution pins [Compaction.Threshold]'s fail-safe
// polarity: unset or anything outside (0, 1) exclusive resolves to
// [config.DefaultCompactionThreshold] rather than never-compact (0) or
// always-compact (>= 1).
func TestCompactionThresholdResolution(t *testing.T) {
	tests := []struct {
		name string
		in   *float64
		want float64
	}{
		{"unset resolves to default", nil, config.DefaultCompactionThreshold},
		{"zero resolves to default", floatPtr(0), config.DefaultCompactionThreshold},
		{"negative resolves to default", floatPtr(-0.5), config.DefaultCompactionThreshold},
		{"one resolves to default", floatPtr(1), config.DefaultCompactionThreshold},
		{"above one resolves to default", floatPtr(1.5), config.DefaultCompactionThreshold},
		{"a valid fraction is honored", floatPtr(0.5), 0.5},
		{"just under one is honored", floatPtr(0.99), 0.99},
		{"just above zero is honored", floatPtr(0.01), 0.01},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := (config.Compaction{ThresholdFraction: tt.in}).Threshold()
			if got != tt.want {
				t.Errorf("Compaction{ThresholdFraction: %v}.Threshold() = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// TestCompactionAutoEnabled pins the accessor's inverted polarity against
// Disabled: the zero value (Disabled: false) means auto-compaction is ON.
func TestCompactionAutoEnabled(t *testing.T) {
	if !(config.Compaction{}).AutoEnabled() {
		t.Error("zero-value Compaction.AutoEnabled() = false, want true (auto-compaction on by default)")
	}
	if (config.Compaction{Disabled: true}).AutoEnabled() {
		t.Error("Compaction{Disabled: true}.AutoEnabled() = true, want false")
	}
}
