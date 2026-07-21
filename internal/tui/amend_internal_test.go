package tui

// amend_internal_test.go lives in package tui because it drives the amend
// editor's unexported core directly — [amendEditor]'s key handling and
// [Model.AmendedInput]'s spec re-marshalling. The App-level key routing (Tab,
// the key capture, ctrl+s/esc) is exercised through App.Update in
// app_internal_test.go; the rendered result is locked by the approval_amending
// goldens in golden_test.go.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// editorFor opens an editor over body under the "cmd" key — the shape every
// case below starts from.
func editorFor(body string) amendEditor { return newAmendEditor("cmd", body) }

// key builds the tea.Key an editor test feeds applyKey, matching the shapes a
// real terminal delivers (see input_keymap.go's doc).
func key(code rune, mod tea.KeyMod) tea.Key { return tea.Key{Code: code, Mod: mod} }

// TestAmendEditorSeedsAndSplits covers the prefill contract: a multi-line
// body becomes one buffer per physical line, the cursor opens at the END of
// the last one, and Text() rejoins them byte-identically.
func TestAmendEditorSeedsAndSplits(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantLines []string
		wantLine  int
		wantCol   int
	}{
		{"single line", "go test ./...", []string{"go test ./..."}, 0, 13},
		{"empty body still has a line", "", []string{""}, 0, 0},
		{
			name:      "multi line",
			body:      "go test \\\n  -run TestAmend",
			wantLines: []string{"go test \\", "  -run TestAmend"},
			wantLine:  1,
			wantCol:   16,
		},
		{"trailing newline keeps the empty tail line", "ls\n", []string{"ls", ""}, 1, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ed := editorFor(tc.body)
			got := make([]string, len(ed.lines))
			for i, l := range ed.lines {
				got[i] = l.String()
			}
			if !reflect.DeepEqual(got, tc.wantLines) {
				t.Errorf("lines = %q, want %q", got, tc.wantLines)
			}
			if ed.cursorLine != tc.wantLine || ed.cur().Cursor() != tc.wantCol {
				t.Errorf("cursor = line %d col %d, want line %d col %d",
					ed.cursorLine, ed.cur().Cursor(), tc.wantLine, tc.wantCol)
			}
			if ed.Text() != tc.body {
				t.Errorf("Text() = %q, want the body back verbatim %q", ed.Text(), tc.body)
			}
		})
	}
}

