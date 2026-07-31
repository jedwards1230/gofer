package tui

// interrupt_test.go pins the reframe of a user interrupt in the transcript: a
// turn the user deliberately stops (Esc / session/cancel) cancels the turn ctx,
// which the SDK surfaces as a context-cancellation session.error. That is a
// stop, not a failure, so it must render as the muted "stopped" indicator
// (itemInterrupted) and NEVER as a red itemError leaking the raw
// "context canceled" Go string. White-box (package tui) because the item kinds
// and the classifier are unexported.

import (
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

func TestSessionErrorUserCancelReframed(t *testing.T) {
	const s = "sess-x"

	tests := []struct {
		name     string
		errText  string
		wantKind itemKind
	}{
		{"bare cancel (cancelled between model calls — the ask_user case)", "context canceled", itemInterrupted},
		{"provider-wrapped cancel (a model call was in flight)", "openai: request: context canceled", itemInterrupted},
		{"a genuine failure stays an error", "openai: http 400: bad request", itemError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := ingested(theme.Test(),
				event.NewTurnStarted(s),
				event.NewSessionError(s, tc.errText, true))

			if len(m.items) == 0 {
				t.Fatal("no transcript item produced")
			}
			last := m.items[len(m.items)-1]
			if last.kind != tc.wantKind {
				t.Fatalf("item kind = %d, want %d", last.kind, tc.wantKind)
			}

			frame := testkit.Render(m, testkit.Width, testkit.Height)
			switch tc.wantKind {
			case itemInterrupted:
				if !strings.Contains(frame, "stopped") {
					t.Errorf("interrupted turn did not render the muted \"stopped\" indicator; frame:\n%s", frame)
				}
				if strings.Contains(frame, "context canceled") {
					t.Errorf("interrupted turn leaked the raw \"context canceled\" error into the transcript; frame:\n%s", frame)
				}
			case itemError:
				if !strings.Contains(frame, tc.errText) {
					t.Errorf("genuine error did not render its message %q; frame:\n%s", tc.errText, frame)
				}
			}
		})
	}
}

func TestIsUserCancel(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"context canceled", true},
		{"openai: request: context canceled", true},
		{"", false},
		{"boom", false},
		{"openai: http 400: bad request", false},
		{"context deadline exceeded", false}, // a timeout is a failure, not a user stop
	} {
		if got := isUserCancel(tc.in); got != tc.want {
			t.Errorf("isUserCancel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}
