package tui

// resumepicker_test.go covers the /resume command-panel tab (resumepicker.go):
// the four mutually exclusive list states (loading / load error / empty store /
// filtered-to-nothing), the newest-first ordering, the live mark, the
// "age unknown" fallback, filter + row navigation, the height window that keeps
// the highlight on screen, and the degenerate small/zero-height renders a first
// frame produces. White-box (package tui) because resumePickerView is
// unexported — the App-level "/resume opens the panel / resumes an id" behavior
// is covered in session_commands_test.go (package tui_test).

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// resumeRefs is the picker's fixture list: deliberately out of order (the
// picker sorts), one row with an empty title and a zero Updated (the
// "(untitled)" + "age unknown" fallbacks), and one id that also appears in
// GoldenRoster so the live mark has something to mark.
func resumeRefs() []SessionRef {
	return []SessionRef{
		{
			ID:      "01928bee-old0-7000-8000-000000000003",
			Title:   "draft the release notes",
			Cwd:     "/home/j/notes",
			Updated: GoldenNow.Add(-3 * 24 * time.Hour),
		},
		{
			ID:      "0192a1b2-app0-7000-8000-000000000001",
			Title:   "wire the app root",
			Cwd:     "/home/j/orchestration",
			Updated: GoldenNow.Add(-2 * time.Minute),
		},
		{
			ID:  "01927f04-bare-7000-8000-000000000004",
			Cwd: "",
		},
		{
			ID:      "0192a0c4-mid0-7000-8000-000000000002",
			Title:   "review the supervisor contract",
			Cwd:     "/home/j/orchestration",
			Updated: GoldenNow.Add(-90 * time.Minute),
		},
	}
}

// loadedPicker returns a picker over resumeRefs with GoldenRoster's live ids
// marked — the state every list-shape assertion below starts from.
func loadedPicker() resumePickerView {
	return newResumePickerView(theme.Test(), GoldenNow, GoldenRoster()).withSessions(resumeRefs())
}

func renderResume(t *testing.T, name string, v resumePickerView) {
	t.Helper()
	testkit.AssertGolden(t, name, testkit.Render(v, testkit.Width, panelBodyRows))
}

// TestGoldenResumeLoading pins the pre-load state: the picker opens before its
// listing has arrived and must say so, never render an empty list as "no
// sessions".
func TestGoldenResumeLoading(t *testing.T) {
	renderResume(t, "resume_loading", newResumePickerView(theme.Test(), GoldenNow, GoldenRoster()))
}

// TestGoldenResumeLoaded pins the populated list: newest-first, the live mark
// on the roster-held row, "(untitled)"/"age unknown" for the bare row, and the
// project base name where a cwd is known.
func TestGoldenResumeLoaded(t *testing.T) {
	renderResume(t, "resume_loaded", loadedPicker())
}

// TestGoldenResumeFiltered pins typing into the filter box: the list narrows on
// title/id/cwd substring and the highlight drops.
func TestGoldenResumeFiltered(t *testing.T) {
	renderResume(t, "resume_filtered", loadedPicker().typeFilter("supervisor"))
}

// TestGoldenResumeSelected pins the row highlight after ↓↓.
func TestGoldenResumeSelected(t *testing.T) {
	renderResume(t, "resume_selected", loadedPicker().selectDown().selectDown())
}

// TestGoldenResumeEmpty pins the honest empty-store state, which is reachable
// only AFTER a successful listing came back with nothing.
func TestGoldenResumeEmpty(t *testing.T) {
	renderResume(t, "resume_empty", newResumePickerView(theme.Test(), GoldenNow, nil).withSessions(nil))
}

// TestGoldenResumeLoadError pins the failed-listing state: the reason is
// rendered, not swallowed into a blank list.
func TestGoldenResumeLoadError(t *testing.T) {
	v := newResumePickerView(theme.Test(), GoldenNow, nil).withLoadError(errors.New("daemonbridge: list sessions: connection refused"))
	renderResume(t, "resume_load_error", v)
}

