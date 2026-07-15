package tui

// streaming_test.go reproduces and guards the daemon-attach streaming
// top-anchor bug: a real assistant reply streams in as MessageStarted +
// many MessageDelta events (never a single settled MessageFinished until
// the whole turn completes), and real LLM output is virtually always
// multi-line — paragraphs, lists, code blocks. Every test here drives App
// through that exact incremental path (never a pre-built/settled
// transcript) because that is what hid the bug from the earlier
// multi-turn-only scroll tests in app_internal_test.go: those build
// overflow out of many *settled*, single-line MessageFinished turns, which
// never exercises a single item whose own text contains embedded "\n".

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// streamMultilineReply attaches a to sess-x (if not already) and streams a
// single assistant reply, one line at a time via MessageDelta — never
// finishing the turn — so the transcript holds exactly one item whose text
// accumulates totalLines physical lines. Returns the updated App.
func streamMultilineReply(t *testing.T, a App, totalLines int) App {
	t.Helper()
	mdl, _ := a.Update(sessEventMsg{id: "sess-x", ev: event.NewMessageStarted("sess-x", event.MessageText)})
	a = mdl.(App)
	for i := 0; i < totalLines; i++ {
		text := fmt.Sprintf("line %d", i)
		if i > 0 {
			text = "\n" + text
		}
		mdl, _ = a.Update(sessEventMsg{id: "sess-x", ev: event.NewMessageDelta("sess-x", event.MessageText, text)})
		a = mdl.(App)
	}
	return a
}

// newStreamingAttachApp returns an App already attached to sess-x, sized to
// testkit's fixed golden dimensions, with cfg controlling the tui.autoscroll
// setting the streaming tests below toggle.
func newStreamingAttachApp(t *testing.T, cfg config.Config) App {
	t.Helper()
	meta := GoldenMeta()
	meta.AttachSessionID = "sess-x"
	env := GoldenCommandEnv()
	env.Config = func() (config.Config, error) { return cfg, nil }
	a := NewApp(theme.Test(), &internalFakeSup{}, meta, env)
	mdl, _ := a.Update(tea.WindowSizeMsg{Width: testkit.Width, Height: testkit.Height})
	return mdl.(App)
}

// TestStreamingMultilineReplyTailFollows is the failing-then-passing
// reproduction for defect 2a: a single assistant reply streams in, deltas
// arriving one physical line at a time (see streamMultilineReply), never
// finished, totaling far more lines than the fixed testkit.Height (24)
// leaves room for once the header/footer are carved out. Before the fix
// (styledMarkerLines in model.go), the whole growing reply was one
// []string entry carrying raw embedded "\n" characters: transcriptLines
// counted it as ONE line against Model.view's avail/scrollTail/pad height
// math, so avail/scrollTail never clipped it and the render blew well past
// a.height while STILL failing to show the newest streamed line (see this
// test's prior failure log: 36 rows rendered for a.height=24, "line 79"
// nowhere in the output, the header pinned at the top instead) — the exact
// "old messages pinned at the top, new content off the bottom" user report.
// This asserts both halves of the fix: the render never exceeds a.height,
// and the tail (scroll=0, the default) shows the newest content.
func TestStreamingMultilineReplyTailFollows(t *testing.T) {
	a := newStreamingAttachApp(t, config.Config{})

	const totalLines = 80
	a = streamMultilineReply(t, a, totalLines)

	got := a.render()
	rows := strings.Split(got, "\n")
	if len(rows) > a.height {
		t.Errorf("render produced %d rows, want <= a.height (%d): a multi-line streamed item overflowed the frame instead of being clipped by scrollTail\n%s", len(rows), a.height, got)
	}
	wantLatest := fmt.Sprintf("line %d", totalLines-1)
	if !strings.Contains(got, wantLatest) {
		t.Errorf("tailed render (scroll=0, the default) missing the newest streamed line %q — the transcript is anchored to the top instead of tailing:\n%s", wantLatest, got)
	}
	if strings.Contains(got, "gofer v0.3.0") {
		t.Errorf("tailed render still shows the identity header on an overflowing streamed reply; want it scrolled away like the rest of the oldest content:\n%s", got)
	}
}

