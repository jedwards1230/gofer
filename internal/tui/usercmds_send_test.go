package tui_test

// usercmds_send_test.go covers what a markdown command DOES, through App's
// exported Update/View surface only: it autocompletes like a builtin, and
// running it submits its expanded body through the same Supervisor.Send seam
// a hand-typed prompt uses (fakeSup.sent records both identically, which is
// the point — there is one send path, not two).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/theme"
	"github.com/jedwards1230/gofer/internal/usercmd"
)

// seedUserCmd writes rel under dir with content, making parents.
func seedUserCmd(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

// newUserCmdEnv returns a CommandEnv rooted at fresh temp directories, plus
// the two commands directories to seed.
func newUserCmdEnv(t *testing.T) (env tui.CommandEnv, userDir, projectDir string) {
	t.Helper()
	root, cwd := t.TempDir(), t.TempDir()
	env = tui.GoldenCommandEnv()
	env.Root, env.Cwd = root, cwd
	return env, usercmd.UserDir(root), usercmd.ProjectDir(cwd)
}

// newUserCmdModel builds a sized App over sup/env with the first roster fetch
// resolved — newTestApp's construction with a caller-supplied CommandEnv.
func newUserCmdModel(t *testing.T, sup tui.Supervisor, env tui.CommandEnv) tea.Model {
	t.Helper()
	var m tea.Model = tui.NewApp(theme.Test(), sup, tui.GoldenMeta(), env)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m, _ = m.Update(m.Init()())
	return m
}

// attachedSessionID is GoldenRoster's first (selected) session — the one a
// single → press attaches to.
const attachedSessionID = "0192a1b2-app0-7000-8000-000000000001"

// TestUserCommandSendsExpandedBody is the end-to-end case: `/review 42
// urgently` from the attach input reaches Supervisor.Send carrying the body
// with its arguments substituted, exactly as if the user had typed that text.
func TestUserCommandSendsExpandedBody(t *testing.T) {
	env, userDir, _ := newUserCmdEnv(t)
	seedUserCmd(t, userDir, "review.md",
		"---\ndescription: Review a PR\nargument-hint: [pr] [tone]\n---\nReview PR $1 ${2:-carefully} — all args: $ARGUMENTS\n")

	sup := newFakeSup(tui.GoldenRoster())
	m := newUserCmdModel(t, sup, env)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach the selected session
	dispatchSlash(t, m, "/review 42 urgently")

	want := attachedSessionID + ":Review PR 42 urgently — all args: 42 urgently"
	if len(sup.sent) != 1 || sup.sent[0] != want {
		t.Fatalf("sup.sent = %v; want one entry %q", sup.sent, want)
	}
}

// TestUserCommandUsesDefaultForMissingArg covers `${N:-default}` reaching the
// wire, not just the unit test in internal/usercmd.
func TestUserCommandUsesDefaultForMissingArg(t *testing.T) {
	env, userDir, _ := newUserCmdEnv(t)
	seedUserCmd(t, userDir, "review.md", "Review PR $1 ${2:-carefully}\n")

	sup := newFakeSup(tui.GoldenRoster())
	m := newUserCmdModel(t, sup, env)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
	dispatchSlash(t, m, "/review 42")

	want := attachedSessionID + ":Review PR 42 carefully"
	if len(sup.sent) != 1 || sup.sent[0] != want {
		t.Fatalf("sup.sent = %v; want one entry %q", sup.sent, want)
	}
}

// TestUserCommandMatchesTypedPromptExactly is the "one send path" assertion:
// running a markdown command and typing its expanded text by hand must
// produce byte-identical Send calls.
func TestUserCommandMatchesTypedPromptExactly(t *testing.T) {
	env, userDir, _ := newUserCmdEnv(t)
	seedUserCmd(t, userDir, "review.md", "Review PR $1")

	sup := newFakeSup(tui.GoldenRoster())
	m := newUserCmdModel(t, sup, env)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
	m = dispatchSlash(t, m, "/review 42")
	dispatchSlash(t, m, "Review PR 42") // not a slash command: a plain prompt

	if len(sup.sent) != 2 {
		t.Fatalf("sup.sent = %v; want two sends", sup.sent)
	}
	if sup.sent[0] != sup.sent[1] {
		t.Fatalf("markdown command sent %q but the same text typed by hand sent %q — "+
			"a markdown command must go through the same send path as a typed prompt",
			sup.sent[0], sup.sent[1])
	}
}

// TestUserCommandWithoutSessionReports covers the overview case: there is no
// session to send to, so the command must SAY so rather than silently drop
// the prompt.
func TestUserCommandWithoutSessionReports(t *testing.T) {
	env, userDir, _ := newUserCmdEnv(t)
	seedUserCmd(t, userDir, "review.md", "Review PR $1")

	sup := newFakeSup(tui.GoldenRoster())
	m := newUserCmdModel(t, sup, env)
	m = dispatchSlash(t, m, "/review 42") // still on the overview: nothing attached

	if len(sup.sent) != 0 {
		t.Fatalf("sup.sent = %v; want none — there is no session to send to", sup.sent)
	}
	if got := content(m); !strings.Contains(got, "attach a session") {
		t.Fatalf("expected a status note explaining there is no session, got:\n%s", got)
	}
	if len(sup.created) != 0 {
		t.Fatalf("sup.created = %v; want none — a markdown command must not silently create a session", sup.created)
	}
}

// TestUserCommandEmptyExpansionReports covers a body that expands to nothing
// (an empty file, or `$1` with no argument): a turn must not be burned on an
// empty prompt, and the user must be told why nothing happened.
func TestUserCommandEmptyExpansionReports(t *testing.T) {
	env, userDir, _ := newUserCmdEnv(t)
	seedUserCmd(t, userDir, "blank.md", "$1\n")

	sup := newFakeSup(tui.GoldenRoster())
	m := newUserCmdModel(t, sup, env)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
	m = dispatchSlash(t, m, "/blank")

	if len(sup.sent) != 0 {
		t.Fatalf("sup.sent = %v; want none — an empty expansion must not be sent", sup.sent)
	}
	if got := content(m); !strings.Contains(got, "empty prompt") {
		t.Fatalf("expected a status note about the empty expansion, got:\n%s", got)
	}
}

// TestUserCommandAutocompletes verifies a markdown command shows up in the
// autocomplete popup with its frontmatter summary and argument hint, and that
// Tab completes it — the same affordance a builtin gets, via the same popup.
func TestUserCommandAutocompletes(t *testing.T) {
	env, userDir, _ := newUserCmdEnv(t)
	seedUserCmd(t, userDir, "git/review.md",
		"---\ndescription: Review the diff\nargument-hint: [pr]\n---\nreview $1\n")

	m := newUserCmdModel(t, newFakeSup(tui.GoldenRoster()), env)
	m = type_(t, m, "/gi")

	got := content(m)
	for _, want := range []string{"/git:review [pr]", "Review the diff"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected the popup to show %q, got:\n%s", want, got)
		}
	}

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	if got := content(m); !strings.Contains(got, "/git:review ") {
		t.Fatalf("expected Tab to complete the markdown command name, got:\n%s", got)
	}
}