// TestResumeStatesAreDistinct is the must-fire twin for the four golden states
// above: it asserts they render four DIFFERENT lines. Without it, a refactor
// that collapsed "loading" and "empty" into one message would leave every
// golden above passing (each still matches its own file) while the picker
// silently started claiming a store is empty before it has looked.
func TestResumeStatesAreDistinct(t *testing.T) {
	states := map[string]resumePickerView{
		"loading":   newResumePickerView(theme.Test(), GoldenNow, nil),
		"empty":     newResumePickerView(theme.Test(), GoldenNow, nil).withSessions(nil),
		"loadError": newResumePickerView(theme.Test(), GoldenNow, nil).withLoadError(errors.New("boom")),
		"noMatch":   loadedPicker().typeFilter("zzz-nothing-matches"),
	}
	seen := map[string]string{}
	for name, v := range states {
		line, ok := v.stateLine()
		if !ok {
			t.Fatalf("%s: expected a state line to replace the row list, got none", name)
		}
		if prev, dup := seen[line]; dup {
			t.Errorf("%s and %s render the identical state line %q — the two states are no longer distinguishable", name, prev, line)
		}
		seen[line] = name
	}
}

// TestResumeSortsNewestFirst pins the ordering contract the fixture is
// deliberately shuffled against: most recently active first, with an unknown
// (zero) timestamp sorting last rather than to the epoch's position at the top.
func TestResumeSortsNewestFirst(t *testing.T) {
	got := loadedPicker().filtered()
	want := []string{
		"0192a1b2-app0-7000-8000-000000000001", // 2m ago
		"0192a0c4-mid0-7000-8000-000000000002", // 90m ago
		"01928bee-old0-7000-8000-000000000003", // 3d ago
		"01927f04-bare-7000-8000-000000000004", // no timestamp
	}
	if len(got) != len(want) {
		t.Fatalf("filtered() returned %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Errorf("row %d = %s, want %s", i, got[i].ID, want[i])
		}
	}
}

// TestResumeSelectedFollowsFilter proves the Enter seam reads the FILTERED
// list, not the unfiltered one — the bug where a filter narrows what is on
// screen but Enter commits whatever the raw index happened to point at.
func TestResumeSelectedFollowsFilter(t *testing.T) {
	v := loadedPicker().typeFilter("notes").selectDown()
	ref, ok := v.selected()
	if !ok {
		t.Fatal("selected() reported nothing after ↓ onto the single matching row")
	}
	if ref.ID != "01928bee-old0-7000-8000-000000000003" {
		t.Errorf("selected() = %s, want the /home/j/notes row the filter left standing", ref.ID)
	}
}

// TestResumeSelectedEmptyStates covers every "Enter must do nothing" state: no
// highlight, an unloaded list, and a filter matching nothing.
func TestResumeSelectedEmptyStates(t *testing.T) {
	for name, v := range map[string]resumePickerView{
		"no highlight": loadedPicker(),
		"not loaded":   newResumePickerView(theme.Test(), GoldenNow, nil).selectDown(),
		"no match":     loadedPicker().typeFilter("zzz").selectDown(),
	} {
		if _, ok := v.selected(); ok {
			t.Errorf("%s: selected() reported a selection; Enter must be a no-op here", name)
		}
	}
}

// TestResumeWindowKeepsHighlightVisible pins the scrolling contract: with more
// sessions than body rows, walking the highlight down past the last visible row
// scrolls the window instead of moving the highlight off screen. A picker whose
// 12th row can never be seen is not a picker.
func TestResumeWindowKeepsHighlightVisible(t *testing.T) {
	refs := make([]SessionRef, 40)
	for i := range refs {
		refs[i] = SessionRef{
			ID:      fmt.Sprintf("0192a1b2-bulk-7000-8000-%012d", i),
			Title:   fmt.Sprintf("session %02d", i),
			Updated: GoldenNow.Add(-time.Duration(i) * time.Minute),
		}
	}
	v := newResumePickerView(theme.Test(), GoldenNow, nil).withSessions(refs)
	for range 25 {
		v = v.selectDown()
	}
	ref, ok := v.selected()
	if !ok {
		t.Fatal("selected() reported nothing after 25 ↓ presses")
	}
	got := testkit.Render(v, testkit.Width, panelBodyRows)
	if !strings.Contains(got, ref.Title) {
		t.Errorf("the highlighted row %q is not in the rendered window:\n%s", ref.Title, got)
	}
	if strings.Count(got, "\n")+1 > panelBodyRows {
		t.Errorf("rendered %d lines, want at most the %d-row body budget:\n%s", strings.Count(got, "\n")+1, panelBodyRows, got)
	}
}

