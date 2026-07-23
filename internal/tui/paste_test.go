package tui_test

// paste_test.go covers bracketed paste (paste.go) end to end through App's
// exported tea.Model surface: one tea.PasteMsg carrying a whole payload lands
// at the focused surface's cursor on all three text-entry screens (overview
// dispatch bar, attach input, peek reply), never routed through the key
// handlers — so an embedded newline can't submit and a leading space can't
// close peek. The pure helpers behind it (clip/normalize/display) are
// unit-tested in paste_internal_test.go.

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// paste drives one bracketed-paste payload through Update the way press
// drives one key, executing any returned Cmd immediately.
func paste(t *testing.T, m tea.Model, s string) tea.Model {
	t.Helper()
	m, cmd := m.Update(tea.PasteMsg{Content: s})
	if cmd == nil {
		return m
	}
	m, _ = m.Update(cmd())
	return m
}

// frameRows is the rendered frame's row count — the layout-integrity measure
// a literal newline inside a one-line input would break.
func frameRows(m tea.Model) int { return len(strings.Split(content(m), "\n")) }

// TestPasteInsertsIntoDispatchBar is the base case: a single-line paste lands
// in the overview dispatch bar with the cursor after it, exactly as if it had
// been typed.
func TestPasteInsertsIntoDispatchBar(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = paste(t, m, "review the docs")

	if got := content(m); !strings.Contains(got, "❯ review the docs▏") {
		t.Fatalf("expected the pasted text at the dispatch-bar cursor, got:\n%s", got)
	}
}

// TestPasteMultiLineDoesNotSubmit is the acceptance bar for #150: a paste
// carrying newlines must NOT be read as Enter presses — no session is
// created, the app stays on the overview, and the frame keeps its row count
// (a literal newline in the one-line dispatch bar would add rows).
func TestPasteMultiLineDoesNotSubmit(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := newTestApp(t, sup)
	rowsBefore := frameRows(m)

	m = paste(t, m, "first line\nsecond line\nthird line")

	if len(sup.created) != 0 {
		t.Fatalf("multi-line paste submitted %d session(s): %v; want none", len(sup.created), sup.created)
	}
	if got := content(m); strings.Contains(got, "> ▏") {
		t.Fatalf("expected to stay on the overview after a multi-line paste, got:\n%s", got)
	}
	if got := frameRows(m); got != rowsBefore {
		t.Fatalf("frame grew from %d to %d rows: a pasted newline broke the layout:\n%s", rowsBefore, got, content(m))
	}
	if got := content(m); !strings.Contains(got, "❯ first line␊second line␊third line▏") {
		t.Fatalf("expected the newlines rendered as ␊ pictures on one row, got:\n%s", got)
	}

	// The buffer keeps the REAL newlines: submitting it dispatches the pasted
	// text intact, not a flattened or truncated copy.
	press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(sup.created) != 1 {
		t.Fatalf("created %d session(s) after Enter, want 1", len(sup.created))
	}
	if want := "first line\nsecond line\nthird line"; sup.created[0] != want {
		t.Fatalf("created with %q, want %q", sup.created[0], want)
	}
}

// TestPasteIntoMiddleOfBuffer pins the splice: the payload goes in at the
// cursor, not at the end, and the cursor lands immediately after it.
func TestPasteIntoMiddleOfBuffer(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = type_(t, m, "abcd")
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyLeft})
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyLeft})
	m = paste(t, m, "XY")

	if got := content(m); !strings.Contains(got, "❯ abXY▏cd") {
		t.Fatalf("expected the paste spliced at the cursor, got:\n%s", got)
	}

	// Typing after the paste proves the cursor really sits after it rather
	// than merely rendering there.
	m = press(t, m, tea.KeyPressMsg{Text: "Z"})
	if got := content(m); !strings.Contains(got, "❯ abXYZ▏cd") {
		t.Fatalf("expected the cursor left immediately after the paste, got:\n%s", got)
	}
}