// TestUserCommandFileWrittenWhileRunningIsPickedUp pins the reload contract:
// the markdown layer is refreshed when the autocomplete popup opens (typing
// "/"), so a file created after startup is usable without restarting.
func TestUserCommandFileWrittenWhileRunningIsPickedUp(t *testing.T) {
	env, userDir, _ := newUserCmdEnv(t)

	sup := newFakeSup(tui.GoldenRoster())
	m := newUserCmdModel(t, sup, env)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight})

	seedUserCmd(t, userDir, "late.md", "written after startup")

	dispatchSlash(t, m, "/late")
	want := attachedSessionID + ":written after startup"
	if len(sup.sent) != 1 || sup.sent[0] != want {
		t.Fatalf("sup.sent = %v; want one entry %q — a command file written while the TUI runs "+
			"must be picked up when the popup opens", sup.sent, want)
	}
}

// TestUserCommandFirstFrameRender guards the zero-height/first-frame render
// that has panicked this TUI before: an App carrying markdown commands must
// render before it is ever sized, and at a one-row height, without panicking.
func TestUserCommandFirstFrameRender(t *testing.T) {
	env, userDir, _ := newUserCmdEnv(t)
	seedUserCmd(t, userDir, "review.md", "---\ndescription: Review\n---\nreview $1\n")

	var m tea.Model = tui.NewApp(theme.Test(), newFakeSup(tui.GoldenRoster()), tui.GoldenMeta(), env)
	_ = m.View() // unsized: width and height are still 0

	for _, size := range []tea.WindowSizeMsg{{Width: 0, Height: 0}, {Width: 80, Height: 1}, {Width: 1, Height: 1}} {
		m, _ = m.Update(size)
		_ = m.View()
		next := press(t, m, tea.KeyPressMsg{Text: "/"}) // opens the popup, reloads the layer
		_ = next.View()
	}
}
