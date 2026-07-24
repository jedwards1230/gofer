package tui

// decision_multi_test.go covers the MULTI-question shape of the inline
// decision prompt — the tab strip, the side panel, the notes editor, and the
// draft-then-submit-once contract — at all four of docs/TUI.md's test layers:
//
//   - the Ascii goldens, locking the two frames' layout against the mockup
//     (a question tab and the Submit tab);
//   - the styled golden, locking the color state an Ascii render is blind to
//     (an answered tab's accent ✔ against an unanswered tab's muted □, and the
//     focused tab's label against its unfocused siblings');
//   - the narrow-width colored layout test, catching the #61 ANSI-width class
//     that the two-column split is the newest way to reintroduce; and
//   - the Update-level key behavior, driven through App's real dispatch
//     against a REAL decision.Gate with a genuinely blocked ask_user call, so
//     "the answer reached the agent" is asserted rather than assumed.
//
// It lives in package tui (not tui_test) for the reason app_internal_test.go
// does: it constructs the app root's unexported decision messages, and the
// golden fixture drives the same Model mutators the keymap calls (the key →
// mutator wiring is what the Update-level tests below assert).

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/charmbracelet/x/ansi"

	"github.com/jedwards1230/agent-sdk-go/acp"

	"github.com/jedwards1230/gofer/internal/decision"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// multiDecisionQuestions is the two-question batch every test here asks: both
// questions titled (so the tab strip renders titles rather than its Q1/Q2
// fallback), both carrying side-panel context, the first's options carrying
// references, and labels/rationales long enough that they wrap once the side
// panel takes its third of the frame.
func multiDecisionQuestions() []acp.DecisionQuestion {
	return []acp.DecisionQuestion{
		{
			Title:    "M4 slicing",
			Question: "How should the M4 milestone be sliced?",
			Context:  "M4 is the TUI polish milestone; whichever half lands first is what M5 has to build on.",
			Options: []acp.DecisionOption{
				{
					Label:     "Renderer first, wiring after",
					Rationale: "goldens land early, so the frame is reviewable before any plumbing moves",
					Reference: "docs/TUI.md#rendering",
				},
				{
					Label:       "Wiring first, renderer after",
					Rationale:   "unblocks the daemon path sooner, at the cost of an unreviewable frame",
					Reference:   "docs/PRD.md#m4",
					Recommended: true,
				},
			},
			AllowFreeText: decision.DefaultAllowFreeText,
			AllowChat:     decision.DefaultAllowChat,
		},
		{
			Title:    "Views v1 scope",
			Question: "Which views ship in v1?",
			Context:  "Every view added now is a view the roster has to keep working.",
			Options: []acp.DecisionOption{
				{Label: "Roster and attach only"},
				{
					Label:       "Roster, attach, and peek",
					Rationale:   "peek is where the roster stops being a list and starts being a dashboard",
					Recommended: true,
				},
			},
			AllowFreeText: decision.DefaultAllowFreeText,
			AllowChat:     decision.DefaultAllowChat,
		},
	}
}

// multiDecisionUpdate wraps multiDecisionQuestions in the UpdateRequested the
// gate publishes, ids stamped exactly as decision.Gate.Request stamps them.
func multiDecisionUpdate() decision.Update {
	return decision.Update{
		Kind: decision.UpdateRequested,
		Request: decision.Request{
			ID:        "dec-1",
			SessionID: "sess-x",
			Questions: decision.AssignIDs(multiDecisionQuestions()),
		},
	}
}

// multiDecisionModel is the golden fixture's state: the second question
// answered, a note attached to the FIRST, and the first question focused —
// which puts every multi-question affordance in one frame (a mixed ✔/□ tab
// strip, the focus caret, a side panel carrying context + the focused option's
// reference + the note, and wrapped option labels).
//
// It composes the same Model mutators App.handleDecisionKey calls rather than a
// test-only setter, so the golden asserts the production render of production
// state; the Update-level tests below own the key → mutator half.
func multiDecisionModel(th theme.Theme) Model {
	m := New(th).IngestDecision(multiDecisionUpdate())
	m = m.moveDecisionTab(1) // → "Views v1 scope"
	m = m.recordDecisionAnswer(acp.DecisionOutcomeSelected{OptionID: "q2o2"})
	m = m.moveDecisionTab(-1) // ← back to "M4 slicing"
	m = m.startDecisionNoting()
	m = m.withDecisionNotes(inputBuffer{}.SetText("leaning renderer-first, ask again after the spike"))
	return m.commitDecisionNote()
}

