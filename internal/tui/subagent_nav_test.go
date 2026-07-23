package tui_test

// subagent_nav_test.go covers the navigable half of docs/TUI.md § "Subagent
// sessions": drilling into a child session and back out to its parent, the
// roster's bulk stop binding, the transcript's background-agents block, and
// the tool block's originating-agent attribution.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

const (
	navRootID       = "0192a1b2-nav0-7000-8000-000000000001"
	navChildID      = "0192a1b2-nav0-7000-8000-000000000002"
	navGrandchildID = "0192a1b2-nav0-7000-8000-000000000003"
)

// navTree is the roster every navigation test below drives: a root session
// with one child and one grandchild beneath it, most-recently-active first so
// the root is the initially selected row and ↓ walks down the tree.
func navTree() []tui.SessionInfo {
	return []tui.SessionInfo{
		{
			ID:      navRootID,
			Title:   "ship the subagent nav",
			Summary: "one worker fanned out",
			Status:  tui.StatusWorking,
			Created: tui.GoldenNow.Add(-40 * time.Minute),
			Updated: tui.GoldenNow.Add(-10 * time.Second),
		},
		{
			ID:       navChildID,
			ParentID: navRootID,
			Agent:    "go-developer",
			Title:    "wire the return path",
			Summary:  "editing app.go",
			Status:   tui.StatusWorking,
			Depth:    1,
			Created:  tui.GoldenNow.Add(-8 * time.Minute),
			Updated:  tui.GoldenNow.Add(-20 * time.Second),
		},
		{
			ID:       navGrandchildID,
			ParentID: navChildID,
			Agent:    "go-reviewer",
			Title:    "review the return path",
			Summary:  "reading the diff",
			Status:   tui.StatusWorking,
			Depth:    2,
			Created:  tui.GoldenNow.Add(-2 * time.Minute),
			Updated:  tui.GoldenNow.Add(-30 * time.Second),
		},
	}
}

// attachedTo reports which session the app is currently attached to, by
// submitting a prompt and reading back the id Supervisor.Send was called with
// — the same indirection TestNavAttachSendsPrompt uses, and the only way this
// black-box package can observe the attachment.
func attachedTo(t *testing.T, sup *fakeSup, m tea.Model) string {
	t.Helper()
	before := len(sup.sent)
	m = type_(t, m, "ping")
	press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if len(sup.sent) != before+1 {
		t.Fatalf("submitting a prompt recorded %d sends, want exactly one more than %d", len(sup.sent), before)
	}
	id, _, _ := strings.Cut(sup.sent[len(sup.sent)-1], ":")
	return id
}

// TestNavEnterDrillsIntoChildSession is the drill-in contract the tree
// ordering already delivers, pinned so it can't regress: ↓ selects a child row
// and enter opens THAT child's own session, not its parent's. A child is an
// ordinary roster row, so this needed no new navigation model — which is
// exactly the claim worth a test.
func TestNavEnterDrillsIntoChildSession(t *testing.T) {
	sup := newFakeSup(navTree())
	m := newTestApp(t, sup)

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyDown})  // the child, depth-first under its root
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // open the child's own session

	if got := attachedTo(t, sup, m); got != navChildID {
		t.Errorf("attached session = %q; want the child %q", got, navChildID)
	}
}

// TestNavAttachLeftReturnsToParent is the return path: ← from an attached
// CHILD with an empty input lands on its PARENT's session — still the attach
// screen, now showing the parent — rather than dropping all the way to the
// overview. Pressed twice from the grandchild it walks the whole chain up one
// level at a time.
func TestNavAttachLeftReturnsToParent(t *testing.T) {
	sup := newFakeSup(navTree())
	m := newTestApp(t, sup)

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyDown}) // child
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyDown}) // grandchild
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := attachedTo(t, sup, m); got != navGrandchildID {
		t.Fatalf("attached session = %q; want the grandchild %q", got, navGrandchildID)
	}

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyLeft})
	if got := content(m); strings.Contains(got, "space peek") {
		t.Fatalf("← from a child fell through to the overview; want its parent's session:\n%s", got)
	}
	if got := attachedTo(t, sup, m); got != navChildID {
		t.Errorf("after ← the attached session = %q; want the parent %q", got, navChildID)
	}

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyLeft})
	if got := attachedTo(t, sup, m); got != navRootID {
		t.Errorf("after a second ← the attached session = %q; want the root %q", got, navRootID)
	}
}

