package tui

// markdown.go renders an assistant-text item's markdown to styled terminal rows
// via Charm's glamour (the library behind `glow`) — bold/italic, headings,
// lists, blockquotes, inline code, links, and fenced code blocks.
//
// Two render paths share the renderer and its cache:
//
//   - [markdownRenderer.render] renders a SETTLED message's whole text at once,
//     so glamour's cross-block layout is exactly what a finished reply shows.
//   - [markdownRenderer.renderStreaming] renders a STILL-STREAMING message
//     incrementally: its COMPLETE markdown blocks are glamoured (each memoized
//     by its own text, so a keystroke that only grows the tail re-renders
//     nothing already complete) while the trailing INCOMPLETE block — a
//     half-arrived fence or a paragraph not yet closed by a blank line — stays
//     raw, because glamouring a half-block renders garbage. [splitMarkdownBlocks]
//     is the complete-vs-incomplete oracle.
//
// Three properties the rest of the TUI depends on shape this file:
//
//   - One row per slice entry, no embedded "\n". transcriptLines' height math
//     and the mouse selection's row measurement (mouse.go) both budget on the
//     invariant that one returned slice entry is one terminal row. glamour
//     emits a single multi-line string, so [markdownRenderer.render] splits it
//     and hands back one entry per physical row, each already wrapped to the
//     width it was given.
//   - Deterministic goldens. glamour emits ANSI; under termenv.Ascii (the
//     profile theme.Test forces for golden tests) render strips every escape,
//     so a golden file is plain, byte-stable text — the same guarantee the
//     theme's Style methods give the rest of the transcript. A real terminal
//     profile keeps the ANSI (color + attributes).
//   - Cheap re-render. glamour re-parses on every Render call (~80µs even with
//     a reused renderer), and View re-renders the whole transcript on every
//     keystroke, so each distinct text (a settled message, or a streaming
//     message's every complete block) is rendered once and memoized by
//     (width, text). The cache is cleared whenever the width changes (a
//     resize), which also bounds its growth.