// TestResumeRendersSmallAndZeroHeights is the first-frame guard: App can render
// before any WindowSizeMsg arrives, and the body budget it hands a panel on a
// short terminal can be 0 or negative. Every one of those must produce output,
// not a panic (a zero-height first frame has crashed this TUI before).
func TestResumeRendersSmallAndZeroHeights(t *testing.T) {
	views := map[string]resumePickerView{
		"loading": newResumePickerView(theme.Test(), GoldenNow, GoldenRoster()),
		"loaded":  loadedPicker(),
		"scrolled": func() resumePickerView {
			v := loadedPicker()
			return v.selectDown().selectDown().selectDown()
		}(),
	}
	for name, v := range views {
		for _, h := range []int{-2, -1, 0, 1, 2, 3} {
			for _, w := range []int{0, 1, 10, testkit.Width} {
				got := testkit.Render(v, w, h)
				if h == 0 && got != "" {
					t.Errorf("%s at %dx0: rendered %q, want nothing", name, w, got)
				}
				if h > 0 && strings.Count(got, "\n")+1 > h {
					t.Errorf("%s at %dx%d: rendered %d lines, want at most %d", name, w, h, strings.Count(got, "\n")+1, h)
				}
			}
		}
	}
}

// TestResumeNegativeHeightIsUnbounded pins why View's `height > 0` truncation
// guard is NOT redundant with its `height == 0` early return. A negative height
// is the [testkit.Renderable] convention for "unbounded", and it is a value
// [App.render] genuinely produces on a terminal too short for the panel's fixed
// chrome. Dropping the sign check would leave `len(lines) > height` true for
// every negative height and slice `lines[:height]` — an immediate panic, which
// is precisely the zero/first-frame class of crash this TUI has hit before.
func TestResumeNegativeHeightIsUnbounded(t *testing.T) {
	v := loadedPicker()
	full := len(v.lines(-1))
	if full <= 1 {
		t.Fatalf("the fixture rendered %d lines; this test needs a populated list to be meaningful", full)
	}
	for _, h := range []int{-1, -5, -100} {
		got := testkit.Render(v, testkit.Width, h)
		if lines := strings.Count(got, "\n") + 1; lines != full {
			t.Errorf("height %d rendered %d lines, want the full %d — a negative height means unbounded, not truncate-to-negative", h, lines, full)
		}
	}
}

// TestResumeEscapeClearsFilterThenBubbles pins the two-stage Esc contract the
// Config tab also has: the first Esc discards a filter, the second is left for
// the panel host to close on.
func TestResumeEscapeClearsFilterThenBubbles(t *testing.T) {
	v := loadedPicker().typeFilter("wire")
	v, consumed := v.handleEscape()
	if !consumed {
		t.Fatal("first Esc with a filter set was not consumed; it would have closed the panel")
	}
	if v.filter != "" {
		t.Errorf("filter = %q after Esc, want it cleared", v.filter)
	}
	if _, consumed = v.handleEscape(); consumed {
		t.Error("second Esc was consumed; it must bubble up and close the panel")
	}
}

// TestResumeBackspaceEditsFilter covers the filter's edit keys through the same
// handleKey entry point the panel host routes to.
func TestResumeBackspaceEditsFilter(t *testing.T) {
	v := loadedPicker()
	for _, r := range "wired" {
		v = v.handleKey(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	v = v.handleKey(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if v.filter != "wire" {
		t.Fatalf("filter = %q after typing \"wired\" and one backspace, want %q", v.filter, "wire")
	}
	if got := len(v.filtered()); got != 1 {
		t.Errorf("filter %q matched %d rows, want 1", v.filter, got)
	}
}
