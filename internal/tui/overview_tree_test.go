package tui_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
)

// The roster's subagent tree render (docs/TUI.md § "Subagent sessions"): a
// parent at the root with its children indented beneath it, each child row
// carrying its agent identity, run duration and token tally, and a gutter
// marker wherever something below the row is awaiting the user.

const (
	treeRootID  = "0192a1b2-tree-7000-8000-000000000001"
	treeChildID = "0192a1b2-tree-7000-8000-000000000002"
)

// treeSession builds one subagent row: the fields a child needs beyond an
// ordinary roster row (parent link, agent identity) plus a run window and a
// usage tally the right column renders.
func treeSession(id, parent, agent, summary string, ran time.Duration, tokens int) tui.SessionInfo {
	updated := tui.GoldenNow.Add(-30 * time.Second)
	return tui.SessionInfo{
		ID:       id,
		ParentID: parent,
		Agent:    agent,
		Title:    "spawned by " + parent,
		Summary:  summary,
		Status:   tui.StatusWorking,
		Depth:    1,
		Usage:    provider.Usage{InputTokens: tokens},
		Created:  updated.Add(-ran),
		Updated:  updated,
	}
}

// twoLevelFixture is a root session with two subagents beneath it — the shape
// docs/TUI.md sketches.
func twoLevelFixture() []tui.SessionInfo {
	return []tui.SessionInfo{
		{
			ID:      treeRootID,
			Title:   "ship the subagent roster",
			Summary: "two workers fanned out",
			Status:  tui.StatusWorking,
			Created: tui.GoldenNow.Add(-42 * time.Minute),
			Updated: tui.GoldenNow.Add(-20 * time.Second),
			Usage:   provider.Usage{InputTokens: 214700},
		},
		treeSession(treeChildID, treeRootID, "tui-inline-perm-owner", "editing overview_render.go", 5*time.Minute+9*time.Second, 214700),
		treeSession("0192a1b2-tree-7000-8000-000000000003", treeRootID, "go-developer", "running the golden suite", 6*time.Minute+47*time.Second, 128000),
	}
}

// threeLevelFixture nests a grandchild under the two-level tree's first child,
// so the indent has to compose rather than just fire once.
func threeLevelFixture() []tui.SessionInfo {
	return append(twoLevelFixture(),
		treeSession("0192a1b2-tree-7000-8000-000000000004", treeChildID, "go-reviewer", "reviewing the row renderer", 42*time.Second, 8400),
	)
}

// TestGoldenOverviewTreeTwoLevel locks the two-level render: the parent at the
// root, its children indented two cells inside the title column (so every
// other column stays aligned), each child labelled by its agent, and the wide
// "<elapsed> · ↓ <N> tokens" right column a tree roster switches to.
func TestGoldenOverviewTreeTwoLevel(t *testing.T) {
	o := newOverview().WithSessions(twoLevelFixture())
	testkit.AssertGolden(t, "overview_tree_two_level", testkit.Render(o, testkit.Width, testkit.Height))
}

// TestGoldenOverviewTreeThreeLevel locks the composed indent: the grandchild
// sits two cells further in than its parent, and depth-first order puts it
// immediately under that parent rather than at the end of the roster.
func TestGoldenOverviewTreeThreeLevel(t *testing.T) {
	o := newOverview().WithSessions(threeLevelFixture())
	testkit.AssertGolden(t, "overview_tree_three_level", testkit.Render(o, testkit.Width, testkit.Height))
}

// TestOverviewTreeOrderIsDepthFirst asserts the ordering contract behind the
// goldens above in terms the goldens can't state directly: every descendant
// follows its parent immediately, even though the grandchild here is the
// LEAST recently active row and a plain recency ordering would sink it to the
// bottom.
func TestOverviewTreeOrderIsDepthFirst(t *testing.T) {
	sessions := threeLevelFixture()
	// Age the grandchild well past every other row: recency alone would put it
	// last; the tree order must still keep it directly under its parent.
	sessions[3].Updated = tui.GoldenNow.Add(-3 * time.Hour)
	o := newOverview().WithSessions(sessions)

	got := labelOrder(t, o)
	want := []string{"ship the subagent roster", "tui-inline-perm-owner", "go-reviewer", "go-developer"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("tree row order = %v; want depth-first %v", got, want)
	}
}

