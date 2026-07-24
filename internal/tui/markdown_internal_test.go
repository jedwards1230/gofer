package tui

// markdown_internal_test.go exercises the settled-assistant-text markdown path
// (markdown.go) against Model/App internals: that markdown renders only once a
// message settles (never on a streaming delta), that the render is
// deterministic and ANSI-free under the Ascii golden profile, that a code
// block's raw text survives verbatim and stays selectable, and that the output
// reflows to the transcript width.

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// sidMD is a fixed session id for the markdown tests (package tui's own copy —
// golden_test.go's sid lives in package tui_test).
const sidMD = "0192a1b2-c3d4-7e5f-8a90-0000000000a1"

// markdownSample is a settled assistant reply exercising every element the
// feature renders: a heading, bold/italic, an unordered list, inline code, a
// fenced (indented) code block, and a link.
const markdownSample = "# Title\n\n" +
	"Some **bold** and *italic* text with `inline`.\n\n" +
	"- first item\n" +
	"- second item\n\n" +
	"```go\n" +
	"func main() {\n" +
	"    x := 1\n" +
	"}\n" +
	"```\n\n" +
	"See [docs](https://example.com).\n"

// TestMarkdownRendersCompleteBlocksProgressively is the load-bearing
// incremental-render assertion. Mid-stream, a message's COMPLETE blocks (a
// paragraph closed by a blank line, or a closed fence) are glamoured while the
// message streams — but the trailing INCOMPLETE block (here a half-arrived
// fence) stays raw, because glamouring a half-block is garbage. The mid-stream
// text below has two complete blocks (a heading, a bold paragraph) and an
// incomplete tail (an unclosed ```go fence):
//
//   - the complete bold paragraph is rendered — its "**bold**" markers are gone;
//   - the incomplete fence is NOT — its raw "```go" and typed body show through.
//
// Both directions are mutation-checks: were the tail glamoured too, "```go"
// would vanish; were nothing split into blocks (all treated as the raw tail),
// "**bold**" would reappear. On settle the whole message — closed fence and all —
// renders, so neither the asterisks nor any fence marker survives.
func TestMarkdownRendersCompleteBlocksProgressively(t *testing.T) {
	const streamed = "# Title\n\n" +
		"Some **bold** text.\n\n" +
		"```go\nfunc main() {" // unclosed fence: the incomplete tail

	streaming := New(theme.Test()).
		Ingest(event.NewMessageStarted(sidMD, event.MessageText)).
		Ingest(event.NewMessageDelta(sidMD, event.MessageText, streamed))
	streamOut := ansi.Strip(streaming.View(testkit.Width, testkit.Height))
	if strings.Contains(streamOut, "**bold**") {
		t.Errorf("a COMPLETE block mid-stream should be glamoured (no raw **bold**), got:\n%s", streamOut)
	}
	if !strings.Contains(streamOut, "bold") {
		t.Errorf("the glamoured complete block dropped its content ('bold'):\n%s", streamOut)
	}
	if !strings.Contains(streamOut, "```go") || !strings.Contains(streamOut, "func main() {") {
		t.Errorf("the INCOMPLETE trailing fence should stay raw (```go + body), got:\n%s", streamOut)
	}

	// On settle the whole message renders (fence now closed): no raw markers left.
	settled := streaming.Ingest(event.NewMessageFinished(sidMD, event.MessageText, markdownSample))
	out := ansi.Strip(settled.View(testkit.Width, testkit.Height))
	if strings.Contains(out, "**bold**") {
		t.Errorf("settled render still shows the raw **bold** markers; markdown was not rendered:\n%s", out)
	}
	if strings.Contains(out, "```") {
		t.Errorf("settled render still shows the raw ``` fence; the code block was not rendered:\n%s", out)
	}
	if !strings.Contains(out, "bold") || !strings.Contains(out, "func main() {") {
		t.Errorf("settled render dropped rendered content (expected 'bold' and the code body):\n%s", out)
	}
}

// TestStreamingHalfFenceNeverGlamoured pins the safety edge the incremental
// path must never violate: a fence that has opened but not closed is the
// incomplete tail and is rendered RAW, verbatim — feeding a half-fence to
// glamour renders broken. It also confirms a preceding complete paragraph IS
// glamoured, so the two coexist in one streaming frame.
func TestStreamingHalfFenceNeverGlamoured(t *testing.T) {
	// One complete paragraph, then an unclosed fence whose body is literal
	// markdown that MUST NOT be interpreted while raw.
	const streamed = "Intro paragraph.\n\n```\n**not bold** inside a fence"
	m := New(theme.Test()).
		Ingest(event.NewMessageStarted(sidMD, event.MessageText)).
		Ingest(event.NewMessageDelta(sidMD, event.MessageText, streamed))
	out := ansi.Strip(m.View(testkit.Width, testkit.Height))
	if !strings.Contains(out, "```") {
		t.Errorf("the unclosed fence delimiter must show raw, got:\n%s", out)
	}
	if !strings.Contains(out, "**not bold** inside a fence") {
		t.Errorf("the half-fence body must show raw and verbatim, got:\n%s", out)
	}
	if !strings.Contains(out, "Intro paragraph.") {
		t.Errorf("the preceding complete paragraph is missing:\n%s", out)
	}
}

