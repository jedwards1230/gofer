package tui

// copy_transcript_test.go covers whole-transcript copy (gofer#312), the
// counterpart to select-all's viewport-bounded copy.
//
// The property that matters is REACH, not formatting: select-all already
// produces well-shaped text, but it can only ever address rows the frame is
// showing. So every test here builds a transcript materially LONGER than the
// viewport and asserts on content that is provably scrolled off — a fixture
// that fits on screen would pass identically against the old viewport-bounded
// path and prove nothing.

import (
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// longTranscriptApp returns an attach-screen App whose transcript is turns
// user/assistant exchanges long — deliberately far more than testkit.Height can
// show — with each message carrying its turn number so any given row can be
// asserted present or absent by content.
func longTranscriptApp(t *testing.T, turns int) App {
	t.Helper()
	const s = "sess-long"
	m := New(theme.Test())
	for i := range turns {
		m.Ingest(event.NewMessageFinished(s, event.MessageUser, fmt.Sprintf("user turn %d", i)))
		m.Ingest(event.NewMessageStarted(s, event.MessageText))
		m.Ingest(event.NewMessageFinished(s, event.MessageText, fmt.Sprintf("assistant turn %d", i)))
	}
	a := NewApp(theme.Test(), newInternalFakeSup(GoldenRoster()), GoldenMeta(), GoldenCommandEnv())
	mdl, _ := a.Update(tea.WindowSizeMsg{Width: testkit.Width, Height: testkit.Height})
	a = mdl.(App)
	a.scr = screenAttach
	a.sess = m
	return a
}

// TestCopyTranscriptReachesScrolledOffContent is the load-bearing assertion:
// content the viewport cannot show must still reach the clipboard.
//
// The first turn is the probe. With a transcript this long it is scrolled far
// off the top — asserted, not assumed, by first confirming it is absent from
// the rendered frame — so its presence in the copied text is only possible if
// the copy bypassed the viewport entirely.
func TestCopyTranscriptReachesScrolledOffContent(t *testing.T) {
	a := longTranscriptApp(t, 60)

	frame := ansi.Strip(a.render())
	if strings.Contains(frame, "user turn 0") {
		t.Fatalf("precondition: turn 0 is still on screen, so this test cannot "+
			"distinguish a whole-transcript copy from a viewport copy:\n%s", frame)
	}
	if !strings.Contains(frame, "assistant turn 59") {
		t.Fatalf("precondition: the transcript tail is not on screen; the fixture is wrong:\n%s", frame)
	}

	next, cmd := a.copyTranscript()
	if cmd == nil {
		t.Fatal("copyTranscript produced no clipboard Cmd")
	}
	if next.sess.items == nil {
		t.Error("copyTranscript mutated the session's transcript away")
	}
	copied := fmt.Sprintf("%v", cmd())

	// Both ends, and a middle row, must be present.
	for _, want := range []string{"user turn 0", "assistant turn 0", "user turn 30", "assistant turn 59"} {
		if !strings.Contains(copied, want) {
			t.Errorf("copied transcript is missing %q — the copy is still viewport-bounded", want)
		}
	}
	// Every turn, not just the sampled ones.
	for i := range 60 {
		if !strings.Contains(copied, fmt.Sprintf("user turn %d", i)) {
			t.Fatalf("copied transcript is missing user turn %d", i)
		}
	}
}

// TestCopyTranscriptIsNormalizedLikeSelectAll pins that whole-transcript copy
// reuses normalizeCopy rather than growing a second, divergent shape. The rules
// were confirmed by hand against a live roster and are deliberately shared.
//
// The fixture matters more than the assertions. An ordinary transcript's
// rendered lines are ALREADY clean — transcriptLines emits at most one blank
// gap between blocks and pads nothing — so asserting normalization over one is
// vacuous: it passes just as happily against a copy that never normalizes at
// all (confirmed by mutation). The messages below therefore carry interior
// blank RUNS and trailing spaces of their own, which is the only way the
// assertions can tell the two implementations apart.
func TestCopyTranscriptIsNormalizedLikeSelectAll(t *testing.T) {
	const s = "sess-msgy"
	m := New(theme.Test())
	m.Ingest(event.NewMessageFinished(s, event.MessageUser, "first   \n\n\n\nafter a blank run   "))
	m.Ingest(event.NewMessageStarted(s, event.MessageText))
	m.Ingest(event.NewMessageFinished(s, event.MessageText, "reply line   \n\n\n\ntrailing block   "))
	a := longTranscriptApp(t, 40)
	a.sess = m

	// Precondition: the raw lines really do contain what normalization removes,
	// or the assertions below cannot discriminate.
	raw := a.sess.transcriptLines(a.width)
	sawRun, sawPad := false, false
	for i, l := range raw {
		if l != strings.TrimRight(l, " \t") {
			sawPad = true
		}
		if i > 0 && strings.TrimSpace(l) == "" && strings.TrimSpace(raw[i-1]) == "" {
			sawRun = true
		}
	}
	if !sawRun || !sawPad {
		t.Fatalf("precondition: raw transcript lines have no blank run (%t) or no trailing padding (%t); "+
			"this fixture cannot distinguish normalized from unnormalized output:\n%q", sawRun, sawPad, raw)
	}

	_, cmd := a.copyTranscript()
	if cmd == nil {
		t.Fatal("copyTranscript produced no clipboard Cmd")
	}
	got := strings.Split(fmt.Sprintf("%v", cmd()), "\n")

	for i, line := range got {
		if line != strings.TrimRight(line, " \t") {
			t.Errorf("copied line %d has trailing whitespace: %q", i, line)
		}
		if i > 0 && line == "" && got[i-1] == "" {
			t.Errorf("copied text has a run of blank lines at %d", i)
		}
	}
	if len(got) > 0 && (got[0] == "" || got[len(got)-1] == "") {
		t.Errorf("copied text has a leading or trailing blank line: %q … %q", got[0], got[len(got)-1])
	}
}

// TestCopyTranscriptEmptyIsANoOp guards the clipboard: copying an empty
// transcript must not issue a Cmd at all. An empty clipboard WRITE would
// silently destroy whatever the user had copied before, which is worse than
// doing nothing — the same rule selectAll follows.
func TestCopyTranscriptEmptyIsANoOp(t *testing.T) {
	a := NewApp(theme.Test(), newInternalFakeSup(GoldenRoster()), GoldenMeta(), GoldenCommandEnv())
	mdl, _ := a.Update(tea.WindowSizeMsg{Width: testkit.Width, Height: testkit.Height})
	a = mdl.(App)
	a.scr = screenAttach

	if _, cmd := a.copyTranscript(); cmd != nil {
		t.Errorf("copyTranscript over an empty transcript issued a clipboard Cmd (%v); want none", cmd())
	}
}

// TestCopyTranscriptBindingIsAttachOnlyAndGatedOnEmptyInput pins the keybinding
// contract alt+a ships under. Both halves matter: it must not fire on the
// overview (which has no transcript), and it must yield to the input bar the
// same way ctrl+a does, so the two select/copy keys behave consistently rather
// than one stealing a key the other yields.
func TestCopyTranscriptBindingIsAttachOnlyAndGatedOnEmptyInput(t *testing.T) {
	altA := tea.Key{Code: 'a', Mod: tea.ModAlt}

	var binding *keyBinding
	for i, b := range globalKeymap() {
		if b.Keys == "alt+a" {
			binding = &globalKeymap()[i]
			break
		}
	}
	if binding == nil {
		t.Fatal("no alt+a binding in globalKeymap() — ? will not document whole-transcript copy")
	}
	if !binding.match(altA) {
		t.Error("the alt+a binding does not match an alt+a key press")
	}

	attach := longTranscriptApp(t, 20)
	if !binding.enabled(attach) {
		t.Error("alt+a is disabled on the attach screen with an empty input bar; it should fire there")
	}

	overview := attach
	overview.scr = screenOverview
	if binding.enabled(overview) {
		t.Error("alt+a is enabled on the overview, which has no transcript to copy")
	}

	typing := attach
	typing.sess = typing.sess.SetInput("half-written prompt")
	if binding.enabled(typing) {
		t.Error("alt+a fired with text in the input bar; it must yield like ctrl+a does")
	}
}

// BenchmarkCopyTranscript measures whole-transcript copy, which walks EVERY
// item rather than the viewport's worth — so unlike select-all its cost grows
// with transcript length, the axis that produced gofer#308.
//
// Swept rather than reported as a single number, per CONTRIBUTING: one point
// cannot distinguish linear from quadratic, and "what happens as this grows" is
// the whole question for an action whose entire purpose is to reach content the
// viewport cannot show.
//
// It lives in this internal test file rather than transcript_bench_test.go
// because that file is package tui_test and the subject (transcriptLines +
// normalizeCopy) is unexported.
func BenchmarkCopyTranscript(b *testing.B) {
	const s = "sess-bench"
	for _, turns := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("turns=%d", turns), func(b *testing.B) {
			m := New(theme.Test())
			for i := range turns {
				m.Ingest(event.NewMessageFinished(s, event.MessageUser, fmt.Sprintf("user turn %d: wire the websocket ACP listener", i)))
				m.Ingest(event.NewMessageStarted(s, event.MessageText))
				m.Ingest(event.NewMessageFinished(s, event.MessageText, fmt.Sprintf("assistant turn %d: the listener already accepts upgrades; wiring the fan-out", i)))
			}

			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				got := normalizeCopy(m.transcriptLines(testkit.Width))
				// Guard against a future short-circuit making this benchmark
				// measure an early return instead of the copy. A benchmark that
				// stops exercising its subject reads as a spectacular
				// optimization.
				if got == "" {
					b.Fatal("copy produced no text")
				}
			}
		})
	}
}