// TestNavAttachLeftFromRootReturnsToOverview is the other half of the same
// contract: a ROOT session has no parent to return to, so ← keeps backing out
// to the overview exactly as it did before drill-out existed. The orphan case
// — a child whose parent is missing from the polled snapshot, which the roster
// already renders as a root — takes the same path.
func TestNavAttachLeftFromRootReturnsToOverview(t *testing.T) {
	orphan := navTree()[1]
	orphan.ParentID = "0192a1b2-nav0-7000-8000-00000000dead" // a parent no snapshot holds

	for _, tc := range []struct {
		name    string
		roster  []tui.SessionInfo
		attachs string
	}{
		{name: "root", roster: navTree(), attachs: navRootID},
		{name: "orphan", roster: []tui.SessionInfo{orphan}, attachs: orphan.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sup := newFakeSup(tc.roster)
			m := newTestApp(t, sup)

			m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})
			if got := attachedTo(t, sup, m); got != tc.attachs {
				t.Fatalf("attached session = %q; want %q", got, tc.attachs)
			}

			m = press(t, m, tea.KeyPressMsg{Code: tea.KeyLeft})
			if got := content(m); !strings.Contains(got, "space peek") {
				t.Errorf("expected ← to back out to the overview, got:\n%s", got)
			}
		})
	}
}

// selectedRowLabel returns the title-column label of the roster row carrying
// the selection caret, read off the rendered frame so it asserts what a user
// actually sees. It reports "" when no row is selected.
func selectedRowLabel(t *testing.T, rendered string) string {
	t.Helper()
	for _, line := range strings.Split(rendered, "\n") {
		runes := []rune(line)
		if len(runes) == 0 || runes[0] != '▸' {
			continue
		}
		if label, ok := rowLabel(line); ok {
			return label
		}
	}
	return ""
}

// TestNavAttachDownSelectsFirstChild is the binding the transcript's
// background-agents block advertises — "(↓ to manage)": ↓ on an attached
// session with an empty input returns to the roster with that session's FIRST
// child selected, which is where children are actually managed (peek, attach,
// ctrl-x, ctrl-t). Landing on the overview with the PARENT still selected
// would make the caption a lie in the same way "? shortcuts" was.
func TestNavAttachDownSelectsFirstChild(t *testing.T) {
	sup := newFakeSup(navTree())
	m := newTestApp(t, sup)

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // attach the root
	if got := content(m); !strings.Contains(got, "1 background agent launched") {
		t.Fatalf("expected the attached root to advertise its child:\n%s", got)
	}

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyDown})

	got := content(m)
	if !strings.Contains(got, "space peek") {
		t.Fatalf("expected ↓ to return to the overview, got:\n%s", got)
	}
	// The child row is labelled by its agent identity (see Overview.rowLabel).
	if label := selectedRowLabel(t, got); label != "go-developer" {
		t.Errorf("selected row = %q; want the first spawned child %q:\n%s", label, "go-developer", got)
	}
}

// TestNavAttachDownWithoutChildrenIsNoOp is the guard on the same key: a
// session that spawned nothing never renders the caption, so there is nothing
// for ↓ to honor — it must stay on the attach screen rather than navigating
// somewhere the user didn't ask for.
func TestNavAttachDownWithoutChildrenIsNoOp(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := newTestApp(t, sup)

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // attach a childless session
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyDown})

	if got := content(m); strings.Contains(got, "space peek") {
		t.Errorf("↓ on a childless session left the attach screen:\n%s", got)
	}
}

// TestNavAttachDownWithTextDoesNotNavigate pins the empty-input guard: ↓ is a
// navigation key only while the input is empty. With text pending it belongs
// to the shared input keymap — the key is not claimed here, so whatever that
// keymap does with it (nothing today, a history/completion move tomorrow)
// stays available, and the half-typed prompt is never dropped by a navigation
// the user didn't intend.
func TestNavAttachDownWithTextDoesNotNavigate(t *testing.T) {
	sup := newFakeSup(navTree())
	m := newTestApp(t, sup)

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // attach the root (has a child)
	m = type_(t, m, "half a prompt")
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyDown})

	got := content(m)
	if strings.Contains(got, "space peek") {
		t.Fatalf("↓ with text in the input navigated to the overview:\n%s", got)
	}
	if !strings.Contains(got, "> half a prompt▏") {
		t.Errorf("↓ with text disturbed the input buffer:\n%s", got)
	}
}

// TestStopAgentsKillsDescendantsOnly is the bulk stop binding: ctrl-t on the
// selected row issues one Supervisor.Kill per DESCENDANT — the whole subtree,
// not just the direct children — and never for the selected session itself,
// which ctrl-x is still the way to stop. Kill keeps the journal (repo
// invariant #4), so nothing here deletes anything.
func TestStopAgentsKillsDescendantsOnly(t *testing.T) {
	sup := newFakeSup(navTree())
	m := newTestApp(t, sup)

	m = press(t, m, tea.KeyPressMsg{Code: 't', Mod: ctrl})

	want := []string{"kill:" + navChildID, "kill:" + navGrandchildID}
	if strings.Join(sup.ops, "|") != strings.Join(want, "|") {
		t.Fatalf("sup.ops = %v; want one kill per descendant %v", sup.ops, want)
	}
	if got := content(m); !strings.Contains(got, "Stopping 2 subagents.") {
		t.Errorf("expected the bulk-stop status note, got:\n%s", got)
	}
}

