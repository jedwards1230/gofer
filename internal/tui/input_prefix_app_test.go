package tui_test

// input_prefix_app_test.go covers the non-slash input prefixes wired end to
// end through App's exported tea.Model surface: `!` / `!!` from BOTH
// text-entry surfaces (the overview dispatch bar and the attach input, which
// must behave identically), the `!!` context exclusion observed where it
// actually matters — the prompt string the Supervisor receives — and the `@`
// mention popup's completion. The components' own logic is unit-tested in
// shell_test.go / filemention_test.go (package tui).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// Payload markers the escapes below read out of files rather than echo back.
// A marker that appears in the COMMAND text as well as its output would make
// every assertion below vacuous — `$ echo MARKER` contains "MARKER" whether
// or not the command ever ran — so the command names a file and the marker
// lives only inside it.
const (
	shellPayload  = "SHELL-OUTPUT-MARKER"
	shellWithheld = "WITHHELD-SECRET-MARKER"
)

// shellApp builds a sized, roster-loaded App over a REAL working directory
// (so the escape's cmd.Dir resolves and `cat payload.txt` genuinely runs
// there — which is also what proves the run inherits CommandEnv.Cwd), with
// $SHELL pinned to /bin/sh so the escapes run under a predictable interpreter
// on any machine.
func shellApp(t *testing.T, sup tui.Supervisor) tea.Model {
	t.Helper()
	t.Setenv("SHELL", "/bin/sh")
	root := t.TempDir()
	for name, content := range map[string]string{"payload.txt": shellPayload, "secret.txt": shellWithheld} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var m tea.Model = tui.NewApp(theme.Test(), sup, tui.GoldenMeta(), testCommandEnv(root))
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m, _ = m.Update(m.Init()())
	return m
}

// testCommandEnv is GoldenCommandEnv with a real cwd substituted in.
func testCommandEnv(cwd string) tui.CommandEnv {
	return tui.CommandEnv{
		Version: "0.3.0",
		Cwd:     cwd,
		Root:    "~/.gofer",
		Auth:    func() ([]tui.ProviderAuth, error) { return nil, nil },
		Config:  func() (config.Config, error) { return config.Config{}, nil },
	}
}

func TestShellEscapeIsNotSubmittedAsAPrompt(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := shellApp(t, sup)
	m = type_(t, m, "!cat payload.txt")
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if len(sup.created) != 0 {
		t.Fatalf("a `!` escape created a session from the literal text: %v", sup.created)
	}
	if got := content(m); !strings.Contains(got, shellPayload) {
		t.Fatalf("expected the command's output in the shell pane, got:\n%s", got)
	}
}

// TestShellEscapeOutputReachesTheNextPrompt is the `!` half of the contract:
// the user ran it so the agent can see it.
func TestShellEscapeOutputReachesTheNextPrompt(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := shellApp(t, sup)
	m = type_(t, m, "!cat payload.txt")
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	m = type_(t, m, "what does that mean")
	_ = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if len(sup.created) != 1 {
		t.Fatalf("expected exactly one session created, got %v", sup.created)
	}
	prompt := sup.created[0]
	for _, want := range []string{"$ cat payload.txt", shellPayload, "what does that mean"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt = %q, want it to contain %q", prompt, want)
		}
	}
}

// TestDoubleBangOutputNeverReachesThePrompt is the `!!` half, asserted at the
// Supervisor boundary — the last place gofer controls before the text becomes
// model input. A rendering-only assertion would pass even if the output
// leaked, which is precisely the bug this spelling exists to prevent.
func TestDoubleBangOutputNeverReachesThePrompt(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := shellApp(t, sup)
	m = type_(t, m, "!!cat secret.txt")
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if got := content(m); !strings.Contains(got, shellWithheld) {
		t.Fatalf("expected `!!` output shown to the OPERATOR, got:\n%s", got)
	}

	m = type_(t, m, "carry on")
	_ = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if len(sup.created) != 1 {
		t.Fatalf("expected exactly one session created, got %v", sup.created)
	}
	if prompt := sup.created[0]; strings.Contains(prompt, shellWithheld) || strings.Contains(prompt, "cat secret.txt") {
		t.Fatalf("prompt = %q — a `!!` run reached the model", prompt)
	}
	if prompt := sup.created[0]; prompt != "carry on" {
		t.Fatalf("prompt = %q, want exactly the user's own text", prompt)
	}
}