import (
	"strings"
	"sync"

	"github.com/charmbracelet/glamour"
	glamouransi "github.com/charmbracelet/glamour/ansi"
	glamourstyles "github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// mdMinWidth is the narrowest wrap width markdown rendering is attempted at.
// Below it glamour's word-wrap produces cramped, list-mangling output, so
// render falls back to plain text lines — the transcript stays legible in a
// sliver-width window rather than trying to lay out headings and code blocks
// in a handful of columns.
const mdMinWidth = 4

// markdownRenderer renders settled assistant markdown to styled terminal rows,
// memoized by width. It is created once in New and shared (by pointer) across
// every copy-on-write copy of a Model, so the memo survives the Model's
// per-keystroke recopying. All access is serialized by mu — the bubbletea
// render loop is single-threaded today, but the lock keeps the shared cache
// honest regardless.
type markdownRenderer struct {
	profile termenv.Profile
	style   glamouransi.StyleConfig

	mu       sync.Mutex
	width    int
	renderer *glamour.TermRenderer
	cache    map[string][]string
}

// newMarkdownRenderer returns a renderer bound to profile — termenv.Ascii for
// golden tests (output is ANSI-stripped, deterministic), the detected terminal
// profile in the live adapter (output keeps color + attributes).
func newMarkdownRenderer(profile termenv.Profile) *markdownRenderer {
	return &markdownRenderer{profile: profile, style: markdownStyle()}
}

// render lays text out as markdown word-wrapped to width, returning one slice
// entry per terminal row — no entry carries an embedded "\n", none carries
// trailing padding, and leading/trailing/duplicate blank rows are collapsed
// away. Under termenv.Ascii every ANSI escape is stripped so the rows are
// plain, byte-stable text. A width below mdMinWidth, or any glamour error,
// falls back to the raw text split into lines so nothing is ever lost.
func (r *markdownRenderer) render(text string, width int) []string {
	if r == nil || width < mdMinWidth {
		return fallbackLines(text)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.renderer == nil || r.width != width {
		tr, err := glamour.NewTermRenderer(
			glamour.WithStyles(r.style),
			glamour.WithColorProfile(r.profile),
			glamour.WithWordWrap(width),
		)
		if err != nil {
			return fallbackLines(text)
		}
		r.renderer = tr
		r.width = width
		r.cache = map[string][]string{}
	}

	if rows, ok := r.cache[text]; ok {
		return rows
	}

	out, err := r.renderer.Render(text)
	if err != nil {
		return fallbackLines(text)
	}
	rows := postProcessMarkdown(out, r.profile == termenv.Ascii)
	r.cache[text] = rows
	return rows
}

// fallbackLines splits raw text into one entry per physical line — the shape
// render's callers expect (no embedded "\n") when markdown rendering is
// skipped or fails.
func fallbackLines(text string) []string {
	return strings.Split(text, "\n")
}

// renderStreaming lays out a STILL-STREAMING assistant message incrementally:
// its COMPLETE markdown blocks are rendered through glamour (each memoized by
// its own text, exactly like render — so a keystroke that only grows the tail
// re-renders nothing already complete), and the trailing INCOMPLETE block — a
// half-arrived fence or a paragraph not yet closed by a blank line — is kept as
// raw physical lines. Glamouring a half-block is garbage (a lone "```go" reads
// as an unterminated fence, a mid-typed "**bo" as literal asterisks), which is
// why the tail waits until it completes.
//
// A block is COMPLETE when it is a paragraph followed by a blank line (or ended
// by the fence that follows it) or a fence closed by its "```", per
// [splitMarkdownBlocks]. One blank row separates adjacent rendered blocks,
// matching render's settled whole-document rhythm; the raw tail is fronted by
// the same one-blank separator. width is the CONTENT width (already less the
// marker glyph + its trailing space), the same value render is handed.
//
// A message with no complete block yet (a single still-typing paragraph — the
// common short-reply case) returns exactly render's raw fallback shape for that
// text, so the marker block the caller wraps it in is byte-identical to the old
// "stream raw until settled" path.
func (r *markdownRenderer) renderStreaming(text string, width int) []string {
	complete, trailing := splitMarkdownBlocks(text)
	var rows []string
	for _, blk := range complete {
		if len(rows) > 0 {
			rows = append(rows, "")
		}
		rows = append(rows, r.render(blk, width)...)
	}
	if strings.TrimSpace(trailing) != "" {
		if len(rows) > 0 {
			rows = append(rows, "")
		}
		rows = append(rows, fallbackLines(trailing)...)
	}
	return rows
}

// splitMarkdownBlocks partitions a streaming markdown string into its COMPLETE
// blocks and the single trailing INCOMPLETE block (which may be ""). It is
// fence-aware: a blank line inside a fenced code block does not split it, and a
// fence is a block in its own right, complete only once its closing delimiter
// arrives.
//
// Completeness follows one rule per block kind:
//
//   - A paragraph is complete once a blank line follows it (or a fence opens
//     immediately after it — a fence interrupts a paragraph, so no more text can
//     join it). A paragraph with no terminating blank yet is the incomplete
//     tail: more deltas may still extend it, and reflowing it every keystroke
//     would churn.
//   - A fence is complete once a line of >= its opening run of the same fence
//     char, carrying no info string, closes it. An unclosed fence is the
//     incomplete tail.
//
// A single trailing newline is the current line's terminator, NOT a blank
// separator: "para\n" leaves "para" incomplete, while "para\n\n" completes it.
// That one distinction is what keeps a just-finished line from glamouring a beat
// early only to reflow when the next word of the same paragraph streams in.
func splitMarkdownBlocks(text string) (complete []string, trailing string) {
	lines := strings.Split(text, "\n")
	// Drop the lone "" a single trailing "\n" produces — it is the current
	// line's terminator, not a blank line. ("\n\n" keeps one "", a real blank.)
	if n := len(lines); n >= 2 && lines[n-1] == "" {
		lines = lines[:n-1]
	}

	var cur []string // the block under construction
	inFence := false
	var fenceChar byte
	var fenceLen int

	flush := func() {
		if len(cur) > 0 {
			complete = append(complete, strings.Join(cur, "\n"))
			cur = nil
		}
	}

	for _, ln := range lines {
		if inFence {
			cur = append(cur, ln)
			if c, n, info := fenceLine(ln); c == fenceChar && n >= fenceLen && !info {
				inFence = false
				flush() // a closed fence is a complete block
			}
			continue
		}
		if c, n, _ := fenceLine(ln); n > 0 {
			flush() // a fence opening ends any paragraph it interrupts
			inFence, fenceChar, fenceLen = true, c, n
			cur = append(cur, ln)
			continue
		}
		if strings.TrimSpace(ln) == "" {
			flush() // a blank line terminates the current paragraph
			continue
		}
		cur = append(cur, ln)
	}
	// Whatever remains is the incomplete tail: an unclosed fence, or a paragraph
	// that no blank line has terminated. (It was never flushed, so it is exactly
	// the raw text after the last complete block.)
	return complete, strings.Join(cur, "\n")
}

// fenceLine reports whether ln is a fenced-code-block delimiter: the fence char
// (a backtick or tilde), the length of its opening run (>= 3, else 0), and
// whether an info string trails it. Up to three leading spaces are allowed (four
// is an indented code line, not a fence). info matters only for a CLOSING fence,
// which must carry none — an opening fence's info string (```go) is its
// language. A run under three, or a first non-space char that is neither fence
// char, is not a fence (length 0).
func fenceLine(ln string) (char byte, length int, info bool) {
	i := 0
	for i < len(ln) && i < 4 && ln[i] == ' ' {
		i++
	}
	if i >= 4 {
		return 0, 0, false // 4+ leading spaces is an indented code line
	}
	s := ln[i:]
	if len(s) < 3 || (s[0] != '`' && s[0] != '~') {
		return 0, 0, false
	}
	c := s[0]
	n := 0
	for n < len(s) && s[n] == c {
		n++
	}
	if n < 3 {
		return 0, 0, false
	}
	return c, n, strings.TrimSpace(s[n:]) != ""
}

// postProcessMarkdown turns glamour's single rendered string into transcript
// rows. glamour right-pads every line to the wrap width and frames blocks with
// blank lines; this strips the trailing padding (so a selection copy never
// grabs filler spaces — and code blocks copy verbatim, since only trailing
// whitespace is removed and interior indentation is untouched), collapses runs
// of blank rows to a single one, and drops leading/trailing blanks.
//
// The trailing-padding trim is ANSI-aware (trimTrailingPad): on a real
// terminal glamour wraps every padding space in its own color…reset group, so
// a naive strings.TrimRight can't reach the spaces. Trimming by DISPLAY width
// keeps the colored and Ascii renders structurally identical — stripping the
// colored output of its ANSI yields byte-for-byte the Ascii output, which the
// color-layout invariant (assertColorLayout: color must not change layout)
// depends on. When stripANSI is set (the Ascii/golden profile) every escape is
// removed first, guaranteeing a plain, deterministic row regardless of any
// attribute the style carries.
func postProcessMarkdown(out string, stripANSI bool) []string {
	raw := strings.Split(out, "\n")
	rows := make([]string, 0, len(raw))
	blankRun := false
	for _, line := range raw {
		if stripANSI {
			line = ansi.Strip(line)
		}
		line = trimTrailingPad(line)
		if line == "" {
			if len(rows) == 0 || blankRun {
				continue // drop leading blanks and collapse blank runs
			}
			blankRun = true
			rows = append(rows, "")
			continue
		}
		blankRun = false
		rows = append(rows, line)
	}
	// Drop a single trailing blank left by a blank run at the very end.
	if n := len(rows); n > 0 && rows[n-1] == "" {
		rows = rows[:n-1]
	}
	return rows
}

// trimTrailingPad removes a line's trailing spaces, ANSI-aware. It finds the
// display width of the line's visible content with trailing spaces removed,
// then truncates the (possibly ANSI-styled) line to that width — dropping the
// filler cells while keeping the retained cells' styling intact. glamour emits
// each padding space as its own self-closing color…reset group, so truncating
// at a cell boundary never leaves a dangling open color. A whitespace-only
// line yields "".
func trimTrailingPad(line string) string {
	trimmed := strings.TrimRight(ansi.Strip(line), " ")
	if trimmed == "" {
		return ""
	}
	w := ansi.StringWidth(trimmed)
	if ansi.StringWidth(line) <= w {
		return line
	}
	return ansi.Truncate(line, w, "")
}

// markdownStyle is glamour's DarkStyleConfig adapted to the TUI:
//
//   - Block margins and the H1 banner are removed so a message sits flush
//     against its "●" marker instead of glamour's default two-column document
//     indent, and inline-code padding is dropped so plain-profile output
//     carries no stray spaces.
//   - The document/paragraph BASE color is cleared so body prose renders in the
//     terminal's default foreground — matching the rest of the transcript's ink
//     and, importantly, leaving plain prose entirely ANSI-free on a real
//     terminal. Only genuine markdown ELEMENTS (headings, code, links,
//     emphasis) carry glamour's dark-theme color. This keeps the styled-golden
//     harness (testkit.TagANSI, which recognizes only the theme's marker
//     palette) valid for the existing prose fixtures: a plain-prose message adds
//     no unrecognized escape.
//
// Colors on the retained elements are stripped under the Ascii golden profile,
// so they never affect a plain golden file.
func markdownStyle() glamouransi.StyleConfig {
	zero := uint(0)
	sc := glamourstyles.DarkStyleConfig
	sc.Document.Margin = &zero
	sc.Document.BlockPrefix = ""
	sc.Document.BlockSuffix = ""
	sc.Document.Color = nil
	sc.Text.Color = nil
	sc.CodeBlock.Margin = &zero
	sc.Code.Prefix = ""
	sc.Code.Suffix = ""
	sc.H1.Prefix = "# "
	sc.H1.Suffix = ""
	sc.H1.BackgroundColor = nil
	return sc
}
