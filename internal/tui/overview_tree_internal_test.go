package tui

import (
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// The tree roster's right column is the one place a one-cell formatting
// mistake silently overruns rowTallyW and truncates itself. A golden pins the
// typical values; these tables pin the unit BOUNDARIES, which is where an
// off-by-one in the switch actually lives.

func TestHumanElapsedBoundaries(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{59 * time.Second, "59s"},
		{time.Minute, "1m 0s"},
		{5*time.Minute + 9*time.Second, "5m 9s"},
		{9*time.Minute + 59*time.Second, "9m 59s"},
		{10 * time.Minute, "10m"}, // seconds drop here — the widest form is "9m 59s"
		{41*time.Minute + 40*time.Second, "41m"},
		{time.Hour, "1h 0m"},
		{25*time.Hour + 30*time.Minute, "1d 1h"},
	}
	for _, tc := range tests {
		if got := humanElapsed(tc.d); got != tc.want {
			t.Errorf("humanElapsed(%s) = %q; want %q", tc.d, got, tc.want)
		}
	}
}

func TestHumanTokensBoundaries(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1.0k"},
		{214700, "214.7k"},
		{999949, "999.9k"},
		{1_000_000, "1.0M"},
		{12_340_000, "12.3M"},
	}
	for _, tc := range tests {
		if got := humanTokens(tc.n); got != tc.want {
			t.Errorf("humanTokens(%d) = %q; want %q", tc.n, got, tc.want)
		}
	}
}

// TestTallyFitsItsColumn proves the widest realistic tally still fits
// rowTallyW, so the right column never truncates itself into "…tok…".
func TestTallyFitsItsColumn(t *testing.T) {
	s := SessionInfo{
		Created: time.Unix(0, 0),
		Updated: time.Unix(0, 0).Add(9*time.Minute + 59*time.Second),
	}
	s.Usage.InputTokens = 999_949
	got := Overview{}.tally(s)
	if w := ansi.StringWidth(got); w >= rowTallyW {
		t.Errorf("widest tally %q is %d cells; rowTallyW is %d, leaving no gap before the summary", got, w, rowTallyW)
	}
}
