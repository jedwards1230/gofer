package tui

// link_select_test.go covers Phase B of the transcript-render polish: native-
// like word selection (double-click a word, drag-extend word-by-word) and
// clickable markdown hyperlinks (OSC 8 emission + the modifier-click platform
// opener). White-box (package tui) because the selection state machine, the
// word-boundary helper, and the link helpers are all unexported.

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// --- B1: word selection -----------------------------------------------------

func TestWordBoundsCells(t *testing.T) {
	// "  wire the app" — leading spaces, then words separated by single spaces.
	const line = "  wire the app"
	cases := []struct {
		name         string
		x            int
		wantS, wantE int
		wantOK       bool
	}{
		{"inside first word", 3, 2, 6, true},   // "wire" spans [2,6)
		{"start of a word", 7, 7, 10, true},    // "the" spans [7,10)
		{"on a space run", 0, 0, 2, true},      // leading "  " spans [0,2)
		{"last word to EOL", 12, 11, 14, true}, // "app" spans [11,14)
		{"past end of line", 99, 0, 0, false},  // no cell there
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, e, ok := wordBoundsCells(line, tc.x)
			if ok != tc.wantOK || (ok && (s != tc.wantS || e != tc.wantE)) {
				t.Errorf("wordBoundsCells(%q, %d) = (%d, %d, %v), want (%d, %d, %v)",
					line, tc.x, s, e, ok, tc.wantS, tc.wantE, tc.wantOK)
			}
		})
	}
}

