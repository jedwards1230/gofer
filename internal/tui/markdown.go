package tui

// markdown.go renders a SETTLED assistant-text item's markdown to styled
// terminal rows via Charm's glamour (the library behind `glow`) — bold/italic,
// headings, lists, blockquotes, inline code, links, and fenced code blocks.
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
//     keystroke, so a settled message is rendered once and memoized by
//     (width, text). The cache is cleared whenever the width changes (a
//     resize), which also bounds its growth.
//
// Only settled text is rendered here — a streaming (still-open) item renders
// its raw deltas plainly (see renderItemLines), because re-running glamour on
// every delta would flicker and lag.

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