// TestGoldenDecisionPromptMulti locks the multi-question frame: the tab strip
// with its end arrows and per-question checkboxes, the focused question beside
// its side panel, the wrapped option labels and rationales, and the merged
// two-line key hint.
func TestGoldenDecisionPromptMulti(t *testing.T) {
	m := multiDecisionModel(theme.Test())
	testkit.AssertGolden(t, "decision_prompt_multi", testkit.Render(m, testkit.Width, testkit.Height))
}

// TestGoldenStyledDecisionPromptMulti is the multi-question frame's styled
// counterpart, and it is the layer that matters most here: the tab strip
// reports "answered" with an accent ✔ and "not answered" with a muted □, and
// marks the focused tab by leaving its label unstyled while its siblings are
// muted. A regression that colored every tab the same — or that lost the
// answered/unanswered distinction entirely — passes every Ascii golden.
func TestGoldenStyledDecisionPromptMulti(t *testing.T) {
	m := multiDecisionModel(testkit.ColorTheme())
	testkit.AssertGoldenStyled(t, "decision_prompt_multi", testkit.Render(m, testkit.Width, testkit.Height))
}

// TestGoldenDecisionPromptMultiSubmit locks the Submit tab: the per-question
// review of what is about to be sent (including the questions that will be
// cancelled because they were never answered), and the Submit row itself.
func TestGoldenDecisionPromptMultiSubmit(t *testing.T) {
	m := multiDecisionModel(theme.Test()).moveDecisionTab(-1) // ← wraps onto Submit
	if !m.pendingDec.onSubmitTab() {
		t.Fatalf("expected shift+tab off the first tab to wrap onto Submit; tab = %d", m.pendingDec.tab)
	}
	testkit.AssertGolden(t, "decision_prompt_multi_submit", testkit.Render(m, testkit.Width, testkit.Height))
}

// TestColorDecisionPromptMultiWidths proves the two-column split changes no
// geometry when colored and never overruns the frame — the #61 display-width
// lesson, which the side panel reintroduces the risk of because it is the only
// part of this prompt composited by display column rather than stacked top to
// bottom. 80 is the normal split, 46 is just below the width at which the
// panel gives up and stacks beneath the options, and 24 is the narrow case
// where the tab strip itself falls back to its "(i/n)" form.
func TestColorDecisionPromptMultiWidths(t *testing.T) {
	for _, width := range []int{80, 46, 24} {
		plain := testkit.Render(multiDecisionModel(theme.Test()), width, testkit.Height)
		colored := testkit.Render(multiDecisionModel(testkit.ColorTheme()), width, testkit.Height)

		if stripped := ansi.Strip(colored); stripped != plain {
			t.Errorf("width %d: colored render stripped of ANSI != plain render (color changed layout)\n--- stripped ---\n%s\n--- plain ---\n%s",
				width, stripped, plain)
		}
		for i, line := range strings.Split(colored, "\n") {
			if w := ansi.StringWidth(line); w > width {
				t.Errorf("width %d: line %d is %d cells: %q", width, i, w, line)
			}
		}
	}
}

// TestDecisionMultiPanelCollapsesNarrow pins the narrow-terminal contract the
// widths test can only assert negatively: below the split's minimum the side
// panel does not tear the layout, and it does not silently drop its content
// either — it stacks beneath the option list instead.
func TestDecisionMultiPanelCollapsesNarrow(t *testing.T) {
	m := multiDecisionModel(theme.Test())
	got := strings.Join(renderDecisionPrompt(theme.Test(), *m.pendingDec, 40), "\n")

	if strings.Contains(got, decisionPanelDivider) {
		t.Errorf("expected no two-column divider at width 40:\n%s", got)
	}
	for _, want := range []string{decisionPanelContext, "M4 is the TUI polish milestone", "leaning renderer-first"} {
		if !strings.Contains(got, want) {
			t.Errorf("collapsed panel dropped %q:\n%s", want, got)
		}
	}
}