// TestSplitMarkdownBlocks is the table-driven oracle for the block splitter
// that decides complete-vs-incomplete — the load-bearing half of incremental
// rendering. The completeness rules it pins: a paragraph completes on a blank
// line (not a lone trailing newline); a fence completes on its close and a blank
// line inside it never splits it; a closing fence must carry no info string (so
// a "```python" line inside a fence is body, not a close); tilde fences and a
// fence interrupting a paragraph both behave.
func TestSplitMarkdownBlocks(t *testing.T) {
	tests := []struct {
		name         string
		in           string
		wantComplete []string
		wantTrailing string
	}{
		{"empty", "", nil, ""},
		{"single incomplete paragraph", "hello world", nil, "hello world"},
		{"lone trailing newline does not complete", "para\n", nil, "para"},
		{"blank line completes the paragraph", "para\n\n", []string{"para"}, ""},
		{"soft break stays one paragraph", "a\nb", nil, "a\nb"},
		{"one complete + incomplete tail", "a\n\nb", []string{"a"}, "b"},
		{"two complete blocks", "a\n\nb\n\n", []string{"a", "b"}, ""},
		{"closed fence is complete", "```go\nx\n```", []string{"```go\nx\n```"}, ""},
		{"open fence is the incomplete tail", "```go\nfunc() {", nil, "```go\nfunc() {"},
		{"blank inside fence does not split", "```\n\nx\n```", []string{"```\n\nx\n```"}, ""},
		{"info-string line inside fence is body, not close", "```\n```python\ncode\n```", []string{"```\n```python\ncode\n```"}, ""},
		{"tilde fence", "~~~\nx\n~~~", []string{"~~~\nx\n~~~"}, ""},
		{"fence interrupts a paragraph with no blank between", "a\n```\nx\n```", []string{"a", "```\nx\n```"}, ""},
		{"paragraph then still-open fence", "a\n\n```go\nx", []string{"a"}, "```go\nx"},
		{"complete blocks then open fence", "# T\n\npara\n\n```go\nf()", []string{"# T", "para"}, "```go\nf()"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			complete, trailing := splitMarkdownBlocks(tc.in)
			if !slices.Equal(complete, tc.wantComplete) {
				t.Errorf("complete = %#v, want %#v", complete, tc.wantComplete)
			}
			if trailing != tc.wantTrailing {
				t.Errorf("trailing = %q, want %q", trailing, tc.wantTrailing)
			}
		})
	}
}

// TestMarkdownRenderIsDeterministic locks the two golden-testability
// guarantees: the Ascii profile yields ANSI-free output, and the same settled
// message renders byte-identically every time — both across repeated View
// calls on one Model and across two Models built from the same events (the
// memo must not make the second render differ from the first).
func TestMarkdownRenderIsDeterministic(t *testing.T) {
	build := func() Model {
		return New(theme.Test()).
			Ingest(event.NewMessageStarted(sidMD, event.MessageText)).
			Ingest(event.NewMessageFinished(sidMD, event.MessageText, markdownSample))
	}
	m := build()
	first := m.View(testkit.Width, testkit.Height)
	if strings.Contains(first, "\x1b") {
		t.Errorf("Ascii-profile markdown render carries ANSI escapes; goldens would be unstable:\n%q", first)
	}
	if second := m.View(testkit.Width, testkit.Height); second != first {
		t.Errorf("repeated View of the same Model differ:\n--- first ---\n%s\n--- second ---\n%s", first, second)
	}
	if fresh := build().View(testkit.Width, testkit.Height); fresh != first {
		t.Errorf("independent Models from the same events render differently:\n--- a ---\n%s\n--- b ---\n%s", first, fresh)
	}
}

// TestMarkdownReflowsToWidth proves the render is wired to the transcript width:
// the same message wraps to more rows at a narrow width than at a wide one.
func TestMarkdownReflowsToWidth(t *testing.T) {
	prose := "The quick brown fox jumps over the lazy dog while reviewing the refactored authentication middleware."
	m := New(theme.Test()).
		Ingest(event.NewMessageStarted(sidMD, event.MessageText)).
		Ingest(event.NewMessageFinished(sidMD, event.MessageText, prose))
	wide := len(m.transcriptLines(80))
	narrow := len(m.transcriptLines(30))
	if narrow <= wide {
		t.Errorf("expected more rows at width 30 (%d) than width 80 (%d) — markdown did not reflow", narrow, wide)
	}
}