// TestGoldenOverviewTreeBlockedAncestor is the rollup oracle: a pending
// approval on the GRANDCHILD must light the gutter marker on the child AND the
// root, so a supervisor sees "something below here needs me" without
// descending. Rows with nothing blocked below them stay unmarked — otherwise
// the marker would say nothing at all.
func TestGoldenOverviewTreeBlockedAncestor(t *testing.T) {
	sessions := threeLevelFixture()
	sessions[3].Pending = 1 // the grandchild, three levels down
	o := newOverview().WithSessions(sessions)
	got := testkit.Render(o, testkit.Width, testkit.Height)
	testkit.AssertGolden(t, "overview_tree_blocked_ancestor", got)

	marked := markedLabels(t, got)
	want := []string{"ship the subagent roster", "tui-inline-perm-owner", "go-reviewer"}
	if strings.Join(marked, "|") != strings.Join(want, "|") {
		t.Errorf("gutter-marked rows = %v; want the blocked grandchild and both of its ancestors %v\n%s", marked, want, got)
	}
	// The unrelated sibling must NOT be marked — a marker on every row is the
	// same as no marker at all.
	for _, label := range marked {
		if label == "go-developer" {
			t.Errorf("unrelated sibling go-developer carries the blocked marker:\n%s", got)
		}
	}
}

// TestGoldenStyledOverviewTreeBlockedAncestor is the blocked-marker's color
// oracle: under termenv.Ascii every glyph renders identically, so only a
// colored render can assert the gutter "!" is WARN-colored rather than plain
// text. It doubles as the #61 guard for the prefix column — the marker is
// styled before the single padTo that sizes the column, and a regression that
// padded first would show up here as shifted columns.
func TestGoldenStyledOverviewTreeBlockedAncestor(t *testing.T) {
	sessions := threeLevelFixture()
	sessions[3].Pending = 1
	o := tui.NewOverview(testkit.ColorTheme(), tui.GoldenMeta()).WithSessions(sessions)
	testkit.AssertGoldenStyled(t, "overview_tree_blocked_ancestor", testkit.Render(o, testkit.Width, testkit.Height))
}

// TestColorOverviewTreeLayout is the tree roster's #61 display-width check:
// the styled gutter marker, indent, status word and tally column must not
// change the geometry a plain render produces, at a normal width and a narrow
// one where every column is fighting for cells.
func TestColorOverviewTreeLayout(t *testing.T) {
	sessions := threeLevelFixture()
	sessions[3].Pending = 1
	for _, width := range []int{120, 80, 40} {
		t.Run(fmt.Sprintf("width=%d", width), func(t *testing.T) {
			plain := testkit.Render(newOverview().WithSessions(sessions), width, testkit.Height)
			colored := testkit.Render(tui.NewOverview(testkit.ColorTheme(), tui.GoldenMeta()).WithSessions(sessions), width, testkit.Height)
			assertColorLayout(t, plain, colored, width)
		})
	}
}

// TestGoldenOverviewTreeOverflow locks the "↓ N more" indicator: a tree taller
// than its row budget reports how much it is hiding on its last visible line,
// instead of silently ending.
func TestGoldenOverviewTreeOverflow(t *testing.T) {
	sessions := twoLevelFixture()
	for i := range 6 {
		sessions = append(sessions, treeSession(
			fmt.Sprintf("0192a1b2-tree-7000-8000-0000000001%02d", i),
			treeRootID,
			fmt.Sprintf("worker-%d", i),
			"working",
			time.Duration(i+1)*time.Minute,
			1000*(i+1),
		))
	}
	const height = 14 // 4 header + 3 dispatch rows leaves a 7-row body
	got := testkit.Render(newOverview().WithSessions(sessions), testkit.Width, height)
	testkit.AssertGolden(t, "overview_tree_overflow", got)

	if !strings.Contains(got, "more") {
		t.Errorf("overflowing tree rendered no overflow indicator:\n%s", got)
	}
}

// TestGoldenOverviewTreeOrphan covers the polled-snapshot reality: a child
// whose parent is absent from the roster (archived, or simply polled between
// two writes) renders as a ROOT — unindented, never dropped — because
// indenting it under a parent nobody can see reads as a render bug.
func TestGoldenOverviewTreeOrphan(t *testing.T) {
	sessions := twoLevelFixture()[1:] // drop the root, orphaning both children
	o := newOverview().WithSessions(sessions)
	got := testkit.Render(o, testkit.Width, testkit.Height)
	testkit.AssertGolden(t, "overview_tree_orphan", got)

	for _, label := range labelOrder(t, o) {
		if label != "tui-inline-perm-owner" && label != "go-developer" {
			t.Errorf("unexpected row %q; both orphans must render, and nothing else", label)
		}
	}
	for _, line := range rosterLines(t, got) {
		if strings.HasPrefix(line, "   ") {
			t.Errorf("orphan row is indented under an absent parent: %q\n%s", line, got)
		}
	}
}

