// Package theme defines the small semantic token palette gofer's TUI renders
// through, plus the color-profile gate that lets golden tests force
// deterministic, colorless output.
//
// Per docs/TUI.md, area styles are functions of tokens, not pre-baked struct
// fields: callers ask a [Theme] for a [Style] (e.g. theme.Accent()) rather
// than reaching into a fixed set of struct fields, which keeps the token set
// free to grow without every call site changing shape.
package theme

import (
	"charm.land/lipgloss/v2"
	"github.com/muesli/termenv"
)

// Theme is the token palette a component renders through. Colors are hex
// strings ("#rrggbb"); Profile gates whether [Theme]'s Style methods apply
// color/attribute codes at all.
//
// Under [termenv.Ascii] (see [Test]), every Style method returns an
// unstyled [lipgloss.Style]: no ANSI escape ever reaches the rendered
// string, so golden files stay plain text.
type Theme struct {
	Profile termenv.Profile

	BG     string
	Panel  string
	Ink    string
	Muted  string
	Accent string
	OK     string
	Warn   string
	Danger string

	// State markers. Plain runes, not styled output — color is applied by the
	// caller (marker-only styling), so the glyph carries state only through the
	// style it is rendered in.
	GlyphHuman string // ○ — a human message (the only hollow circle)
	GlyphAgent string // ● — everything else; color carries the state
}

// Test returns a fixed theme for golden-file tests: [termenv.Ascii] forced,
// so every Style method is a no-op and rendered output is plain text,
// independent of the terminal running the test.
func Test() Theme {
	return Theme{
		Profile: termenv.Ascii,

		BG:     "#1e1e2e",
		Panel:  "#313244",
		Ink:    "#cdd6f4",
		Muted:  "#6c7086",
		Accent: "#89b4fa",
		OK:     "#a6e3a1",
		Warn:   "#f9e2af",
		Danger: "#f38ba8",

		GlyphHuman: "○",
		GlyphAgent: "●",
	}
}

// Default returns a theme with the color profile detected from the process
// environment, for the live bubbletea adapter. Golden tests should use
// [Test], never Default — environment detection is exactly the
// nondeterminism golden tests exist to avoid.
func Default() Theme {
	t := Test()
	t.Profile = termenv.EnvColorProfile()
	return t
}

// colored builds a base [lipgloss.Style] with a foreground color, or an
// unstyled Style when the theme's profile is [termenv.Ascii].
func (t Theme) colored(hex string) lipgloss.Style {
	s := lipgloss.NewStyle()
	if t.Profile == termenv.Ascii {
		return s
	}
	return s.Foreground(lipgloss.Color(hex))
}

// Ink styles primary text.
func (t Theme) InkStyle() lipgloss.Style { return t.colored(t.Ink) }

// MutedStyle styles secondary/dimmed text, such as streamed reasoning.
func (t Theme) MutedStyle() lipgloss.Style { return t.colored(t.Muted) }

// AccentStyle styles interactive/emphasized elements (the input cursor,
// active tool names).
func (t Theme) AccentStyle() lipgloss.Style { return t.colored(t.Accent) }

// OKStyle styles success states.
func (t Theme) OKStyle() lipgloss.Style { return t.colored(t.OK) }

// WarnStyle styles cautionary states (pending approvals).
func (t Theme) WarnStyle() lipgloss.Style { return t.colored(t.Warn) }

// DangerStyle styles error/failure states.
func (t Theme) DangerStyle() lipgloss.Style { return t.colored(t.Danger) }

// SelectionStyle styles the mouse click-drag selection highlight (reverse
// video). Unlike the marker-vocabulary colors above, this is NOT gated by
// [Theme.Profile]: reverse video is a text attribute every ANSI terminal
// supports, including one with no color capability at all, so a selection
// stays visible even under [Test]'s forced Ascii profile — only an actual
// mouse drag ever sets it, so no existing Ascii golden exercises this path
// or would gain unexpected escape codes from it.
func (t Theme) SelectionStyle() lipgloss.Style { return lipgloss.NewStyle().Reverse(true) }
