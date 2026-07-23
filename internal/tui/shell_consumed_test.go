package tui

// shell_consumed_test.go covers the round-6 fix: a `!` run that has been
// consumed (folded into a submitted prompt, e.g. by the reply-now default) keeps
// rendering as a SIGIL block in the transcript, decoupled from the model-facing
// `$` fold. The daemon echoes the folded prompt verbatim as the user message, so
// the fix has two halves that must hold together: CommitShellRuns pins the run
// as a sigil block, and the MessageFinished{User} echo is stripped of the fold.
// The model still receives the `$` fold (composePrompt is unchanged) — that pair
// is the load-bearing assertion.

import (
	"strings"
	"testing"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// consumedRunModel is the shared fixture for the consumed-state goldens: a
// reply-now `!` run that fired its turn — committed as a sigil block, its echo
// stripped to nothing, followed by the agent's reply. This is exactly the
// transcript the round-6 bug got wrong (it showed `○ $ ls docs`).
func consumedRunModel(th theme.Theme) Model {
	const s = "sess-x"
	run := finishedRun(1, "ls docs", "doc1.md\ndoc2.md", true)
	fold := run.contextBlock()
	return New(th).
		CommitShellRuns([]shellRun{run}, fold).
		Ingest(event.NewMessageStarted(s, event.MessageUser)).
		Ingest(event.NewMessageFinished(s, event.MessageUser, fold)). // pure `!` turn → stripped away
		Ingest(event.NewMessageStarted(s, event.MessageText)).
		Ingest(event.NewMessageFinished(s, event.MessageText, "Two markdown files.")).
		Ingest(event.NewTurnFinished(s, "end_turn", provider.Usage{InputTokens: 12, OutputTokens: 4}))
}

// TestGoldenShellRunConsumed pins the fixed transcript: the `! ls docs` sigil
// block (no `$`, no `○`/`●` user bullet) at the message position, then the
// agent's reply — never the `$ ls docs` fold the model reads.
func TestGoldenShellRunConsumed(t *testing.T) {
	testkit.AssertGolden(t, "shell_run_consumed", consumedRunModel(theme.Test()).View(testkit.Width, testkit.Height))
}

func TestGoldenShellRunConsumedStyled(t *testing.T) {
	testkit.AssertGoldenStyled(t, "shell_run_consumed", consumedRunModel(testkit.ColorTheme()).View(testkit.Width, testkit.Height))
}

// TestConsumedShellRunRendersAsSigilNotFold is THE round-6 pair. A pure reply-now
// `!` run (no typed text): the transcript shows `! ls docs` (sigil block) and NOT
// the `$ ls docs` fold the model reads, while composePrompt's output — the model's
// copy — still carries `$ ls docs`. Mutation both ways: drop the echo strip and
// `$ ls docs` reappears on screen; drop the commit and `! ls docs` vanishes; stop
// folding and composePrompt loses `$ ls docs`.
func TestConsumedShellRunRendersAsSigilNotFold(t *testing.T) {
	const s = "sess-x"
	run := finishedRun(1, "ls docs", "doc1.md\ndoc2.md", true)
	fold := run.contextBlock() // "$ ls docs\ndoc1.md\ndoc2.md\n\n" — what the model reads

	// The consumed run is committed as a sigil block; its echo (== the fold, for a
	// pure `!` turn) arrives and is stripped to nothing (no separate user message).
	m := New(theme.Test()).
		CommitShellRuns([]shellRun{run}, fold).
		Ingest(event.NewMessageFinished(s, event.MessageUser, fold))
	view := m.View(testkit.Width, testkit.Height)

	if !strings.Contains(view, "! ls docs") {
		t.Errorf("consumed run does not render as a `! ls docs` sigil block:\n%s", view)
	}
	if strings.Contains(view, "$ ls docs") {
		t.Errorf("the model-facing `$ ls docs` fold leaked onto the screen:\n%s", view)
	}

	// The model's copy is unchanged: composePrompt still folds `$ ls docs` in.
	a := App{shellRuns: []shellRun{finishedRun(2, "ls docs", "doc1.md\ndoc2.md", true)}}
	if got := a.composePrompt(""); !strings.Contains(got, "$ ls docs") {
		t.Errorf("composePrompt = %q, want the model still gets the `$ ls docs` fold", got)
	}
}

// TestConsumedShellRunWithTypedText is the mixed case: a `!` run folded ahead of
// the user's typed text. The transcript shows the sigil block AND the typed
// message (residual after the fold is stripped), never the `$` fold.
func TestConsumedShellRunWithTypedText(t *testing.T) {
	const s = "sess-x"
	run := finishedRun(1, "ls docs", "doc1.md", true)
	fold := run.contextBlock()

	m := New(theme.Test()).
		CommitShellRuns([]shellRun{run}, fold).
		Ingest(event.NewMessageFinished(s, event.MessageUser, fold+"explain this"))
	view := m.View(testkit.Width, testkit.Height)

	if !strings.Contains(view, "! ls docs") {
		t.Errorf("consumed run does not render as a sigil block:\n%s", view)
	}
	if !strings.Contains(view, "explain this") {
		t.Errorf("the typed message (residual after the fold) is missing:\n%s", view)
	}
	if strings.Contains(view, "$ ls docs") {
		t.Errorf("the model-facing `$ ls docs` fold leaked onto the screen:\n%s", view)
	}
}

// TestIngestFoldQueueCopyOnWrite locks the copy-on-write invariant flagged in
// review: Ingest pops the head fold for a stripped echo, and because Model is
// value-copied per frame that pop must NOT reach through to a prior Model's
// queue. Two children derived from one prior each see the pop; the prior sees
// neither. Runs under `-race` in CI alongside the rest.
func TestIngestFoldQueueCopyOnWrite(t *testing.T) {
	const s = "sess-x"
	prior := New(theme.Test()).
		CommitShellRuns(nil, "$ a\n\n").
		CommitShellRuns(nil, "$ b\n\n")
	if len(prior.pendingEchoFolds) != 2 {
		t.Fatalf("precondition: prior queue = %v, want 2 folds", prior.pendingEchoFolds)
	}

	childA := prior.Ingest(event.NewMessageFinished(s, event.MessageUser, "$ a\n\ntyped A"))
	childB := prior.Ingest(event.NewMessageFinished(s, event.MessageUser, "$ a\n\ntyped B"))

	if len(prior.pendingEchoFolds) != 2 {
		t.Errorf("prior queue mutated by a child Ingest: %v, want it left at 2 folds", prior.pendingEchoFolds)
	}
	for _, c := range []struct {
		name string
		m    Model
	}{{"A", childA}, {"B", childB}} {
		if len(c.m.pendingEchoFolds) != 1 || c.m.pendingEchoFolds[0] != "$ b\n\n" {
			t.Errorf("child %s queue = %v, want the head popped leaving [\"$ b\\n\\n\"]", c.name, c.m.pendingEchoFolds)
		}
	}
}

// TestEchoStripOnlyMatchesHeadFold guards the byte-exact discipline: an ordinary
// user message that merely starts with a `$` (a user typing shell-looking text)
// is NOT stripped when no fold is queued — the strip is a byte-exact match
// against the recorded fold, never `$`-parsing.
func TestEchoStripOnlyMatchesHeadFold(t *testing.T) {
	const s = "sess-x"
	m := New(theme.Test()).Ingest(event.NewMessageFinished(s, event.MessageUser, "$ not a fold"))
	if got := m.View(testkit.Width, testkit.Height); !strings.Contains(got, "$ not a fold") {
		t.Errorf("a plain user message starting with `$` was wrongly stripped:\n%s", got)
	}
}
