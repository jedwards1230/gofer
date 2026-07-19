package tui

// modelprovider_test.go covers modelProvider, the TUI's client-side
// same-provider decision behind /model's Enter. It is a DECISION path, not a
// display one: a registry-membership check here reads an unregistered-but-
// runnable id as "unknown provider", which handleModelSelect treats as a
// mismatch — so swapping between two models of the same provider would decline
// the live swap and only set the new-session default.

import "testing"

func TestModelProvider(t *testing.T) {
	cases := []struct {
		name string
		id   string
		want string
	}{
		{name: "registered anthropic", id: "claude-sonnet-5", want: "anthropic"},
		{name: "registered openai", id: "gpt-5", want: "openai"},
		// The case this function exists to get right: newer than the registry,
		// still placeable by shape, so a same-provider swap stays available.
		{name: "unregistered anthropic by prefix", id: "claude-sonnet-9-future", want: "anthropic"},
		{name: "unregistered openai by prefix", id: "gpt-5.6", want: "openai"},
		{name: "unregistered openai reasoning family", id: "o5-mini", want: "openai"},
		// Genuinely unplaceable ids stay "", so handleModelSelect keeps
		// declining swaps it cannot reason about rather than guessing.
		{name: "unplaceable id", id: "no-such-model", want: ""},
		{name: "empty id is the no-override state", id: "", want: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := modelProvider(c.id); got != c.want {
				t.Errorf("modelProvider(%q) = %q, want %q", c.id, got, c.want)
			}
		})
	}
}