// TestStreamingWheelScrollsAttachDuringOverflow is defect 2b's streaming
// companion to TestHandleWheelScrollsOverflowingTranscript: that existing
// test only proves the wheel moves a.scroll against a transcript built from
// many settled single-line turns. This proves the SAME wheel notch moves
// the visible window while a single reply is still actively streaming
// multi-line content in — the exact moment (and screen) the user reported
// the wheel doing nothing on.
func TestStreamingWheelScrollsAttachDuringOverflow(t *testing.T) {
	a := newStreamingAttachApp(t, config.Config{})

	const totalLines = 80
	a = streamMultilineReply(t, a, totalLines)

	tailed := a.render()
	wantLatest := fmt.Sprintf("line %d", totalLines-1)
	if !strings.Contains(tailed, wantLatest) {
		t.Fatalf("precondition failed: tailed render missing the newest streamed line %q:\n%s", wantLatest, tailed)
	}

	a = a.handleWheel(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if a.scroll == 0 {
		t.Fatal("precondition failed: handleWheel(up) left a.scroll at 0")
	}

	scrolled := a.render()
	if strings.Contains(scrolled, wantLatest) {
		t.Errorf("one wheel-up notch during active streaming still shows the newest line %q — the visible window did not move:\ntailed:\n%s\nscrolled:\n%s", wantLatest, tailed, scrolled)
	}
	if scrolled == tailed {
		t.Error("render() unchanged after handleWheel(up) during active streaming — the wheel notch had no visible effect")
	}
}

// TestAutoscrollEnabledDefaultTailsOnNewEvents covers the tui.autoscroll
// config setting's default (unset/true): streaming content keeps tailing to
// the latest line exactly as it always has — an explicit behavioral pin for
// the default, alongside the (already covered) fix itself above.
func TestAutoscrollEnabledDefaultTailsOnNewEvents(t *testing.T) {
	a := newStreamingAttachApp(t, config.Config{}) // zero Config: autoscroll unset -> true

	a = streamMultilineReply(t, a, 40)
	mid := a.render()
	if !strings.Contains(mid, "line 39") {
		t.Fatalf("autoscroll-on render after 40 lines missing the latest line 'line 39':\n%s", mid)
	}

	a = streamMultilineReply(t, a, 20) // 20 more lines land on a NEW item (started again)
	final := a.render()
	if !strings.Contains(final, "line 19") {
		t.Errorf("autoscroll-on render after further streaming missing the newest content 'line 19' — it should keep tailing:\n%s", final)
	}
}

// TestAutoscrollDisabledStaysPutOnNewEvents covers tui.autoscroll=false: new
// streaming content must NOT force the view down toward the tail — it stays
// on the exact lines the operator was looking at. Once the transcript has
// overflowed, the visible window is pinned to the same absolute lines (same
// start/end indices into the growing transcript) as more content streams in,
// so the render is byte-identical before and after further growth, and never
// shows the newest line.
func TestAutoscrollDisabledStaysPutOnNewEvents(t *testing.T) {
	disabled := false
	a := newStreamingAttachApp(t, config.Config{TUI: config.TUI{Autoscroll: &disabled}})

	a = streamMultilineReply(t, a, 80) // overflow testkit.Height (24) well past the frame
	before := a.render()
	if strings.Contains(before, "line 79") {
		t.Fatalf("precondition failed: autoscroll-off render already shows the newest line after the first overflow — nothing to prove \"stays put\" against:\n%s", before)
	}

	a = streamMultilineReply(t, a, 40) // stream substantially more content
	after := a.render()

	if after != before {
		t.Errorf("autoscroll-off render changed after new streamed content arrived; want the view frozen exactly where it was:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if strings.Contains(after, "line 39") {
		t.Errorf("autoscroll-off render jumped to show the newest streamed content 'line 39' — it should have stayed put:\n%s", after)
	}
}

// TestIngestAttachNoPanicAtTinyHeights extends the #87 tiny-height guard
// (TestRenderNoPanicAtTinyHeightsWithContent) to this PR's own new surface:
// ingestAttach's autoscroll-disabled path calls transcriptLines(a.width)
// directly (not just at render time), so a zero/tiny width — the first
// frame, before any WindowSizeMsg arrives — must not panic there either.
func TestIngestAttachNoPanicAtTinyHeights(t *testing.T) {
	sizes := []struct{ width, height int }{
		{0, 0}, {1, 1}, {2, 2}, {80, 0}, {10, 5},
	}
	for _, sz := range sizes {
		t.Run(fmt.Sprintf("%dx%d", sz.width, sz.height), func(t *testing.T) {
			disabled := false
			a := newStreamingAttachApp(t, config.Config{TUI: config.TUI{Autoscroll: &disabled}})
			mdl, _ := a.Update(tea.WindowSizeMsg{Width: sz.width, Height: sz.height})
			a = mdl.(App)
			a = streamMultilineReply(t, a, 30) // must not panic at this size
			_ = a.render()                     // must not panic
		})
	}
}
