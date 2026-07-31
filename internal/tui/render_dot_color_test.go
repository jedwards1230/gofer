package tui

// render_dot_color_test.go pins the transcript's status-dot color grammar (the
// contract documented in docs/TUI.md): amber = in-progress, green = completed,
// red = error, and a dot FROZEN amber when its item was in flight at a user
// cancel (it never completed, so it must not flip green). White-box (package
// tui) because dotStyle, the item states, and runningToolInput are unexported.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// TestDotStyleGrammar is the direct state→color mapping table: every transcript
// item lifecycle state maps to exactly one marker color, uniform across
// assistant messages and tool calls.
func TestDotStyleGrammar(t *testing.T) {
	m := New(testkit.ColorTheme())
	probe := func(it item) string { return m.dotStyle(it).Render("●") } // ●

	cases := []struct {
		name string
		it   item
		want string // rendered through the expected theme style
	}{
		{"streaming message (not done) = amber", item{kind: itemAssistantText}, m.theme.WarnStyle().Render("●")},
		{"settled message (done ok) = green", item{kind: itemAssistantText, done: true}, m.theme.OKStyle().Render("●")},
		{"interrupted message freezes amber", item{kind: itemAssistantText, done: true, interrupted: true}, m.theme.WarnStyle().Render("●")},
		{"running tool = amber", item{kind: itemTool}, m.theme.WarnStyle().Render("●")},
		{"done tool ok = green", item{kind: itemTool, done: true}, m.theme.OKStyle().Render("●")},
		{"failed tool = red", item{kind: itemTool, done: true, toolErr: true}, m.theme.DangerStyle().Render("●")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := probe(tc.it); got != tc.want {
				t.Errorf("dotStyle color = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestInterruptFreezesAssistantDotAmber is the A1 regression: on a user cancel
// the SDK flushes the still-open streaming message to a MessageFinished BEFORE
// the cancel error, so gofer sees it done=true. Its dot must NOT go green — it
// was cut off, not completed — it freezes amber, while a NON-interrupted settled
// message of the same text is green.
func TestInterruptFreezesAssistantDotAmber(t *testing.T) {
	const s = "sess-x"
	const msg = "The shell command was requested, but"

	build := func(interrupt bool) string {
		m := New(testkit.ColorTheme())
		m.Ingest(event.NewTurnStarted(s))
		m.Ingest(event.NewMessageStarted(s, event.MessageText))
		m.Ingest(event.NewMessageDelta(s, event.MessageText, msg))
		m.Ingest(event.NewMessageFinished(s, event.MessageText, msg))
		if interrupt {
			// The cancel error the SDK surfaces after flushing the open message.
			m.Ingest(event.NewSessionError(s, "context canceled", true))
		}
		return testkit.TagANSI(t, testkit.Render(m, testkit.Width, testkit.Height))
	}

	// Control: a message that settled without interruption is green.
	if got := build(false); !strings.Contains(got, "<green>●</green>") {
		t.Errorf("a settled (non-interrupted) message did not render a green dot; frame:\n%s", got)
	}

	// Regression: the interrupted (cancel-flushed) message freezes amber and is
	// never green.
	tagged := build(true)
	if !strings.Contains(tagged, "<yellow>●</yellow>") {
		t.Errorf("interrupted message did not freeze its dot amber; frame:\n%s", tagged)
	}
	if strings.Contains(tagged, "<green>●</green>") {
		t.Errorf("interrupted message wrongly rendered a green (completed) dot; frame:\n%s", tagged)
	}
	// The separate muted "stopped" marker still appears.
	if !strings.Contains(tagged, "stopped") {
		t.Errorf("interrupted turn dropped the muted \"stopped\" marker; frame:\n%s", tagged)
	}
}

// TestRunningToolShowsStreamedInvocation is the A2 regression: while a tool runs,
// the header shows its FULL invocation reconstructed from the streaming argument
// deltas (a provider seeds ToolCallStarted with an empty {} and streams the real
// arguments), not a bare `bash` the user can't act on.
func TestRunningToolShowsStreamedInvocation(t *testing.T) {
	const s = "sess-x"
	m := New(theme.Test())
	m.Ingest(event.NewToolCallStarted(s, "call-1", "bash", json.RawMessage(`{}`)))
	m.Ingest(event.NewToolCallDelta(s, "call-1", `{"command":"sleep 15 `))
	m.Ingest(event.NewToolCallDelta(s, "call-1", `&& echo 'Waited 15 seconds.'"}`))

	frame := testkit.Render(m, testkit.Width, testkit.Height)
	if !strings.Contains(frame, "bash(sleep 15 && echo 'Waited 15 seconds.')") {
		t.Errorf("running tool did not show its full streamed invocation; frame:\n%s", frame)
	}

	// The running dot is amber.
	running := ingested(testkit.ColorTheme(),
		event.NewToolCallStarted(s, "call-1", "bash", json.RawMessage(`{}`)),
		event.NewToolCallDelta(s, "call-1", `{"command":"sleep 15"}`))
	tagged := testkit.TagANSI(t, testkit.Render(running, testkit.Width, testkit.Height))
	if !strings.Contains(tagged, "<yellow>●</yellow>") {
		t.Errorf("running tool dot was not amber; frame:\n%s", tagged)
	}
}

// TestRunningToolPartialArgsStayNameOnly guards the mid-stream case: a single
// incomplete-JSON fragment must NOT render as half-JSON — the header stays
// name-only until the accumulated arguments parse cleanly.
func TestRunningToolPartialArgsStayNameOnly(t *testing.T) {
	const s = "sess-x"
	m := New(theme.Test())
	m.Ingest(event.NewToolCallStarted(s, "call-1", "bash", json.RawMessage(`{}`)))
	m.Ingest(event.NewToolCallDelta(s, "call-1", `{"command":"sleep 15 `)) // unterminated JSON

	frame := testkit.Render(m, testkit.Width, testkit.Height)
	if strings.Contains(frame, "sleep 15") {
		t.Errorf("partial-JSON tool args leaked into the header before parsing cleanly; frame:\n%s", frame)
	}
	if !strings.Contains(frame, "● bash\n") && !strings.Contains(frame, "● bash ") {
		t.Errorf("running tool with only partial args did not fall back to a name-only header; frame:\n%s", frame)
	}
}