// TestStopAgentsOnLeafSessionKillsNothing guards the destructive edge: a row
// with no subagents under it must not fall back to killing the row itself, and
// must say so rather than looking like a dropped key press.
func TestStopAgentsOnLeafSessionKillsNothing(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := newTestApp(t, sup)

	m = press(t, m, tea.KeyPressMsg{Code: 't', Mod: ctrl})

	if len(sup.ops) != 0 {
		t.Fatalf("sup.ops = %v; want no ops at all for a session with no subagents", sup.ops)
	}
	if got := content(m); !strings.Contains(got, "No subagents under this session.") {
		t.Errorf("expected the no-subagents status note, got:\n%s", got)
	}
}

// TestRosterHintNamesStopBindingWithinWidth pins the hint line's two forms: an
// ordinary roster keeps the hint it has always had, a tree roster names ctrl-t
// instead of the unhandled "?" entry, and neither overflows the 80-cell budget
// the line is truncated to (an over-long hint would ellipsize the LAST binding,
// which is the new one).
func TestRosterHintNamesStopBindingWithinWidth(t *testing.T) {
	hintOf := func(t *testing.T, o tui.Overview) string {
		t.Helper()
		for _, line := range strings.Split(testkit.Render(o, testkit.Width, testkit.Height), "\n") {
			if strings.Contains(line, "space peek") {
				return line
			}
		}
		t.Fatal("no hint line in the rendered roster")
		return ""
	}

	flat := hintOf(t, newOverview().WithSessions(tui.GoldenRoster()))
	if want := "enter open · space peek · tab toggle view · ctrl-x kill · ? shortcuts"; flat != want {
		t.Errorf("flat-roster hint = %q; want %q", flat, want)
	}

	tree := hintOf(t, newOverview().WithSessions(navTree()))
	if !strings.Contains(tree, "ctrl-t stop agents") {
		t.Errorf("tree-roster hint = %q; want it to name the ctrl-t stop binding", tree)
	}
	if len([]rune(tree)) > testkit.Width {
		t.Errorf("tree-roster hint is %d cells wide; want <= %d (it would truncate)", len([]rune(tree)), testkit.Width)
	}
	if strings.Contains(tree, "…") {
		t.Errorf("tree-roster hint was truncated: %q", tree)
	}
}

// toolCallEvents is the exact finished bash call TestGoldenToolCall captures,
// optionally attributed to agent. One builder for both tests so the attributed
// and un-attributed renders can differ ONLY in the agent id.
func toolCallEvents(agent string) []event.Event {
	started := event.NewToolCallStarted(sid, "call-1", "bash", json.RawMessage(`{"cmd":"echo hi"}`))
	started.Agent = agent
	return []event.Event{
		started,
		event.NewToolCallFinished(sid, "call-1", json.RawMessage(`{"cmd":"echo hi"}`), "hi", false, nil),
	}
}

// TestGoldenToolCallAttributed covers the transcript's originating-agent
// attribution: a tool.call.started carrying Agent renders
// "ToolName(args) · from the <agent> agent" alongside the existing caption, so
// a transcript interleaving a parent's calls with its subagents' reads
// unambiguously.
func TestGoldenToolCallAttributed(t *testing.T) {
	events := toolCallEvents("go-developer")
	render(t, "tool_call_attributed", events...)

	got := testkit.Render(ingest(events...), testkit.Width, testkit.Height)
	if want := "bash(echo hi)" + attributionSuffix + "go-developer agent"; !strings.Contains(got, want) {
		t.Errorf("attributed tool block missing %q:\n%s", want, got)
	}
}

// TestGoldenToolCallUnattributed is the fallback contract, and it is asserted
// against tool_call.golden — the golden captured BEFORE attribution existed —
// on purpose: "purely additive" means an event with no agent id renders the
// same bytes it always did, which only a pre-existing golden can prove. A
// second, absence-shaped assertion covers what a golden cannot state, that no
// placeholder clause can appear.
func TestGoldenToolCallUnattributed(t *testing.T) {
	got := testkit.Render(ingest(toolCallEvents("")...), testkit.Width, testkit.Height)
	testkit.AssertGolden(t, "tool_call", got)

	if strings.Contains(got, attributionSuffix) {
		t.Errorf("un-attributed tool block rendered an attribution clause %q:\n%s", attributionSuffix, got)
	}
}

