package config_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jedwards1230/gofer/internal/config"
)

// TestSubagentsZeroValueIsDisabled pins the polarity the whole opt-in rests on.
// Every other section in this package resolves its zero value to a WORKING
// default; this one must resolve to "the feature does not exist", or an upgrade
// would hand every session a tool its operator never asked for.
func TestSubagentsZeroValueIsDisabled(t *testing.T) {
	if (config.Subagents{}).IsEnabled() {
		t.Fatal("zero config.Subagents is enabled — subagents must be opt-in")
	}
	if (config.Config{}).Subagents.IsEnabled() {
		t.Fatal("zero config.Config enables subagents")
	}
}

// TestSubagentsSectionIsEmptyWhenUnconfigured pins that an untouched section
// serializes with no KEYS — `"subagents":{}` and nothing inside it. (The
// section object itself is always present: encoding/json's omitempty has never
// applied to structs, which is why every sibling section shows up too.) It
// matters because `"enabled": false` written into a file an operator never
// edited reads as a deliberate decision rather than a default.
func TestSubagentsSectionIsEmptyWhenUnconfigured(t *testing.T) {
	raw, err := json.Marshal(config.Config{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"subagents":{}`) {
		t.Fatalf("unconfigured subagents section is not empty: %s", raw)
	}
}

// TestSubagentsRoundTrip covers the section through Save/Load's own encoding.
func TestSubagentsRoundTrip(t *testing.T) {
	want := config.Subagents{Enabled: true, Agents: []string{"go-developer", "go-reviewer"}}
	raw, err := json.Marshal(config.Config{Subagents: want})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got config.Config
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Subagents.IsEnabled() {
		t.Error("Enabled did not round-trip")
	}
	if strings.Join(got.Subagents.AgentNames(), ",") != "go-developer,go-reviewer" {
		t.Errorf("Agents = %v, want the configured order", got.Subagents.AgentNames())
	}
}

// TestAgentNames covers the normalization the tool's schema enum depends on:
// blanks and duplicates out, order kept, and nil (never an empty slice) when
// nothing survives — see [config.Subagents.AgentNames] for why the distinction
// matters.
func TestAgentNames(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nil stays nil", nil, nil},
		{"all-blank collapses to nil", []string{"", "   "}, nil},
		{"trims and dedupes, order preserved", []string{" b ", "a", "b", "a"}, []string{"b", "a"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := config.Subagents{Agents: tc.in}.AgentNames()
			if tc.want == nil && got != nil {
				t.Fatalf("AgentNames() = %v, want nil", got)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("AgentNames() = %v, want %v", got, tc.want)
			}
		})
	}
}