// TestDecisionMultiDegenerateShapes covers the question shapes an agent can
// legitimately ask for but the mockup never draws: no options at all (free text
// only), and both escape hatches opted out of so the question has NO selectable
// row. Neither may panic, and the second must still be tab-navigable — its
// answer is simply "cancel, or move on to the next question".
func TestDecisionMultiDegenerateShapes(t *testing.T) {
	th := theme.Test()
	m := New(th).IngestDecision(decision.Update{
		Kind: decision.UpdateRequested,
		Request: decision.Request{
			ID: "dec-1", SessionID: "sess-x",
			Questions: decision.AssignIDs([]acp.DecisionQuestion{
				{Title: "Freeform", Question: "Anything to add?", AllowFreeText: true},
				{Title: "Locked", Question: "No way to answer this one."},
			}),
		},
	})

	if rows := len(m.pendingDec.rows()); rows != 1 {
		t.Errorf("free-text-only question has %d rows; want 1", rows)
	}
	m = m.moveDecisionTab(1)
	if rows := len(m.pendingDec.rows()); rows != 0 {
		t.Errorf("question with no options and no escape hatches has %d rows; want 0", rows)
	}
	if _, ok := m.pendingDec.focused(); ok {
		t.Error("expected no focused row on a question with no rows at all")
	}
	// The render must survive it, at a sane width and at the floor.
	for _, width := range []int{80, 1} {
		if lines := renderDecisionPrompt(th, *m.pendingDec, width); len(lines) == 0 {
			t.Errorf("width %d: renderDecisionPrompt returned nothing", width)
		}
	}
	m = m.moveDecisionTab(1)
	if !m.pendingDec.onSubmitTab() {
		t.Error("expected the third tab of a two-question request to be Submit")
	}
	if got := m.pendingDec.submitAnswers(); len(got) != 0 {
		t.Errorf("submitAnswers with nothing drafted = %+v; want none (the gate cancels them)", got)
	}
}

// openMultiDecision opens a genuinely blocked multi-question ask_user call on
// the attached session's real gate and pumps the prompt into the App.
func openMultiDecision(t *testing.T) (*internalFakeSup, App, blockedRequest) {
	t.Helper()
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDecisionTest(t, sup)
	a, req := openDecisionWith(t, sup, a, multiDecisionQuestions())
	return sup, a, req
}

// TestDecisionMultiTabCycles covers Tab/shift+tab: Tab walks the strip onto the
// Submit tab and WRAPS back to the first question, shift+tab walks it
// backwards, and the frame reports which tab is focused throughout. Tab wraps
// where the row cursor clamps because switching tabs commits nothing — see
// Model.moveDecisionTab.
func TestDecisionMultiTabCycles(t *testing.T) {
	_, a, req := openMultiDecision(t)
	defer req.stillBlocked(t)

	if got := a.sess.pendingDec.tab; got != 0 {
		t.Fatalf("initial tab = %d; want the first question", got)
	}
	if got := a.render(); !strings.Contains(got, "▸ □ M4 slicing") {
		t.Errorf("expected the first tab focused and unanswered in the strip:\n%s", got)
	}

	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyTab})
	if got := a.sess.pendingDec.tab; got != 1 {
		t.Fatalf("tab after one Tab = %d; want 1", got)
	}
	if got := a.render(); !strings.Contains(got, "Which views ship in v1?") {
		t.Errorf("expected the second question rendered after Tab:\n%s", got)
	}

	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyTab})
	if !a.sess.pendingDec.onSubmitTab() {
		t.Fatal("expected the third Tab stop to be the Submit tab")
	}
	// A note annotates a question's answer, and the Submit tab has no question
	// — so `n` must not open an editor there (one Enter could not close).
	a = pressDecision(t, a, tea.KeyPressMsg{Text: "n"})
	if a.sess.pendingDec.noting {
		t.Error("n opened the notes editor on the Submit tab; want it unbound there")
	}

	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyTab})
	if got := a.sess.pendingDec.tab; got != 0 {
		t.Errorf("tab after wrapping past Submit = %d; want 0", got)
	}

	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if !a.sess.pendingDec.onSubmitTab() {
		t.Error("expected shift+tab off the first tab to wrap back onto Submit")
	}
}