// TestPasteNavigationCharactersInsertedLiterally covers a payload made of
// characters that are otherwise bindings — newline (Enter: submit), tab
// (toggle view), escape (clear the buffer), space — none of which may be
// interpreted. All of them survive into the submitted text.
func TestPasteNavigationCharactersInsertedLiterally(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := newTestApp(t, sup)

	const payload = "a\tb\nc\x1bd e"
	m = paste(t, m, payload)

	if len(sup.created) != 0 {
		t.Fatalf("paste of binding characters submitted %v; want nothing", sup.created)
	}
	if got := content(m); !strings.Contains(got, "❯ a␉b␊c␛d e▏") {
		t.Fatalf("expected control characters rendered as pictures, got:\n%s", got)
	}

	press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(sup.created) != 1 || sup.created[0] != payload {
		t.Fatalf("created = %v, want exactly [%q]", sup.created, payload)
	}
}

// TestPasteCarriageReturnsNormalized covers the terminal encoding: bracketed
// paste commonly delivers a line break as CR (or CRLF), which must become a
// plain newline rather than reaching the terminal as a column-0 return.
func TestPasteCarriageReturnsNormalized(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := newTestApp(t, sup)

	m = paste(t, m, "one\r\ntwo\rthree")
	press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if len(sup.created) != 1 {
		t.Fatalf("created %d session(s), want 1", len(sup.created))
	}
	if want := "one\ntwo\nthree"; sup.created[0] != want {
		t.Fatalf("created with %q, want %q", sup.created[0], want)
	}
}

// TestPasteIntoAttachInput covers the second surface: the attach input takes
// a paste at its own cursor and sends it intact.
func TestPasteIntoAttachInput(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := newTestApp(t, sup)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // attach
	rowsBefore := frameRows(m)

	m = paste(t, m, "line one\nline two")

	if len(sup.sent) != 0 {
		t.Fatalf("multi-line paste sent %v; want nothing before Enter", sup.sent)
	}
	if got := frameRows(m); got != rowsBefore {
		t.Fatalf("frame grew from %d to %d rows on the attach screen:\n%s", rowsBefore, got, content(m))
	}
	if got := content(m); !strings.Contains(got, "> line one␊line two▏") {
		t.Fatalf("expected the paste at the attach input's cursor, got:\n%s", got)
	}

	press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(sup.sent) != 1 || !strings.HasSuffix(sup.sent[0], ":line one\nline two") {
		t.Fatalf("sent = %v, want the pasted text intact", sup.sent)
	}
}

// TestPasteIntoPeekReply covers the third text-entry surface: peek's ❯ reply.
// Its first character is a space — the peek binding that closes back to the
// overview — which must be inserted, not obeyed.
func TestPasteIntoPeekReply(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := newTestApp(t, sup)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeySpace}) // peek
	rowsBefore := frameRows(m)

	m = paste(t, m, " reply one\nreply two")

	got := content(m)
	if !strings.Contains(got, "space/esc to close") {
		t.Fatalf("expected to stay on peek (a pasted leading space must not close it), got:\n%s", got)
	}
	if !strings.Contains(got, "❯  reply one␊reply two▏") {
		t.Fatalf("expected the paste in the peek reply, got:\n%s", got)
	}
	if rows := frameRows(m); rows != rowsBefore {
		t.Fatalf("frame grew from %d to %d rows on peek:\n%s", rowsBefore, rows, got)
	}

	press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(sup.sent) != 1 || !strings.HasSuffix(sup.sent[0], ": reply one\nreply two") {
		t.Fatalf("sent = %v, want the pasted reply intact", sup.sent)
	}
}

// TestPasteClippedAtConfiguredLimit covers the tui.max_paste_bytes guard: an
// oversized paste is clipped rather than inserted whole, and the clip is
// reported on the status line instead of silently swallowing content.
func TestPasteClippedAtConfiguredLimit(t *testing.T) {
	limit := 8
	env := tui.GoldenCommandEnv()
	env.Config = func() (config.Config, error) {
		return config.Config{TUI: config.TUI{MaxPasteBytes: &limit}}, nil
	}

	sup := newFakeSup(tui.GoldenRoster())
	var m tea.Model = tui.NewApp(theme.Test(), sup, tui.GoldenMeta(), env)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m, _ = m.Update(m.Init()())

	m = paste(t, m, "0123456789abcdef")

	got := content(m)
	if !strings.Contains(got, "❯ 01234567▏") {
		t.Fatalf("expected the paste clipped to %d bytes, got:\n%s", limit, got)
	}
	if !strings.Contains(got, "paste clipped to 8 bytes (tui.max_paste_bytes)") {
		t.Fatalf("expected the clip reported on the status line, got:\n%s", got)
	}
}