// TestOverviewTreeNeverDropsARowOnACycle proves the defensive visited set: a
// parent chain that loops (never produced by the supervisor) must not hang the
// render or lose a session, which a naive depth-first walk over "everything
// that isn't a root" would do — a cycle has no root.
func TestOverviewTreeNeverDropsARowOnACycle(t *testing.T) {
	a := treeSession("cycle-a", "cycle-b", "agent-a", "a", time.Minute, 100)
	b := treeSession("cycle-b", "cycle-a", "agent-b", "b", time.Minute, 100)
	o := newOverview().WithSessions([]tui.SessionInfo{a, b})

	got := labelOrder(t, o)
	if len(got) != 2 {
		t.Errorf("cycle rendered %d rows (%v); want both sessions, none dropped", len(got), got)
	}
}

// TestOverviewNoSubagentsRendersUnchanged is the backward-compatibility guard:
// a roster with no subagent in it must render BYTE-IDENTICALLY to the roster
// golden captured before the tree render existed. The tree affordances (the
// wide tally column, the gutter, the indent) are all gated on the roster
// actually containing a subagent precisely so an ordinary roster — which is
// almost every roster — keeps its full-width summary column.
//
// It reads the golden file directly rather than going through
// testkit.AssertGolden so that a `-update` run cannot quietly recapture it: if
// this file changes, that is a defect to investigate, not a golden to refresh.
func TestOverviewNoSubagentsRendersUnchanged(t *testing.T) {
	path := filepath.Join("testdata", "overview_flat.golden")
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	plain := rosterFixture()
	got := testkit.Render(newOverview().WithSessions(plain), testkit.Width, testkit.Height)
	if got != string(want) {
		t.Errorf("a subagent-free roster no longer renders identically to %s — the tree render leaked into the ordinary path\n--- got ---\n%s\n--- want ---\n%s", path, got, string(want))
	}
	if strings.Contains(got, "tokens") {
		t.Errorf("subagent-free roster rendered the wide token tally column:\n%s", got)
	}

	// Guard against the assertion above being vacuous: the SAME roster with one
	// subagent link added must render differently. If it doesn't, the comparison
	// proves nothing about the gating.
	withChild := append(plain, treeSession(treeChildID, plain[0].ID, "go-developer", "spawned", time.Minute, 2048))
	tree := testkit.Render(newOverview().WithSessions(withChild), testkit.Width, testkit.Height)
	if tree == got {
		t.Fatal("adding a subagent changed nothing about the render; the byte-identity assertion above is vacuous")
	}
}

// rosterLines returns the rendered roster body's non-blank lines: everything
// between the 4-line header and the dispatch bar's rule.
func rosterLines(t *testing.T, rendered string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(rendered, "\n")[4:] {
		if strings.HasPrefix(line, "─") {
			break
		}
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// labelOrder returns the title-column label of each rendered session row, in
// render order — the cwd group header and any overflow indicator excluded. It
// reads the rendered frame rather than the model so it asserts what a user
// actually sees.
func labelOrder(t *testing.T, o tui.Overview) []string {
	t.Helper()
	var out []string
	for _, line := range rosterLines(t, testkit.Render(o, testkit.Width, testkit.Height)) {
		if label, ok := rowLabel(line); ok {
			out = append(out, label)
		}
	}
	return out
}

// markedLabels returns the labels of the rows carrying the blocked gutter
// marker, in render order.
func markedLabels(t *testing.T, rendered string) []string {
	t.Helper()
	var out []string
	for _, line := range rosterLines(t, rendered) {
		label, ok := rowLabel(line)
		// Rune-indexed: the caret in cell 0 is multi-byte.
		if runes := []rune(line); !ok || len(runes) < rowPrefixCells || runes[1] != '!' {
			continue
		}
		out = append(out, label)
	}
	return out
}

// rowLabel extracts a session row's title-column text: the 28-cell title
// column after the 2-cell prefix, trimmed. It reports false for a line that
// isn't a session row (a cwd group header, an overflow indicator), both of
// which start at column 0 rather than behind the prefix column.
func rowLabel(line string) (string, bool) {
	runes := []rune(line)
	if len(runes) <= rowPrefixCells {
		return "", false
	}
	// A session row's first two cells are the prefix column (caret + blocked
	// gutter); a cwd group header and the overflow indicator both start their
	// text at column 0 instead.
	if strings.Trim(string(runes[:rowPrefixCells]), " ▸!") != "" {
		return "", false
	}
	end := min(len(runes), rowPrefixCells+rowTitleCells)
	label := strings.TrimSpace(string(runes[rowPrefixCells:end]))
	return label, label != ""
}

// Mirrors overview_render.go's rowPrefixW/rowTitleW for this black-box test
// package.
const (
	rowPrefixCells = 2
	rowTitleCells  = 28
)
