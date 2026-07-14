package layout

import "testing"

// TestTopPaddingNonNegative pins the frame top-padding invariant: it must never
// be negative, or render() would shrink the content budget below the frame.
func TestTopPaddingNonNegative(t *testing.T) {
	if TopPadding < 0 {
		t.Errorf("TopPadding = %d; want >= 0", TopPadding)
	}
}