// TestShellEscapeFromAttachInputBehavesIdentically is the "same wherever it
// is typed" rule: the attach input runs the escape and folds it into the
// SEND, exactly as the dispatch bar does for the CREATE.
func TestShellEscapeFromAttachInputBehavesIdentically(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := shellApp(t, sup)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach the selected session

	m = type_(t, m, "!cat payload.txt")
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(sup.sent) != 0 {
		t.Fatalf("a `!` escape was sent as a prompt: %v", sup.sent)
	}

	m = type_(t, m, "and now")
	_ = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if len(sup.sent) != 1 {
		t.Fatalf("expected one Send, got %v", sup.sent)
	}
	if !strings.Contains(sup.sent[0], shellPayload) || !strings.Contains(sup.sent[0], "and now") {
		t.Fatalf("sent = %q, want the shell output folded in ahead of the prompt", sup.sent[0])
	}
}

func TestBareBangRunsNothing(t *testing.T) {
	for _, buf := range []string{"!", "!!"} {
		sup := newFakeSup(tui.GoldenRoster())
		m := shellApp(t, sup)
		m = type_(t, m, buf)
		m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

		if len(sup.created) != 0 {
			t.Fatalf("%q created a session: %v", buf, sup.created)
		}
		if got := content(m); !strings.Contains(got, "nothing to run") {
			t.Fatalf("%q said nothing to the user, got:\n%s", buf, got)
		}
	}
}

// TestTextContainingABangSubmitsAsAPrompt is the non-hijacking guard end to
// end: only a LEADING sigil dispatches.
func TestTextContainingABangSubmitsAsAPrompt(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := shellApp(t, sup)
	m = type_(t, m, "that worked! mail sorretin@gmail.com")
	_ = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if len(sup.created) != 1 {
		t.Fatalf("expected the text submitted as a prompt, got %v", sup.created)
	}
	if want := "that worked! mail sorretin@gmail.com"; sup.created[0] != want {
		t.Fatalf("prompt = %q, want %q verbatim", sup.created[0], want)
	}
}

// mentionApp builds an App whose CommandEnv.Cwd is a real directory holding
// files, so the `@` enumeration has something to find.
func mentionApp(t *testing.T, sup tui.Supervisor, files []string) tea.Model {
	t.Helper()
	root := t.TempDir()
	for _, rel := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var m tea.Model = tui.NewApp(theme.Test(), sup, tui.GoldenMeta(), testCommandEnv(root))
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m, _ = m.Update(m.Init()())
	return m
}

func TestMentionPopupOpensAndCompletes(t *testing.T) {
	m := mentionApp(t, newFakeSup(tui.GoldenRoster()), []string{"notes.md", "src/main.go"})
	m = type_(t, m, "explain @main")

	if got := content(m); !strings.Contains(got, "@src/main.go") {
		t.Fatalf("expected the mention popup to offer src/main.go, got:\n%s", got)
	}

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	if got := content(m); !strings.Contains(got, "explain @src/main.go") {
		t.Fatalf("expected Tab to splice the path into the buffer, got:\n%s", got)
	}
}

// TestMentionSubmitsThePathAsText pins the scope decision: `@path` passes the
// PATH through — the file's contents are never inlined into the prompt.
func TestMentionSubmitsThePathAsText(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := mentionApp(t, sup, []string{"src/main.go"})
	m = type_(t, m, "explain @main")
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyTab})
	_ = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if len(sup.created) != 1 {
		t.Fatalf("expected the mention submitted as an ordinary prompt, got %v", sup.created)
	}
	if want := "explain @src/main.go "; sup.created[0] != want {
		t.Fatalf("prompt = %q, want %q — the path, not the file's contents", sup.created[0], want)
	}
}

// TestEmailAddressDoesNotOpenTheMentionPopup is the `@` non-triggering case:
// a sigil that isn't at a token boundary is literal text.
func TestEmailAddressDoesNotOpenTheMentionPopup(t *testing.T) {
	m := mentionApp(t, newFakeSup(tui.GoldenRoster()), []string{"main.go", "mail.go"})
	m = type_(t, m, "mail sorretin@ma")

	if got := content(m); strings.Contains(got, "▸ @") {
		t.Fatalf("a mid-word @ opened the mention popup, got:\n%s", got)
	}
}
