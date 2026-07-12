package render_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/render"
)

const sid = "0192a1b2-c3d4-7e5f-8a90-000000000001"

// fauxTurn is the event sequence a session driven by the SDK faux provider
// emits for one turn: reasoning, then text, bracketed by lifecycle events.
func fauxTurn() []event.Event {
	return []event.Event{
		event.NewSessionCreated(sid),
		event.NewTurnStarted(sid),
		event.NewMessageStarted(sid, event.MessageReasoning),
		event.NewMessageDelta(sid, event.MessageReasoning, "The user said hello. "),
		event.NewMessageDelta(sid, event.MessageReasoning, "I'll greet them back."),
		event.NewMessageFinished(sid, event.MessageReasoning, "The user said hello. I'll greet them back."),
		event.NewMessageStarted(sid, event.MessageText),
		event.NewMessageDelta(sid, event.MessageText, "Hello"),
		event.NewMessageDelta(sid, event.MessageText, "! How can "),
		event.NewMessageDelta(sid, event.MessageText, "I help you today?"),
		event.NewMessageFinished(sid, event.MessageText, "Hello! How can I help you today?"),
		event.NewTurnFinished(sid, "end_turn", provider.Usage{InputTokens: 9, OutputTokens: 7}),
	}
}

func renderAll(t *testing.T, r render.Renderer, events []event.Event) {
	t.Helper()
	for _, e := range events {
		if err := r.Render(e); err != nil {
			t.Fatalf("Render(%s): %v", e.Kind(), err)
		}
	}
}

func TestHuman(t *testing.T) {
	tests := []struct {
		name   string
		events []event.Event
		want   string
	}{
		{
			name:   "faux turn",
			events: fauxTurn(),
			want: "· session.created\n" +
				"· turn.started\n" +
				"» The user said hello. I'll greet them back.\n" +
				"Hello! How can I help you today?\n" +
				"· turn.finished  stop=end_turn  usage=9in/7out\n",
		},
		{
			name: "tool and permission markers",
			events: []event.Event{
				event.NewToolCallStarted(sid, "call-1", "bash", nil),
				event.NewToolCallFinished(sid, "call-1", "ok", false, nil),
				event.NewPermissionRequested(sid, "perm-1", "bash", nil, nil),
				event.NewPermissionResolved(sid, "perm-1", event.VerdictAllow, "rule-42"),
			},
			want: "· tool.call.started  bash (call-1)\n" +
				"· tool.call.finished  call-1\n" +
				"· permission.requested  bash (perm-1)\n" +
				"· permission.resolved  perm-1 → allow\n",
		},
		{
			name:   "session error",
			events: []event.Event{event.NewSessionError(sid, "boom", true)},
			want:   "· session.error  boom\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			renderAll(t, render.NewHuman(&buf, false), tt.events)
			if got := buf.String(); got != tt.want {
				t.Errorf("output mismatch\n got: %q\nwant: %q", got, tt.want)
			}
		})
	}
}

// TestHumanColor asserts reasoning is wrapped in ANSI dim codes when color is
// on, and raw text is not.
func TestHumanColor(t *testing.T) {
	var buf bytes.Buffer
	renderAll(t, render.NewHuman(&buf, true), fauxTurn())
	got := buf.String()

	if !strings.Contains(got, ansiDim+"» "+ansiReset) {
		t.Errorf("reasoning prefix not dimmed: %q", got)
	}
	if !strings.Contains(got, ansiDim+"The user said hello. "+ansiReset) {
		t.Errorf("reasoning delta not dimmed: %q", got)
	}
	if strings.Contains(got, ansiDim+"Hello") {
		t.Errorf("assistant text should not be dimmed: %q", got)
	}
}

// ANSI codes duplicated from the package under test so the color assertions read
// against a known constant rather than a magic string.
const (
	ansiDim   = "\x1b[2m"
	ansiReset = "\x1b[0m"
)

func TestJSONL(t *testing.T) {
	var buf bytes.Buffer
	events := []event.Event{
		event.NewMessageDelta(sid, event.MessageText, "Hello"),
		event.NewTurnFinished(sid, "end_turn", provider.Usage{InputTokens: 9, OutputTokens: 7}),
	}
	renderAll(t, render.NewJSONL(&buf), events)

	want := `{"type":"message.delta","session_id":"` + sid + `","seq":0,"kind":"text","text":"Hello"}` + "\n" +
		`{"type":"turn.finished","session_id":"` + sid + `","seq":0,"stop_reason":"end_turn","usage":{"input_tokens":9,"output_tokens":7}}` + "\n"
	if got := buf.String(); got != want {
		t.Errorf("JSONL mismatch\n got: %s\nwant: %s", got, want)
	}
}