// TestToolAttributionSurvivesTheApprovalWindow pins the reason the agent id
// rides the transcript ITEM and not only the per-call toolAgents map the
// approval prompt correlates through: that map is dropped on
// ToolCallFinished, while the block keeps naming its source afterwards.
func TestToolAttributionSurvivesTheApprovalWindow(t *testing.T) {
	m := ingest(toolCallEvents("go-developer")...) // started AND finished
	if got := testkit.Render(m, testkit.Width, testkit.Height); !strings.Contains(got, attributionSuffix+"go-developer agent") {
		t.Errorf("attribution lost once the call finished:\n%s", got)
	}
}

// spawnedChildren is the two-child fixture the background-agents block lists.
func spawnedChildren() []tui.SessionInfo {
	return []tui.SessionInfo{
		{ID: navChildID, ParentID: navRootID, Agent: "go-developer", Title: "wire the return path"},
		{ID: navGrandchildID, ParentID: navRootID, Agent: "go-reviewer", Title: "review the return path"},
	}
}

// TestGoldenBackgroundAgents covers the transcript block a session gets once it
// has spawned children: the count line plus one line per child, naming it and
// the agent it runs as.
func TestGoldenBackgroundAgents(t *testing.T) {
	m := ingest(
		event.NewMessageStarted(sid, event.MessageUser),
		event.NewMessageFinished(sid, event.MessageUser, "Fan out two workers."),
	).WithBackgroundAgents(spawnedChildren())

	got := testkit.Render(m, testkit.Width, testkit.Height)
	testkit.AssertGolden(t, "background_agents", got)

	if want := "2 background agents launched (↓ to manage)"; !strings.Contains(got, want) {
		t.Errorf("background-agents block missing %q:\n%s", want, got)
	}
}

// TestBackgroundAgentsAbsentWithoutChildren is the additive half: a session
// that spawned nothing renders byte-for-byte the transcript it rendered before
// this block existed, and the block never appears empty or zero-counted.
func TestBackgroundAgentsAbsentWithoutChildren(t *testing.T) {
	base := ingest(
		event.NewMessageStarted(sid, event.MessageUser),
		event.NewMessageFinished(sid, event.MessageUser, "Fan out two workers."),
	)
	want := testkit.Render(base, testkit.Width, testkit.Height)
	got := testkit.Render(base.WithBackgroundAgents(nil), testkit.Width, testkit.Height)

	if got != want {
		t.Errorf("WithBackgroundAgents(nil) changed the render:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if strings.Contains(got, "background agent") {
		t.Errorf("a childless session rendered a background-agents block:\n%s", got)
	}
}

// TestBackgroundAgentsNameFallbacks covers the per-child label rules: a titled
// child shows "title · agent", an untitled one falls back to its agent id and
// states it once rather than twice, and a child with neither is still findable
// by a short id.
func TestBackgroundAgentsNameFallbacks(t *testing.T) {
	m := tui.New(theme.Test()).WithBackgroundAgents([]tui.SessionInfo{
		{ID: navChildID, ParentID: navRootID, Agent: "go-developer", Title: "wire the return path"},
		{ID: navGrandchildID, ParentID: navRootID, Agent: "go-reviewer"},
		{ID: "0192a1b2-nav0-7000-8000-000000000004", ParentID: navRootID},
	})
	got := testkit.Render(m, testkit.Width, testkit.Height)

	for _, want := range []string{"wire the return path · go-developer", "0192a1b2"} {
		if !strings.Contains(got, want) {
			t.Errorf("background-agents block missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "go-reviewer · go-reviewer") {
		t.Errorf("an untitled child repeated its agent id:\n%s", got)
	}
	if !strings.Contains(got, "go-reviewer") {
		t.Errorf("an untitled child lost its agent id entirely:\n%s", got)
	}
}

// TestAttachRendersSpawnedChildrenFromTheRoster wires the block to its data
// source: the children are a ROSTER fact (a subagent is a separate session with
// its own stream), so attaching to a parent must list them without anything
// having been ingested on the parent's own stream — and attaching to a leaf
// must not.
func TestAttachRendersSpawnedChildrenFromTheRoster(t *testing.T) {
	sup := newFakeSup(navTree())
	m := newTestApp(t, sup)

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // attach the root
	if got := content(m); !strings.Contains(got, "1 background agent launched") {
		t.Errorf("attached parent did not list its spawned child:\n%s", got)
	}

	leaf := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	leaf = press(t, leaf, tea.KeyPressMsg{Code: tea.KeyEnter})
	if got := content(leaf); strings.Contains(got, "background agent") {
		t.Errorf("attached leaf session rendered a background-agents block:\n%s", got)
	}
}