// TestPasteUnlimitedWhenConfiguredZero pins the escape hatch: an explicit 0
// means no cap at all, so nothing is clipped.
func TestPasteUnlimitedWhenConfiguredZero(t *testing.T) {
	unlimited := 0
	env := tui.GoldenCommandEnv()
	env.Config = func() (config.Config, error) {
		return config.Config{TUI: config.TUI{MaxPasteBytes: &unlimited}}, nil
	}

	var m tea.Model = tui.NewApp(theme.Test(), newFakeSup(tui.GoldenRoster()), tui.GoldenMeta(), env)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m, _ = m.Update(m.Init()())

	m = paste(t, m, strings.Repeat("x", config.DefaultMaxPasteBytes+64))

	if got := content(m); strings.Contains(got, "paste clipped") {
		t.Fatalf("expected no clip with tui.max_paste_bytes=0, got:\n%s", got)
	}
}

// TestPasteOpensAutocompleteMenu pins that a pasted command token syncs the
// autocomplete menu the same way a typed one does — paste is the typed-text
// path with the key interpretation removed, not a bypass around it.
func TestPasteOpensAutocompleteMenu(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = paste(t, m, "/conf")

	if got := content(m); !strings.Contains(got, "/config") {
		t.Fatalf("expected the pasted command token to open the autocomplete menu, got:\n%s", got)
	}
}

// TestPasteIgnoredWhileCommandPanelOpen documents the deliberate limit: an
// open command panel owns the keyboard (its filter/edit line has no
// cursor-aware buffer), so a paste is a no-op there — the same as a typed
// rune never reaching the screen underneath — and must not leak into the
// dispatch bar hidden behind it.
func TestPasteIgnoredWhileCommandPanelOpen(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = dispatchSlash(t, m, "/status")

	m = paste(t, m, "leaked")
	if got := content(m); strings.Contains(got, "leaked") {
		t.Fatalf("expected the paste ignored while the command panel is open, got:\n%s", got)
	}

	// The panel BLANKS the dispatch bar's rows while it is open, so "not
	// rendered" alone proves nothing — close the panel and check the bar the
	// paste would have landed in was never touched.
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEscape})
	got := content(m)
	if !strings.Contains(got, "space peek") {
		t.Fatalf("expected the panel closed back to the overview, got:\n%s", got)
	}
	if strings.Contains(got, "leaked") {
		t.Fatalf("the paste leaked into the dispatch bar hidden behind the panel, got:\n%s", got)
	}
}

// TestPasteClippedRendersAsACaveat is TestPasteClippedAtConfiguredLimit's
// color-state counterpart (issue #161). A clipped paste is a CAVEAT, not a
// failure: the paste landed, it was just truncated at tui.max_paste_bytes. It
// used to render in the error style like every other status note, which reads
// as "your paste was rejected".
//
// Rendered through testkit.ColorTheme because theme.Test's forced Ascii
// profile emits no escapes at all, so the plain test above cannot see this.
func TestPasteClippedRendersAsACaveat(t *testing.T) {
	limit := 8
	env := tui.GoldenCommandEnv()
	env.Config = func() (config.Config, error) {
		return config.Config{TUI: config.TUI{MaxPasteBytes: &limit}}, nil
	}

	sup := newFakeSup(tui.GoldenRoster())
	var m tea.Model = tui.NewApp(testkit.ColorTheme(), sup, tui.GoldenMeta(), env)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m, _ = m.Update(m.Init()())

	m = paste(t, m, "0123456789abcdef")

	tagged := testkit.TagANSI(t, content(m))
	if !strings.Contains(tagged, "<yellow>paste clipped to 8 bytes") {
		t.Fatalf("expected the clip note in the warn style, not the error style; tagged render:\n%s", tagged)
	}
}