// TestDecisionMultiArrowKeysSwitchQuestions is the ←/→ mutation check: the tab
// strip draws end arrows, so left/right must move between question tabs (and
// onto Submit) just as Tab/shift+tab do — a rendered affordance that did
// nothing when pressed would be a lie. → walks forward through the questions
// and onto Submit; ← walks back.
func TestDecisionMultiArrowKeysSwitchQuestions(t *testing.T) {
	_, a, req := openMultiDecision(t)
	defer req.stillBlocked(t)

	if got := a.sess.pendingDec.tab; got != 0 {
		t.Fatalf("initial tab = %d; want the first question", got)
	}

	// → advances to the second question, then onto Submit.
	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyRight})
	if got := a.sess.pendingDec.tab; got != 1 {
		t.Fatalf("tab after one → = %d; want 1", got)
	}
	if got := a.render(); !strings.Contains(got, "Which views ship in v1?") {
		t.Errorf("expected the second question rendered after →:\n%s", got)
	}
	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyRight})
	if !a.sess.pendingDec.onSubmitTab() {
		t.Fatal("expected the second → to land on the Submit tab")
	}

	// ← walks back off Submit onto the last question.
	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyLeft})
	if got := a.sess.pendingDec.tab; got != 1 {
		t.Errorf("tab after ← off Submit = %d; want 1", got)
	}
}

// TestDecisionMultiAnswersAccumulateThenSubmitOnce is the batch contract's
// load-bearing test: answering on one tab does NOT send anything, answers
// accumulate across tabs, and the Submit tab sends exactly ONE
// AnswerDecision carrying every drafted answer — which is the whole reason a
// multi-question request exists.
func TestDecisionMultiAnswersAccumulateThenSubmitOnce(t *testing.T) {
	sup, a, req := openMultiDecision(t)

	a = pressDecision(t, a, tea.KeyPressMsg{Text: "2"}) // q1 → option 2
	if len(sup.answers) != 0 {
		t.Fatalf("answering one question of two sent %+v; want nothing until Submit", sup.answers)
	}
	if !a.sess.HasPendingDecision() {
		t.Fatal("expected the prompt to stay open after answering one of two questions")
	}
	req.stillBlocked(t)

	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyTab})
	a = pressDecision(t, a, tea.KeyPressMsg{Text: "1"}) // q2 → option 1
	if len(sup.answers) != 0 {
		t.Fatalf("answering the second question sent %+v; want nothing until Submit", sup.answers)
	}
	req.stillBlocked(t)

	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyTab})   // → Submit
	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyEnter}) // commit

	if a.sess.HasPendingDecision() {
		t.Fatal("expected the prompt cleared on submit")
	}
	if len(sup.answers) != 1 {
		t.Fatalf("sup.answers = %+v; want exactly ONE AnswerDecision call carrying the batch", sup.answers)
	}
	sent := sup.answers[0].answers
	if len(sent) != 2 {
		t.Fatalf("submit sent %d answers for a two-question batch: %+v", len(sent), sent)
	}

	answers := req.await(t)
	if len(answers) != 2 {
		t.Fatalf("the blocked ask_user call received %+v; want one answer per question", answers)
	}
	for i, want := range []string{"q1o2", "q2o1"} {
		sel, ok := answers[i].Outcome.(acp.DecisionOutcomeSelected)
		if !ok {
			t.Fatalf("answers[%d].Outcome = %#v; want a DecisionOutcomeSelected", i, answers[i].Outcome)
		}
		if sel.OptionID != want {
			t.Errorf("answers[%d] selected %q; want %q — the batch must carry EVERY question's answer, not just the focused one",
				i, sel.OptionID, want)
		}
	}
}

// TestDecisionMultiTabCheckboxFlipsOnAnswer pins the tab strip's one job:
// reporting which questions are still outstanding. An answer flips that
// question's box from □ to ✔ and leaves the other one alone.
func TestDecisionMultiTabCheckboxFlipsOnAnswer(t *testing.T) {
	_, a, req := openMultiDecision(t)
	defer req.stillBlocked(t)

	before := a.render()
	if !strings.Contains(before, "□ M4 slicing") || !strings.Contains(before, "□ Views v1 scope") {
		t.Fatalf("expected both tabs unanswered before anything is picked:\n%s", before)
	}
	if strings.Contains(before, "✔ M4 slicing") {
		t.Fatalf("an unanswered tab rendered as answered:\n%s", before)
	}

	a = pressDecision(t, a, tea.KeyPressMsg{Text: "1"})

	after := a.render()
	if !strings.Contains(after, "✔ M4 slicing") {
		t.Errorf("expected the answered tab to flip to ✔:\n%s", after)
	}
	if !strings.Contains(after, "□ Views v1 scope") {
		t.Errorf("expected the unanswered tab to stay □:\n%s", after)
	}
	if !strings.Contains(after, "□ "+decisionSubmitLabel) {
		t.Errorf("expected the Submit tab to stay □ while a question is outstanding:\n%s", after)
	}
}

