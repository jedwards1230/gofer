package tui_test

import (
	"strings"
	"testing"

	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// newPeek builds a peek over the shared roster fixture with an empty reply
// buffer — the roster rail plus the selected session's summary card.
func newPeek() tui.Peek {
	over := newOverview().WithSessions(rosterFixture())
	return tui.NewPeek(theme.Test(), over, "")
}

// TestGoldenPeekNarrow renders the peek card at the standard 80-column width:
// the roster rail above the selected session's summary card (title, waiting
// line, ❯ reply input, footer).
func TestGoldenPeekNarrow(t *testing.T) {
	testkit.AssertGolden(t, "peek_narrow", testkit.Render(newPeek(), testkit.Width, testkit.Height))
}

// TestGoldenPeekWide renders the same peek card at 130 columns — peek no longer
// splits into panes, so a wide terminal just gives the rail and card more room.
func TestGoldenPeekWide(t *testing.T) {
	testkit.AssertGolden(t, "peek_wide", testkit.Render(newPeek(), 130, testkit.Height))
}

// TestGoldenPeekNextSession renders the peek after moving the selection to the
// next session, so the card reflects the newly selected (needs-input) session.
func TestGoldenPeekNextSession(t *testing.T) {
	testkit.AssertGolden(t, "peek_next_session", testkit.Render(newPeek().NextSession(), testkit.Width, testkit.Height))
}

// TestGoldenPeekReply renders the peek card with a typed reply, locking the
// ❯ input's populated render.
func TestGoldenPeekReply(t *testing.T) {
	over := newOverview().WithSessions(rosterFixture())
	p := tui.NewPeek(theme.Test(), over, "status?")
	testkit.AssertGolden(t, "peek_reply", testkit.Render(p, testkit.Width, testkit.Height))
}

// TestPeekSessionSwitch verifies NextSession/PrevSession move the peeked
// session and clamp at the roster ends.
func TestPeekSessionSwitch(t *testing.T) {
	p := newPeek()
	first := p.SelectedID()
	if got := p.PrevSession().SelectedID(); got != first {
		t.Errorf("PrevSession at top moved selection: got %q want %q", got, first)
	}
	next := p.NextSession().SelectedID()
	if next == first {
		t.Error("NextSession did not move the selection")
	}
}

// TestPeekShowsSessionModelOverride verifies the peeked session's own model
// — "current model should be visible in the peek menu" per the redesign
// brief — rides the card's waiting line when the session carries an
// explicit override (SessionInfo.Model), covering the case app_peek.golden's
// fixture (GoldenRoster, no per-session override) can't: that golden's
// session resolves to the header's own meta.Model, already visible one line
// up, so showing it a second time on the card would only repeat it (see
// Peek.View's doc).
func TestPeekShowsSessionModelOverride(t *testing.T) {
	roster := tui.GoldenRoster()
	roster[0].Model = "claude-opus-4-8"
	over := tui.NewOverview(theme.Test(), tui.GoldenMeta()).WithSessions(roster)
	p := tui.NewPeek(theme.Test(), over, "")

	got := testkit.Render(p, testkit.Width, testkit.Height)
	if !strings.Contains(got, "claude-opus-4-8 · working") {
		t.Errorf("peek card missing the session's model override on the waiting line:\n%s", got)
	}
}

// TestPeekHidesModelWhenSessionHasNoOverride verifies the waiting line stays
// exactly as it was pre-redesign when the peeked session carries no
// override (SessionInfo.Model == "") — the byte-for-byte guarantee
// app_peek.golden depends on: GoldenRoster's fixture sessions set no
// per-session Model (unlike overview_test.go's rosterFixture, which sets one
// on every session for its own, unrelated golden coverage).
func TestPeekHidesModelWhenSessionHasNoOverride(t *testing.T) {
	over := tui.NewOverview(theme.Test(), tui.GoldenMeta()).WithSessions(tui.GoldenRoster())
	got := testkit.Render(tui.NewPeek(theme.Test(), over, ""), testkit.Width, testkit.Height)
	// The unprefixed waiting line's own leading two spaces rule out a model
	// prefix sneaking in before "working" — "  claude-x · working …" would
	// not contain this exact substring.
	if !strings.Contains(got, "  working 2 minutes") {
		t.Errorf("peek card's waiting line changed shape for a session with no override:\n%s", got)
	}
}
