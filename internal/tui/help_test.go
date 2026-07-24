package tui

// help_test.go covers /help's body (help.go). It is a WHITE-BOX test file
// (package tui) for one reason: the claim /help makes is that it renders from
// the LIVE command registry, and the only way to prove that rather than assert
// it is to build a registry this test invented and see the new command appear
// with no edit to help.go. A black-box test can only ever compare help's output
// against the builtins, which a hand-copied list would also satisfy.

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// TestGoldenHelpBody is the committed capture of the Help tab's first screenful
// at the panel's real body budget.
func TestGoldenHelpBody(t *testing.T) {
	v := newHelpView(theme.Test(), newBuiltinRegistry())
	testkit.AssertGolden(t, "help_body", v.View(testkit.Width, panelBodyRows))
}

// TestGoldenHelpBodyScrolled captures the tab after paging down, which is the
// only way the key half of the table is reachable — and the state where the
// "↑ N more · ↓ N more" affordance has to render both halves.
func TestGoldenHelpBodyScrolled(t *testing.T) {
	v := newHelpView(theme.Test(), newBuiltinRegistry())
	v = v.handleKey(tea.KeyPressMsg{Code: tea.KeyPgDown})
	testkit.AssertGolden(t, "help_body_scrolled", v.View(testkit.Width, panelBodyRows))
}

// TestHelpRendersAtDegenerateSizes is the first-frame guard. A bubbletea model
// renders once BEFORE any tea.WindowSizeMsg arrives, so width and height are
// both 0 on that frame — a zero-height first frame has panicked this TUI
// before. Anything that indexes into a line slice by height without checking it
// crashes here rather than in someone's terminal.
func TestHelpRendersAtDegenerateSizes(t *testing.T) {
	sizes := []struct{ w, h int }{
		{0, 0}, {80, 0}, {0, 24}, {1, 1}, {1, 2}, {80, 1}, {-1, -1}, {200, 500},
	}
	for _, size := range sizes {
		v := newHelpView(theme.Test(), newBuiltinRegistry())
		// Scrolled to the bottom as well as the top: the windowing arithmetic
		// differs on each side of the table.
		for _, scrolled := range []helpView{v, v.scroll(1000)} {
			got := scrolled.View(size.w, size.h)
			if size.h <= 0 && got != "" {
				t.Errorf("View(%d, %d) rendered %q; a non-positive height must render nothing", size.w, size.h, got)
			}
			if n := strings.Count(got, "\n") + 1; got != "" && size.h > 0 && n > size.h {
				t.Errorf("View(%d, %d) rendered %d lines, over its height budget", size.w, size.h, n)
			}
		}
	}
}

// TestHelpListsANewlyRegisteredCommand is the test that proves /help reads the
// live registry rather than a copy: it registers a command help.go has never
// heard of and expects to find it, with its summary and argument hint, in the
// rendered body. Hand-maintaining a parallel list would fail this.
func TestHelpListsANewlyRegisteredCommand(t *testing.T) {
	reg := newBuiltinRegistry()
	reg.register(Command{
		Name:    "zzz-invented-by-a-test",
		ArgHint: "[arg]",
		Summary: "a command help.go has never heard of",
	})

	v := newHelpView(theme.Test(), reg)
	// Render wide and tall enough for the whole table — the assertion is about
	// the CONTENT being derived from the registry, not about the viewport or
	// the width-truncation the goldens already pin.
	got := v.View(200, 500)

	for _, want := range []string{"/zzz-invented-by-a-test [arg]", "a command help.go has never heard of"} {
		if !strings.Contains(got, want) {
			t.Errorf("a newly registered command's %q is missing from /help — the body is not rendering "+
				"from Registry.List; got:\n%s", want, got)
		}
	}
}

// TestHelpListsEveryBuiltinCommand is the completeness half: every non-hidden
// registered command appears. Together with the test above it pins both
// directions — nothing invented, nothing dropped.
func TestHelpListsEveryBuiltinCommand(t *testing.T) {
	reg := newBuiltinRegistry()
	got := newHelpView(theme.Test(), reg).View(200, 500)
	for _, cmd := range reg.List() {
		if !strings.Contains(got, "/"+cmd.Name) {
			t.Errorf("/%s is registered but absent from /help:\n%s", cmd.Name, got)
		}
		if cmd.Summary != "" && !strings.Contains(got, cmd.Summary) {
			t.Errorf("/%s's summary %q is absent from /help", cmd.Name, cmd.Summary)
		}
	}
}

// TestHelpHidesHiddenCommands pins that /help honors Command.Hidden — the flag
// a future /debug relies on. It comes free from Registry.List, which is exactly
// why help renders through it.
func TestHelpHidesHiddenCommands(t *testing.T) {
	reg := newBuiltinRegistry()
	reg.register(Command{Name: "secret-debug-thing", Summary: "not for the palette", Hidden: true})

	if got := newHelpView(theme.Test(), reg).View(200, 500); strings.Contains(got, "secret-debug-thing") {
		t.Errorf("a Hidden command leaked into /help:\n%s", got)
	}
}

// TestHelpListsEveryKeymapRow is the key half's completeness check: every row
// declared in keymap.go reaches the rendered table. A scope added to the table
// but left out of keyScopeOrder would silently render nothing — this catches
// that.
func TestHelpListsEveryKeymapRow(t *testing.T) {
	got := newHelpView(theme.Test(), newBuiltinRegistry()).View(200, 500)
	for _, b := range keymap() {
		if !strings.Contains(got, b.Keys) {
			t.Errorf("binding %q (%s) is declared in keymap.go but absent from /help", b.Keys, b.Desc)
		}
		if !strings.Contains(got, b.Desc) {
			t.Errorf("binding %q's description %q is absent from /help", b.Keys, b.Desc)
		}
	}
}

// TestHelpScrollClamps pins the viewport bounds: scrolling past either end
// leaves at least one line on screen rather than an empty body or a panic.
func TestHelpScrollClamps(t *testing.T) {
	v := newHelpView(theme.Test(), newBuiltinRegistry())

	if up := v.scroll(-50); up.offset != 0 {
		t.Errorf("scrolling up from the top left offset %d, want 0", up.offset)
	}
	down := v.scroll(10_000)
	if want := len(v.lines()) - 1; down.offset != want {
		t.Errorf("scrolling past the end left offset %d, want %d", down.offset, want)
	}
	if got := down.View(testkit.Width, panelBodyRows); strings.TrimSpace(got) == "" {
		t.Error("scrolled to the end, the body rendered nothing")
	}
}