// TestDecisionMultiNotesAttachToAnswer covers `n` end to end: it opens the
// notes editor (with its own key hint), typed text lands in the side panel, and
// the saved note rides out on THAT question's DecisionAnswer.Notes — the field
// acp.DecisionAnswer carries the note in and the reason `n` exists at all.
func TestDecisionMultiNotesAttachToAnswer(t *testing.T) {
	sup, a, req := openMultiDecision(t)

	a = pressDecision(t, a, tea.KeyPressMsg{Text: "2"}) // answer q1 first
	a = pressDecision(t, a, tea.KeyPressMsg{Text: "n"})
	if !a.sess.pendingDec.noting {
		t.Fatal("expected the notes editor open after n")
	}
	if got := a.render(); !strings.Contains(got, "Enter to save the note · Esc to discard") {
		t.Errorf("expected the notes-mode key hint:\n%s", got)
	}

	// Digits must type, not select — the editor owns the keyboard while open.
	for _, r := range "revisit in 2 weeks" {
		mdl, _ := a.Update(tea.KeyPressMsg{Text: string(r)})
		a = mdl.(App)
	}
	if got := a.render(); !strings.Contains(got, "revisit in 2 weeks▏") {
		t.Errorf("expected the note being typed, with its cursor, in the side panel:\n%s", got)
	}

	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyEnter}) // save the note
	if a.sess.pendingDec.noting {
		t.Fatal("expected the notes editor closed after Enter")
	}
	if len(sup.answers) != 0 {
		t.Fatalf("saving a note sent %+v; want nothing — a note is not an answer", sup.answers)
	}
	if got := a.sess.pendingDec.drafts[0].notes; got != "revisit in 2 weeks" {
		t.Fatalf("draft note = %q; want the typed text", got)
	}

	// Submit and read the note off the answer the AGENT received.
	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyTab})
	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyTab})
	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyEnter})

	answers := req.await(t)
	if len(answers) != 2 {
		t.Fatalf("answers = %+v; want one per question", answers)
	}
	if answers[0].Notes != "revisit in 2 weeks" {
		t.Errorf("answers[0].Notes = %q; want the note attached with n", answers[0].Notes)
	}
	if answers[1].Notes != "" {
		t.Errorf("answers[1].Notes = %q; want the note on the question it was written for only", answers[1].Notes)
	}
}

// TestDecisionMultiNoteWithoutAnswerSurvivesSubmit covers the one case
// submitAnswers cannot leave to the gate: a question the user annotated but
// never answered. Omitting it would drop the note, so it goes out as an
// explicit cancelled answer carrying the note.
func TestDecisionMultiNoteWithoutAnswerSurvivesSubmit(t *testing.T) {
	_, a, req := openMultiDecision(t)

	a = pressDecision(t, a, tea.KeyPressMsg{Text: "n"})
	for _, r := range "none of these" {
		mdl, _ := a.Update(tea.KeyPressMsg{Text: string(r)})
		a = mdl.(App)
	}
	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyEnter}) // save
	if got := a.render(); !strings.Contains(got, "□ M4 slicing") {
		t.Errorf("a note alone must not mark the question answered:\n%s", got)
	}

	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyTab})
	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyTab})
	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyEnter})
	if a.sess.HasPendingDecision() {
		t.Fatal("expected the prompt cleared on submit")
	}

	answers := req.await(t)
	if _, ok := answers[0].Outcome.(acp.DecisionOutcomeCancelled); !ok {
		t.Errorf("answers[0].Outcome = %#v; want cancelled for an annotated-but-unanswered question", answers[0].Outcome)
	}
	if answers[0].Notes != "none of these" {
		t.Errorf("answers[0].Notes = %q; want the note preserved rather than dropped with the question", answers[0].Notes)
	}
}