func TestIsDoubleClick(t *testing.T) {
	base := time.Now()
	cases := []struct {
		name         string
		prev         time.Time
		gap          time.Duration
		px, py, x, y int
		want         bool
	}{
		{"no prior click", time.Time{}, 0, 0, 0, 0, 0, false},
		{"same cell, within window", base, 100 * time.Millisecond, 5, 6, 5, 6, true},
		{"same cell, too slow", base, 2 * time.Second, 5, 6, 5, 6, false},
		{"different cell, fast", base, 100 * time.Millisecond, 5, 6, 6, 6, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isDoubleClick(tc.prev, tc.prev.Add(tc.gap), tc.px, tc.py, tc.x, tc.y); got != tc.want {
				t.Errorf("isDoubleClick = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDoubleClickSelectsWordAndDragExtends drives the app-owned selection: a
// double-click on the roster's "wire" row 6 selects the whole word, and a drag
// afterward extends the selection word-by-word into "the".
func TestDoubleClickSelectsWordAndDragExtends(t *testing.T) {
	a := newAppForGolden(t, newInternalFakeSup(GoldenRoster()))
	// Row 6 is "▸ wire the app root …": glyph+space at cols 0-1, "wire" [2,6),
	// space at 6, "the" [7,10).
	click := tea.MouseClickMsg{X: 3, Y: 6, Button: tea.MouseLeft}

	a, _ = a.handleMouseClick(click) // first click — plain
	a, _ = a.handleMouseClick(click) // second click — double, word-select

	if a.sel == nil || !a.sel.wordSelect {
		t.Fatalf("double-click did not enter word-select mode: %+v", a.sel)
	}
	if got := a.selectedText(); got != "wire" {
		t.Errorf("double-click selected %q, want %q", got, "wire")
	}

	// Drag into the next word — the selection extends word-by-word to cover
	// whole words, not the raw cursor cell.
	a = a.handleMouseMotion(tea.MouseMotionMsg{X: 8, Y: 6, Button: tea.MouseLeft})
	if got := a.selectedText(); got != "wire the" {
		t.Errorf("word-by-word drag selected %q, want %q", got, "wire the")
	}
}

// --- B2: clickable hyperlinks ----------------------------------------------

func TestLinkifyURLs(t *testing.T) {
	const url = "https://example.com/page"
	got := linkifyURLs("See " + url + " now")
	want := "See \x1b]8;;" + url + "\x1b\\" + url + "\x1b]8;;\x1b\\ now"
	if got != want {
		t.Errorf("linkifyURLs = %q, want %q", got, want)
	}
	if plain := "no links here"; linkifyURLs(plain) != plain {
		t.Errorf("linkifyURLs rewrote link-free text")
	}
}

// TestMarkdownLinkOSC8ProfileGated proves OSC 8 is emitted on a real color
// profile and NEVER under the Ascii golden profile (so goldens stay plain).
func TestMarkdownLinkOSC8ProfileGated(t *testing.T) {
	const msg = "See https://example.com/page for more."
	const osc8 = "\x1b]8;;https://example.com/page\x1b\\"

	color := ingested(testkit.ColorTheme(),
		event.NewMessageStarted("s", event.MessageText),
		event.NewMessageFinished("s", event.MessageText, msg))
	if got := testkit.Render(color, testkit.Width, testkit.Height); !strings.Contains(got, osc8) {
		t.Errorf("color render did not emit an OSC 8 hyperlink for the URL")
	}

	ascii := ingested(theme.Test(),
		event.NewMessageStarted("s", event.MessageText),
		event.NewMessageFinished("s", event.MessageText, msg))
	if got := testkit.Render(ascii, testkit.Width, testkit.Height); strings.Contains(got, "\x1b]8;;") {
		t.Errorf("Ascii render leaked an OSC 8 escape (goldens must stay plain)")
	}
}

func TestLinkAt(t *testing.T) {
	const url = "https://example.com"
	frame := "x " + "\x1b]8;;" + url + "\x1b\\" + url + "\x1b]8;;\x1b\\" + " y"
	// Visible layout: "x " (cols 0-1), URL (cols 2..20), " y" after.
	if got, ok := linkAt(frame, 5, 0); !ok || got != url {
		t.Errorf("linkAt on a link cell = (%q, %v), want (%q, true)", got, ok, url)
	}
	if _, ok := linkAt(frame, 0, 0); ok {
		t.Errorf("linkAt on a non-link cell reported a link")
	}
	if _, ok := linkAt(frame, 5, 3); ok {
		t.Errorf("linkAt on a nonexistent row reported a link")
	}
}

func TestOpenURLArgs(t *testing.T) {
	const url = "https://example.com"
	cases := map[string][]string{
		"darwin":  {"open", url},
		"linux":   {"xdg-open", url},
		"freebsd": {"xdg-open", url},
		"windows": {"rundll32", "url.dll,FileProtocolHandler", url},
	}
	for goos, want := range cases {
		got := openURLArgs(goos, url)
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("openURLArgs(%q) = %v, want %v", goos, got, want)
		}
	}
}

func TestOpenURLCmdRejectsNonHTTP(t *testing.T) {
	if cmd := openURLCmd("file:///etc/passwd"); cmd != nil {
		t.Errorf("openURLCmd opened a non-http(s) URL")
	}
	if cmd := openURLCmd("https://example.com"); cmd == nil {
		t.Errorf("openURLCmd refused a valid https URL")
	}
}

// TestModifierClickOpensLinkPlainClickSelects proves link-open composes with
// selection: a modifier+click on a rendered link returns an open command, while
// a plain click at the same cell starts a selection instead (no open command).
func TestModifierClickOpensLinkPlainClickSelects(t *testing.T) {
	meta := GoldenMeta()
	meta.AttachSessionID = "sess-x"
	a := NewApp(testkit.ColorTheme(), &internalFakeSup{}, meta, GoldenCommandEnv())
	mdl, _ := a.Update(tea.WindowSizeMsg{Width: testkit.Width, Height: testkit.Height})
	a = mdl.(App)
	mdl, _ = a.Update(sessEventMsg{id: "sess-x", ev: event.NewMessageStarted("sess-x", event.MessageText)})
	a = mdl.(App)
	mdl, _ = a.Update(sessEventMsg{
		id: "sess-x",
		ev: event.NewMessageFinished("sess-x", event.MessageText, "See https://example.com/page for more."),
	})
	a = mdl.(App)

	// Locate a cell that the render marked as a link (via the OSC 8 it emitted).
	frame := a.render()
	linkX, linkY := -1, -1
	for y, line := range strings.Split(frame, "\n") {
		for x := 0; x < len(line); x++ {
			if _, ok := linkAt(frame, x, y); ok {
				linkX, linkY = x, y
				break
			}
		}
		if linkX >= 0 {
			break
		}
	}
	if linkX < 0 {
		t.Fatalf("precondition: no link cell found in the color attach frame:\n%s", frame)
	}

	// Modifier+click opens the link.
	_, cmd := a.handleMouseClick(tea.MouseClickMsg{X: linkX, Y: linkY, Button: tea.MouseLeft, Mod: tea.ModCtrl})
	if cmd == nil {
		t.Errorf("Ctrl+click on a link returned no open command")
	}

	// Plain click at the same cell starts a selection instead of opening.
	after, cmd := a.handleMouseClick(tea.MouseClickMsg{X: linkX, Y: linkY, Button: tea.MouseLeft})
	if cmd != nil {
		t.Errorf("plain click on a link returned an open command; want selection")
	}
	if after.sel == nil {
		t.Errorf("plain click on a link did not start a selection")
	}
}
