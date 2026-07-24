package tui

// thinking_test.go covers the turn-in-flight thinking indicator (model.go): the
// turnActive flag Ingest tracks off TurnStarted/TurnFinished, and the
// WithThinking gate that renders the muted "⋯ working…" tail row IFF a turn is
// in flight AND nothing is pending. The pending-suppression case is the
// load-bearing one — an approval commandeers the footer as "awaiting you", the
// opposite of "working" — so it is pinned both as a field check and end to end
// through View. White-box (package tui) because turnActive/pending and the
// itemThinking kind are unexported.

import (
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// TestIngestTracksTurnActive pins the flag's lifecycle: TurnStarted raises it,
// TurnFinished clears it, and a SessionError clears it too (an errored turn is
// not "working", and it may emit no TurnFinished).
func TestIngestTracksTurnActive(t *testing.T) {
	const s = "sess-x"

	m := New(theme.Test())
	if m.turnActive {
		t.Fatal("a fresh Model is turnActive; want idle")
	}

	m = m.Ingest(event.NewTurnStarted(s))
	if !m.turnActive {
		t.Error("TurnStarted did not set turnActive")
	}

	m = m.Ingest(event.NewTurnFinished(s, "end_turn", provider.Usage{}))
	if m.turnActive {
		t.Error("TurnFinished did not clear turnActive")
	}

	// A turn that errors with no TurnFinished must still clear the flag.
	m = m.Ingest(event.NewTurnStarted(s)).Ingest(event.NewSessionError(s, "boom", false))
	if m.turnActive {
		t.Error("SessionError did not clear turnActive — the indicator would stick after a failure")
	}
}

// TestWithThinkingGate is the load-bearing gate: the indicator appears IFF a
// turn is in flight AND no approval/decision is pending. Each case is checked
// both as an item-count delta and end to end through the rendered frame, so
// neutralizing the gate in either direction — showing it when idle/pending, or
// hiding it mid-turn — goes red.
func TestWithThinkingGate(t *testing.T) {
	const s = "sess-x"
	active := func() Model { return New(theme.Test()).Ingest(event.NewTurnStarted(s)) }

	tests := []struct {
		name string
		m    Model
		want bool // want the indicator
	}{
		{"idle: no indicator", New(theme.Test()), false},
		{"turn in flight: indicator", active(), true},
		{
			// The named load-bearing case: a turn IS in flight (the gated call is
			// mid-flight), but an approval prompt owns the footer — "awaiting you",
			// not "working" — so the indicator must be suppressed.
			"turn in flight + pending approval: suppressed",
			active().Ingest(event.NewPermissionRequested(s, "perm-1", "bash", nil, nil)),
			false,
		},
		{
			"turn in flight + pending decision: suppressed",
			func() Model { m := active(); m.pendingDec = &pendingDecision{id: "d1", session: s}; return m }(),
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(tt.m.items)
			got := tt.m.WithThinking()

			gotItem := len(got.items) == before+1
			if gotItem != tt.want {
				t.Errorf("WithThinking appended-item = %v, want %v", gotItem, tt.want)
			}

			view := got.View(testkit.Width, testkit.Height)
			if hasLine := strings.Contains(view, "working…"); hasLine != tt.want {
				t.Errorf("rendered `working…` = %v, want %v:\n%s", hasLine, tt.want, view)
			}
		})
	}
}