// TestAmendEditorKeys covers the editor's own keymap: insertion and deletion,
// Enter's line break and the backspace that takes it back, the line-crossing
// arrows, and ↑/↓ with their column clamp. Each case names the resulting text
// and cursor position, since "the text is right but the cursor moved
// somewhere else" is the failure mode a text-only assertion misses.
func TestAmendEditorKeys(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		keys     []tea.Key
		wantText string
		wantLine int
		wantCol  int
	}{
		{
			name:     "typing appends at the cursor",
			body:     "ls",
			keys:     []tea.Key{{Text: " -la"}},
			wantText: "ls -la",
			wantLine: 0, wantCol: 6,
		},
		{
			name:     "backspace deletes within a line",
			body:     "rm -rf /",
			keys:     []tea.Key{{Code: tea.KeyBackspace}, {Code: tea.KeyBackspace}},
			wantText: "rm -rf",
			wantLine: 0, wantCol: 6,
		},
		{
			name:     "enter breaks the line at the cursor",
			body:     "ab",
			keys:     []tea.Key{{Code: tea.KeyLeft}, {Code: tea.KeyEnter}},
			wantText: "a\nb",
			wantLine: 1, wantCol: 0,
		},
		{
			name:     "backspace at column 0 joins the previous line",
			body:     "a\nb",
			keys:     []tea.Key{{Code: tea.KeyHome}, {Code: tea.KeyBackspace}},
			wantText: "ab",
			wantLine: 0, wantCol: 1,
		},
		{
			name:     "left at a line start wraps to the previous line's end",
			body:     "abc\nxy",
			keys:     []tea.Key{{Code: tea.KeyHome}, {Code: tea.KeyLeft}, {Text: "Z"}},
			wantText: "abcZ\nxy",
			wantLine: 0, wantCol: 4,
		},
		{
			name:     "right at a line end wraps to the next line's start",
			body:     "abc\nxy",
			keys:     []tea.Key{{Code: tea.KeyUp}, {Code: tea.KeyEnd}, {Code: tea.KeyRight}, {Text: "Z"}},
			wantText: "abc\nZxy",
			wantLine: 1, wantCol: 1,
		},
		{
			name:     "left at the very start is a no-op",
			body:     "abc",
			keys:     []tea.Key{{Code: tea.KeyHome}, {Code: tea.KeyLeft}},
			wantText: "abc",
			wantLine: 0, wantCol: 0,
		},
		{
			name:     "right at the very end is a no-op",
			body:     "abc",
			keys:     []tea.Key{{Code: tea.KeyRight}},
			wantText: "abc",
			wantLine: 0, wantCol: 3,
		},
		{
			name:     "up keeps the column",
			body:     "abcd\nwxyz",
			keys:     []tea.Key{{Code: tea.KeyHome}, {Code: tea.KeyRight}, {Code: tea.KeyRight}, {Code: tea.KeyUp}, {Text: "!"}},
			wantText: "ab!cd\nwxyz",
			wantLine: 0, wantCol: 3,
		},
		{
			name:     "down clamps the column to a shorter line",
			body:     "abcdef\nxy",
			keys:     []tea.Key{{Code: tea.KeyUp}, {Code: tea.KeyEnd}, {Code: tea.KeyDown}, {Text: "!"}},
			wantText: "abcdef\nxy!",
			wantLine: 1, wantCol: 3,
		},
		{
			name:     "up on the first line is a no-op",
			body:     "abc",
			keys:     []tea.Key{{Code: tea.KeyUp}},
			wantText: "abc",
			wantLine: 0, wantCol: 3,
		},
		{
			name:     "down on the last line is a no-op",
			body:     "abc",
			keys:     []tea.Key{{Code: tea.KeyDown}},
			wantText: "abc",
			wantLine: 0, wantCol: 3,
		},
		{
			// The whole reason the editor is built over inputBuffer: the
			// app's shared readline keymap (input_keymap.go) applies inside a
			// line for free. ctrl+u is a key ONLY applyInputKey binds — if the
			// delegation were dropped this row goes red on its own.
			name:     "ctrl+u delegates to the shared input keymap",
			body:     "rm -rf /tmp/x",
			keys:     []tea.Key{key('u', tea.ModCtrl), {Text: "ls"}},
			wantText: "ls",
			wantLine: 0, wantCol: 2,
		},
		{
			name:     "alt+backspace deletes a word through the shared keymap",
			body:     "go test ./...",
			keys:     []tea.Key{{Code: tea.KeyBackspace, Mod: tea.ModAlt}},
			wantText: "go test ",
			wantLine: 0, wantCol: 8,
		},
		{
			name:     "an unbound key leaves the editor untouched",
			body:     "ls",
			keys:     []tea.Key{{Code: tea.KeyTab}},
			wantText: "ls",
			wantLine: 0, wantCol: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ed := editorFor(tc.body)
			for _, k := range tc.keys {
				ed = ed.applyKey(k)
			}
			if got := ed.Text(); got != tc.wantText {
				t.Errorf("Text() = %q, want %q", got, tc.wantText)
			}
			if ed.cursorLine != tc.wantLine || ed.cur().Cursor() != tc.wantCol {
				t.Errorf("cursor = line %d col %d, want line %d col %d",
					ed.cursorLine, ed.cur().Cursor(), tc.wantLine, tc.wantCol)
			}
		})
	}
}

// TestAmendEditorInsertTextSpansLines covers the paste path: a payload with
// its own newlines lands as real lines, exactly as typing it would.
func TestAmendEditorInsertTextSpansLines(t *testing.T) {
	ed := editorFor("go test").insertText(" \\\n  -race")
	if want := "go test \\\n  -race"; ed.Text() != want {
		t.Errorf("Text() = %q, want %q", ed.Text(), want)
	}
	if ed.cursorLine != 1 || ed.cur().Cursor() != 7 {
		t.Errorf("cursor = line %d col %d, want line 1 col 7", ed.cursorLine, ed.cur().Cursor())
	}
}

// TestAmendEditorVisibleLinesScrollsToCursor pins the row budget: an editor
// longer than the cap shows a window of exactly cap lines that always
// contains the cursor line, scrolled the minimum distance to get there. A
// window that dropped the cursor would leave the user typing off-screen.
func TestAmendEditorVisibleLinesScrollsToCursor(t *testing.T) {
	body := strings.Repeat("x\n", 9) + "x" // 10 lines
	tests := []struct {
		name              string
		cursorLine, limit int
		wantStart         int
		wantEnd           int
	}{
		{"under the cap shows everything", 3, 12, 0, 10},
		{"uncapped shows everything", 3, 0, 0, 10},
		{"cursor near the top pins the window at the top", 1, 4, 0, 4},
		{"cursor past the window scrolls just far enough", 5, 4, 2, 6},
		{"cursor on the last line pins the window at the bottom", 9, 4, 6, 10},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ed := editorFor(body)
			ed.cursorLine = tc.cursorLine
			start, end := ed.visibleLines(tc.limit)
			if start != tc.wantStart || end != tc.wantEnd {
				t.Fatalf("visibleLines(%d) = [%d,%d), want [%d,%d)", tc.limit, start, end, tc.wantStart, tc.wantEnd)
			}
			if tc.cursorLine < start || tc.cursorLine >= end {
				t.Errorf("cursor line %d is outside the visible window [%d,%d)", tc.cursorLine, start, end)
			}
		})
	}
}