// TestDecisionMultiPartialSubmitCancelsTheRest is the issue's open question,
// answered: submitting with only some questions answered commits those and
// leaves the rest for the GATE to fill in as cancelled — this client sends one
// answer, not two, and the agent still receives one per question.
func TestDecisionMultiPartialSubmitCancelsTheRest(t *testing.T) {
	sup, a, req := openMultiDecision(t)

	a = pressDecision(t, a, tea.KeyPressMsg{Text: "1"})        // answer q1 only
	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyTab}) // → q2
	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyTab}) // → Submit
	if got := a.render(); !strings.Contains(got, decisionUnansweredNote) {
		t.Errorf("expected the Submit tab to say what happens to an unanswered question:\n%s", got)
	}
	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyEnter})
	if a.sess.HasPendingDecision() {
		t.Fatal("expected the prompt cleared on submit")
	}

	if len(sup.answers) != 1 {
		t.Fatalf("sup.answers = %+v; want exactly one AnswerDecision call", sup.answers)
	}
	if sent := sup.answers[0].answers; len(sent) != 1 || sent[0].QuestionID != "q1" {
		t.Fatalf("partial submit sent %+v; want only the answered question — the gate fills the rest in", sent)
	}

	answers := req.await(t)
	if len(answers) != 2 {
		t.Fatalf("the agent received %+v; want one answer per question", answers)
	}
	if sel, ok := answers[0].Outcome.(acp.DecisionOutcomeSelected); !ok || sel.OptionID != "q1o1" {
		t.Errorf("answers[0].Outcome = %#v; want the option that was actually picked", answers[0].Outcome)
	}
	if _, ok := answers[1].Outcome.(acp.DecisionOutcomeCancelled); !ok {
		t.Errorf("answers[1].Outcome = %#v; want the gate's cancelled fill-in", answers[1].Outcome)
	}
}

// TestDecisionMultiEscCancelsEverything pins esc against the drafts: it
// cancels EVERY question — including the ones already answered and annotated —
// rather than quietly committing the half of the batch that happened to be
// filled in. Esc means "I am not answering this", and it has meant that since
// the single-question prompt shipped.
func TestDecisionMultiEscCancelsEverything(t *testing.T) {
	sup, a, req := openMultiDecision(t)

	a = pressDecision(t, a, tea.KeyPressMsg{Text: "1"})
	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyTab})
	a = pressDecision(t, a, tea.KeyPressMsg{Text: "2"})
	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyEscape})

	if a.sess.HasPendingDecision() {
		t.Fatal("expected the prompt cleared by esc")
	}
	if len(sup.answers) != 1 {
		t.Fatalf("sup.answers = %+v; want exactly one AnswerDecision call from esc", sup.answers)
	}
	cancelledAnswers(t, sup.answers[0].answers, "q1", "q2")
	cancelledAnswers(t, req.await(t), "q1", "q2")
}

// TestDecisionMultiTabRestoresTheAnsweredRow covers the small navigation
// contract that keeps a batch reviewable: coming back to an answered question
// puts the cursor on the answer you gave it, not back on option 1 — so a
// second Enter re-confirms rather than silently overwriting.
func TestDecisionMultiTabRestoresTheAnsweredRow(t *testing.T) {
	_, a, req := openMultiDecision(t)
	defer req.stillBlocked(t)

	a = pressDecision(t, a, tea.KeyPressMsg{Text: "2"})          // q1 → option 2
	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyRight}) // → q2 (the strip's arrows)
	a = pressDecision(t, a, tea.KeyPressMsg{Code: tea.KeyLeft})  // ← back to q1

	if got := a.sess.pendingDec.cursor; got != 1 {
		t.Errorf("cursor on returning to an answered question = %d; want the answered row (1)", got)
	}
	if got := a.render(); !strings.Contains(got, "▸ 2  Wiring first, renderer after") {
		t.Errorf("expected the answered option focused on return:\n%s", got)
	}
}

// TestDecisionMultiSingleQuestionKeepsShippedKeys guards the shipped
// single-question contract from this file's additions: Tab and n bind nothing
// there, and its hint line still advertises exactly the keys it always did.
func TestDecisionMultiSingleQuestionKeepsShippedKeys(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDecisionTest(t, sup)
	a, req := openDecision(t, sup, a)
	defer req.stillBlocked(t)

	for _, msg := range []tea.KeyPressMsg{
		{Code: tea.KeyTab},
		{Code: tea.KeyTab, Mod: tea.ModShift},
		{Code: tea.KeyLeft},
		{Code: tea.KeyRight},
		{Text: "n"},
	} {
		a = pressDecision(t, a, msg)
	}
	if got := a.sess.pendingDec.tab; got != 0 {
		t.Errorf("tab = %d after the multi-only keys on a single question; want 0", got)
	}
	if a.sess.pendingDec.noting {
		t.Error("n opened the notes editor on a single-question request; want it unbound there")
	}
	if got := a.render(); !strings.Contains(got, "Enter to select · ↑/↓ to navigate · Esc to cancel") {
		t.Errorf("the single-question hint line changed:\n%s", got)
	}
	if len(sup.answers) != 0 {
		t.Errorf("sup.answers = %+v; want none — none of those keys answers anything", sup.answers)
	}
}
