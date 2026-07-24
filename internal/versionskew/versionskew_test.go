package versionskew

import "testing"

// TestClassify is the pure core: which (client, daemon) pairs are skewed, and
// how. It must never false-positive on an unknown.
func TestClassify(t *testing.T) {
	cases := []struct {
		name           string
		client, daemon string
		want           Kind
	}{
		{"equal releases are none", "v0.3.1", "v0.3.1", None},
		{"equal pseudo-versions are none", "v0.3.1-0.20260721163650-6661a1dcb818", "v0.3.1-0.20260721163650-6661a1dcb818", None},
		{"empty client is none (unknown)", "", "v0.3.1", None},
		{"empty daemon is none (unknown)", "v0.3.1", "", None},
		{"both empty is none", "", "", None},
		{"older release daemon is older", "v0.3.1", "v0.2.1", Older},
		{"newer release daemon is none (client is the stale side)", "v0.2.1", "v0.3.1", None},
		{
			// The exact scenario that triggered this work: a client built from a
			// later commit than the long-running daemon, both Go pseudo-versions.
			"older pseudo-version daemon is older",
			"v0.3.1-0.20260721163650-6661a1dcb818",
			"v0.2.1-0.20260719230853-2aa711248af7",
			Older,
		},
		{"newer pseudo-version daemon is none", "v0.2.1-0.20260719230853-2aa711248af7", "v0.3.1-0.20260721163650-6661a1dcb818", None},
		{"differing dev builds are differs (order unknown)", "dev-6661a1dcb818", "dev-2aa711248af7", Differs},
		{"release client vs dev daemon is differs", "v0.3.1", "dev-2aa711248af7", Differs},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.client, c.daemon); got != c.want {
				t.Errorf("Classify(%q, %q) = %s, want %s", c.client, c.daemon, got, c.want)
			}
		})
	}
}