// amendingModel returns a Model with a pending approval over spec whose amend
// editor is open, failing the test if the spec had nothing to amend.
func amendingModel(t *testing.T, spec map[string]any) Model {
	t.Helper()
	m := New(theme.Test()).Ingest(event.NewPermissionRequested("sess-1", "perm-1", "bash", spec, nil))
	next, ok := m.BeginApprovalAmend()
	if !ok {
		t.Fatalf("BeginApprovalAmend over spec %v: want an editor, got none", spec)
	}
	return next
}

// TestAmendedInputPreservesEveryOtherKey is the load-bearing test of this
// feature. The SDK substitutes event.PermissionReply.Input into the call
// WHOLESALE (loop.awaitApproval assigns it to call.Input), so a reply
// carrying only the edited command would silently erase every other argument
// the model passed. The amended input must therefore be the full original
// spec with exactly one value replaced.
func TestAmendedInputPreservesEveryOtherKey(t *testing.T) {
	spec := map[string]any{
		"cmd":         "rm -rf /tmp/x",
		"timeout":     float64(120),
		"description": "clean the scratch dir",
		"env":         map[string]any{"CI": "1"},
	}
	m := amendingModel(t, spec)
	m = m.ApplyApprovalAmendKey(tea.Key{Text: "1"}) // "rm -rf /tmp/x1"

	raw, ok, err := m.AmendedInput()
	if err != nil || !ok {
		t.Fatalf("AmendedInput() = (_, %v, %v), want (input, true, nil)", ok, err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal amended input %s: %v", raw, err)
	}

	if got["cmd"] != "rm -rf /tmp/x1" {
		t.Errorf("amended cmd = %v, want the edited command", got["cmd"])
	}
	// Every OTHER key survives, value for value. This is the assertion that
	// goes red the moment the reply is built from the command alone.
	want := map[string]any{
		"cmd":         "rm -rf /tmp/x1",
		"timeout":     float64(120),
		"description": "clean the scratch dir",
		"env":         map[string]any{"CI": "1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("amended input = %v, want %v — an amend must replace one key, not the whole input", got, want)
	}
}

// TestAmendedInputEditsTheDisplayedKey pins that the editor writes back to
// the SAME key commandBody picked for display: amending what the prompt shows
// must change what the prompt showed, not add a second key beside it.
func TestAmendedInputEditsTheDisplayedKey(t *testing.T) {
	for _, k := range commandKeys {
		t.Run(k, func(t *testing.T) {
			m := amendingModel(t, map[string]any{k: "original", "other": "kept"})
			m = m.ApplyApprovalAmendKey(tea.Key{Text: "!"})

			raw, ok, err := m.AmendedInput()
			if err != nil || !ok {
				t.Fatalf("AmendedInput() = (_, %v, %v), want (input, true, nil)", ok, err)
			}
			var got map[string]any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("unmarshal %s: %v", raw, err)
			}
			if want := map[string]any{k: "original!", "other": "kept"}; !reflect.DeepEqual(got, want) {
				t.Errorf("amended input = %v, want %v", got, want)
			}
		})
	}
}

// TestBeginApprovalAmendNeedsACommandKey pins the no-edit-target rule: a spec
// with no command-ish key has nothing sensible to open an editor over, so
// BeginApprovalAmend refuses rather than offering an empty editor whose
// commit would blank the call. The nothing-pending case refuses too.
func TestBeginApprovalAmendNeedsACommandKey(t *testing.T) {
	tests := []struct {
		name string
		spec map[string]any
	}{
		{"no command key", map[string]any{"query": "gofer", "limit": float64(5)}},
		{"empty spec", map[string]any{}},
		{"nil spec", nil},
		{"structured payload under a command key", map[string]any{"command": map[string]any{"a": 1}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := New(theme.Test()).Ingest(event.NewPermissionRequested("sess-1", "perm-1", "edit", tc.spec, nil))
			if next, ok := m.BeginApprovalAmend(); ok {
				t.Errorf("BeginApprovalAmend over %v opened an editor; want a refusal", tc.spec)
			} else if next.AmendingApproval() {
				t.Error("a refused BeginApprovalAmend still left an editor open")
			}
		})
	}

	if _, ok := New(theme.Test()).BeginApprovalAmend(); ok {
		t.Error("BeginApprovalAmend with nothing pending opened an editor")
	}
}

// TestAmendMutatorsAreCopyOnWrite pins Model's copy-on-write discipline
// across the amend mutators: the Model a caller still holds must never see an
// edit made on the value returned from it (a shared *pendingApproval would
// leak one).
func TestAmendMutatorsAreCopyOnWrite(t *testing.T) {
	base := amendingModel(t, map[string]any{"cmd": "ls"})

	typed := base.ApplyApprovalAmendKey(tea.Key{Text: "!"})
	if got := base.pending.amend.Text(); got != "ls" {
		t.Errorf("the original Model's editor changed to %q; want it untouched", got)
	}
	if got := typed.pending.amend.Text(); got != "ls!" {
		t.Errorf("the returned Model's editor = %q, want %q", got, "ls!")
	}
	if cancelled := typed.CancelApprovalAmend(); cancelled.AmendingApproval() || !typed.AmendingApproval() {
		t.Error("CancelApprovalAmend must close the editor on the copy only")
	}
}

// TestAmendEditorRenderCarriesTheOverrideWarning is the safety-copy test: the
// no-re-validation warning is present whenever the editor is open, in EVERY
// state the editor has (a fresh open, a typed edit, a scrolled multi-line
// body). The SDK does not re-run the guard over an amended input, and a UI
// that implied otherwise would be actively dangerous.
func TestAmendEditorRenderCarriesTheOverrideWarning(t *testing.T) {
	tests := []struct {
		name string
		spec map[string]any
		keys []tea.Key
	}{
		{"freshly opened", map[string]any{"cmd": "rm -rf /tmp/x"}, nil},
		{"after an edit", map[string]any{"cmd": "rm -rf /tmp/x"}, []tea.Key{{Text: " --dry-run"}}},
		{
			name: "multi-line body",
			spec: map[string]any{"cmd": strings.Repeat("echo hi\n", 19) + "echo bye"},
			keys: []tea.Key{{Code: tea.KeyUp}, {Code: tea.KeyUp}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := amendingModel(t, tc.spec)
			for _, k := range tc.keys {
				m = m.ApplyApprovalAmendKey(k)
			}
			got := flattenPrompt(testkit.Render(m, testkit.Width, testkit.Height))
			if !strings.Contains(got, warnAmendOverride) {
				t.Errorf("the open amend editor is missing the override warning %q:\n%s", warnAmendOverride, got)
			}
		})
	}
}

// TestAmendEditorRememberWarningOnlyWithRemember pins the second half of the
// warning: when remember is on, an amended allow makes the EDITED call the
// standing grant (the SDK substitutes the input BEFORE calling Grant — see
// loop.awaitApproval), and the copy says so. With remember off that sentence
// must NOT appear — it would be a claim about a grant that isn't happening.
func TestAmendEditorRememberWarningOnlyWithRemember(t *testing.T) {
	for _, remember := range []bool{false, true} {
		name := "remember off"
		if remember {
			name = "remember on"
		}
		t.Run(name, func(t *testing.T) {
			m := amendingModel(t, map[string]any{"cmd": "rm -rf /tmp/x"})
			if remember {
				m = m.ToggleApprovalRemember()
			}
			got := flattenPrompt(testkit.Render(m, testkit.Width, testkit.Height))
			if strings.Contains(got, warnAmendRemember) != remember {
				t.Errorf("remember=%v: the remembered-amend sentence %q present=%v, want %v\n%s",
					remember, warnAmendRemember, !remember, remember, got)
			}
			// The override warning is unconditional either way.
			if !strings.Contains(got, warnAmendOverride) {
				t.Errorf("remember=%v: missing the override warning:\n%s", remember, got)
			}
		})
	}
}

// TestAmendCopyNeverClaimsValidation pins the negative half of the safety
// copy: nothing the amend editor renders may suggest the edit is examined,
// approved by a rule, or safe. A golden alone can only prove "the bytes are
// these"; this proves a whole vocabulary can't appear.
func TestAmendCopyNeverClaimsValidation(t *testing.T) {
	m := amendingModel(t, map[string]any{"cmd": "rm -rf /tmp/x"}).ToggleApprovalRemember()
	got := strings.ToLower(flattenPrompt(testkit.Render(m, testkit.Width, testkit.Height)))
	for _, banned := range []string{"verified", "verify", "checked", "validated", "is safe", "safely"} {
		if strings.Contains(got, banned) {
			t.Errorf("amend copy contains %q — it must never imply the amended call was vetted:\n%s", banned, got)
		}
	}
}

// flattenPrompt collapses a rendered frame to one whitespace-normalized line
// so a substring assertion survives the prompt's word wrapping (a sentence
// the render splits across two rows is still one sentence).
func flattenPrompt(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