// TestMarkdownCodeBlockCopiesVerbatimAndSelectable drives a real attach App: it
// renders a settled message with a code block, finds the code line in the
// composed frame, selects exactly its raw text, and asserts the selection —
// which flows through the same transcript-region path OSC 52 copies from —
// returns the code verbatim, indentation preserved and no trailing padding.
// This is both the "rendered markdown stays selectable" and the "code blocks
// copy raw" guarantee in one.
func TestMarkdownCodeBlockCopiesVerbatimAndSelectable(t *testing.T) {
	meta := GoldenMeta()
	meta.AttachSessionID = "sess-md"
	a := NewApp(theme.Test(), &internalFakeSup{}, meta, GoldenCommandEnv())
	mdl, _ := a.Update(tea.WindowSizeMsg{Width: testkit.Width, Height: testkit.Height})
	a = mdl.(App)

	for _, ev := range []event.Event{
		event.NewMessageStarted("sess-md", event.MessageText),
		event.NewMessageFinished("sess-md", event.MessageText, "Here:\n\n```go\nfunc main() {\n    x := 1\n}\n```\n"),
	} {
		mdl, _ = a.Update(sessEventMsg{id: "sess-md", ev: ev})
		a = mdl.(App)
	}

	// The distinctive indented code line, as it appears in the composed frame:
	// the 4-space code indent, prefixed by the 2-space marker-alignment indent.
	const wantRaw = "    x := 1"
	rendered := a.render()
	lines := strings.Split(rendered, "\n")
	row, col := -1, -1
	for i, l := range lines {
		if idx := strings.Index(l, wantRaw); idx >= 0 {
			row, col = i, idx
			break
		}
	}
	if row < 0 {
		t.Fatalf("code line %q not found in the rendered frame:\n%s", wantRaw, rendered)
	}

	// Select from the code text's first column through its last — the exact
	// raw code, excluding the leading marker-alignment indent.
	a.sel = &selectionState{startX: col, startY: row, curX: col + len(wantRaw) - 1, curY: row}
	got := a.selectedText()
	if got != wantRaw {
		t.Errorf("selectedText over the code row = %q, want the raw code %q (verbatim, no padding)", got, wantRaw)
	}
}

// TestMarkdownColorDoesNotChangeLayout is the ANSI-aware-trim guarantee: a
// rich markdown message rendered through a real color profile, stripped of its
// ANSI, must byte-match the same message rendered under the Ascii profile — the
// styled elements (heading, code) must not shift a single cell — and no colored
// row may exceed the width. Mirrors assertColorLayout for a markdown body.
func TestMarkdownColorDoesNotChangeLayout(t *testing.T) {
	const width = 48
	build := func(th theme.Theme) Model {
		return New(th).
			Ingest(event.NewMessageStarted(sidMD, event.MessageText)).
			Ingest(event.NewMessageFinished(sidMD, event.MessageText, markdownSample))
	}
	plain := build(theme.Test()).View(width, testkit.Height)
	colored := build(testkit.ColorTheme()).View(width, testkit.Height)
	if stripped := ansi.Strip(colored); stripped != plain {
		t.Errorf("colored markdown stripped of ANSI != plain (color changed layout)\n--- stripped ---\n%s\n--- plain ---\n%s", stripped, plain)
	}
	for i, line := range strings.Split(colored, "\n") {
		if w := ansi.StringWidth(line); w > width {
			t.Errorf("colored markdown line %d exceeds width %d (got %d): %q", i, width, w, line)
		}
	}
}

// TestTrimTrailingPad covers the ANSI-aware trailing-space trim directly: a
// styled line keeps its retained cells' styling and loses only the padding, and
// its ANSI-stripped form matches the plain line's trim — the property
// postProcessMarkdown relies on to keep colored and Ascii renders identical.
func TestTrimTrailingPad(t *testing.T) {
	if got := trimTrailingPad("hello    "); got != "hello" {
		t.Errorf("trimTrailingPad(plain) = %q, want %q", got, "hello")
	}
	if got := trimTrailingPad("      "); got != "" {
		t.Errorf("trimTrailingPad(all spaces) = %q, want empty", got)
	}
	styled := "\x1b[38;5;39mhi\x1b[0m\x1b[38;5;252m \x1b[0m\x1b[38;5;252m \x1b[0m"
	got := trimTrailingPad(styled)
	if ansi.Strip(got) != "hi" {
		t.Errorf("trimTrailingPad(styled) stripped = %q, want %q", ansi.Strip(got), "hi")
	}
	if !strings.Contains(got, "\x1b[38;5;39m") {
		t.Errorf("trimTrailingPad(styled) dropped the retained cell's styling: %q", got)
	}
}
