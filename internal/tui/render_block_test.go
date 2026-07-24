package tui

// render_block_test.go pins the shared content-block grammar (marker glyph +
// header, └-gutter body, "… +N lines" collapse, per-row styling) the tool /
// background-agents / shell-run blocks all route through renderBlock. The three
// callers each get their own golden coverage elsewhere; this exercises the
// primitive directly so a regression in the grammar is caught at the seam.

import (
	"reflect"
	"testing"

	"github.com/jedwards1230/gofer/internal/tui/theme"
)

func TestRenderBlock(t *testing.T) {
	m := New(theme.Test()) // Ascii profile: marker styling is a no-op
	// tag wraps the whole prefixed row so a mixed-styling body is observable
	// even under the Ascii golden profile — this is exactly how the shell
	// block distinguishes a plain output row from a danger/muted one.
	tag := func(name string) func(string) string {
		return func(s string) string { return "<" + name + ">" + s }
	}

	tests := []struct {
		name  string
		block contentBlock
		want  []string
	}{
		{
			name:  "header only",
			block: contentBlock{marker: m.theme.AccentStyle(), glyph: "●", header: "2 agents"},
			want:  []string{"● 2 agents"},
		},
		{
			name: "body with continuation",
			block: contentBlock{
				marker: m.theme.AccentStyle(), glyph: "●", header: "hdr",
				rows: []blockRow{{text: "a"}, {text: "b"}, {text: "c"}},
			},
			want: []string{"● hdr", "   └ a", "     b", "     c"},
		},
		{
			name: "collapse at maxBody",
			block: contentBlock{
				marker: m.theme.AccentStyle(), glyph: "●", header: "hdr",
				rows:    []blockRow{{text: "a"}, {text: "b"}, {text: "c"}, {text: "d"}, {text: "e"}},
				maxBody: 3,
			},
			want: []string{"● hdr", "   └ a", "     b", "     c", "     … +2 lines"},
		},
		{
			name: "exactly maxBody does not collapse",
			block: contentBlock{
				marker: m.theme.AccentStyle(), glyph: "●", header: "hdr",
				rows:    []blockRow{{text: "a"}, {text: "b"}, {text: "c"}},
				maxBody: 3,
			},
			want: []string{"● hdr", "   └ a", "     b", "     c"},
		},
		{
			name: "per-row mixed styling",
			block: contentBlock{
				marker: m.theme.AccentStyle(), glyph: "$", header: "cat x",
				rows: []blockRow{
					{text: "output"},                     // plain (nil render)
					{text: "exit 1", render: tag("d")},   // danger
					{text: "· queued", render: tag("m")}, // muted disposition
				},
			},
			// nil render leaves the row plain; tag renders wrap the WHOLE
			// prefixed row, gutter included.
			want: []string{"$ cat x", "   └ output", "<d>     exit 1", "<m>     · queued"},
		},
		{
			name: "multi-line header splits per row",
			block: contentBlock{
				marker: m.theme.AccentStyle(), glyph: "$", header: "line1\nline2",
				rows: []blockRow{{text: "out"}},
			},
			// a heredoc/multi-statement header still counts one slice entry per
			// physical row (styledMarkerLines), continuation aligned under $.
			want: []string{"$ line1", "  line2", "   └ out"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := m.renderBlock(tt.block)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("renderBlock() =\n%#v\nwant\n%#v", got, tt.want)
			}
		})
	}
}
