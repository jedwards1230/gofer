package modelmeta_test

import (
	"testing"

	"github.com/jedwards1230/gofer/internal/modelmeta"
)

// TestDisplayNameKnown spot-checks a labeled model id resolves to its short
// friendly name.
func TestDisplayNameKnown(t *testing.T) {
	if got := modelmeta.DisplayName("claude-sonnet-5"); got != "Sonnet 5" {
		t.Fatalf("DisplayName(claude-sonnet-5) = %q, want %q", got, "Sonnet 5")
	}
}

// TestDisplayNameFallsBackToID covers a model id absent from the table (a
// newly registered SDK model gofer hasn't labeled yet): it falls back to the
// raw id rather than an empty name.
func TestDisplayNameFallsBackToID(t *testing.T) {
	if got := modelmeta.DisplayName("some-future-model"); got != "some-future-model" {
		t.Fatalf("DisplayName(unregistered) = %q, want the raw id", got)
	}
}
