package tui

// app_internal_test.go lives in package tui (not tui_test) because it needs
// to construct the app root's unexported messages (rosterMsg, subReadyMsg,
// sessEventMsg) directly — the only way to seed a golden render or set up
// the stale-event guard without spinning a real bubbletea runtime. Anything
// drivable through App's exported Update/View surface instead lives in
// app_test.go (package tui_test) alongside the fake Supervisor and the
// behavioral navigation-contract tests.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/event"
	"github.com/jedwards1230/agent-sdk-go/provider"

	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// internalFakeSup is a minimal Supervisor backed by real event.Brokers,
// just enough to resolve App's subscribe/waitForEvent plumbing for the
// golden tests below.
type internalFakeSup struct {
	mu      sync.Mutex
	roster  []SessionInfo
	brokers map[string]*event.Broker
	replies []replyCall
}

// replyCall records one Supervisor.Reply invocation for the approval
// prompt's behavioral tests to assert against.
type replyCall struct {
	sessionID string
	id        string
	allow     bool
	remember  bool
}

func newInternalFakeSup(roster []SessionInfo) *internalFakeSup {
	return &internalFakeSup{roster: roster, brokers: map[string]*event.Broker{}}
}

func (f *internalFakeSup) broker(id string) *event.Broker {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.brokers[id]
	if !ok {
		b = event.NewBroker()
		f.brokers[id] = b
	}
	return b
}

func (f *internalFakeSup) Roster(context.Context) ([]SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SessionInfo(nil), f.roster...), nil
}

func (f *internalFakeSup) Subscribe(_ context.Context, id string) (*event.Subscription, error) {
	return f.broker(id).Subscribe(event.FilterAll, 16), nil
}

func (f *internalFakeSup) Create(_ context.Context, prompt string, _ CreateOptions) (SessionInfo, error) {
	return SessionInfo{ID: "created-1", Title: prompt, Status: StatusWorking}, nil
}

func (f *internalFakeSup) Send(context.Context, string, string) error     { return nil }
func (f *internalFakeSup) Interrupt(context.Context, string) error        { return nil }
func (f *internalFakeSup) Kill(context.Context, string) error             { return nil }
func (f *internalFakeSup) Archive(context.Context, string) error          { return nil }
func (f *internalFakeSup) SetModel(context.Context, string, string) error { return nil }

func (f *internalFakeSup) Reply(_ context.Context, sessionID, id string, allow, remember bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, replyCall{sessionID: sessionID, id: id, allow: allow, remember: remember})
	return nil
}

// newAppForGolden builds an App wired to a fresh internalFakeSup, sized and
// with the roster seeded via a real Update(rosterMsg{...}) round trip.
func newAppForGolden(t *testing.T, sup *internalFakeSup) App {
	t.Helper()
	a := NewApp(theme.Test(), sup, GoldenMeta(), GoldenCommandEnv())

	mdl, _ := a.Update(tea.WindowSizeMsg{Width: testkit.Width, Height: testkit.Height})
	a = mdl.(App)

	mdl, _ = a.Update(rosterMsg{sessions: GoldenRoster()})
	return mdl.(App)
}

// TestGoldenAppOverview renders the freshly seeded roster screen — App's
// default screen after the first roster fetch resolves.
func TestGoldenAppOverview(t *testing.T) {
	a := newAppForGolden(t, newInternalFakeSup(GoldenRoster()))
	testkit.AssertGolden(t, "app_overview", a.render())
}

// TestGoldenAppPeek reaches the peek screen by pressing enter on the
// (recency-first) selected session. Peek no longer subscribes — this is a
// pure Update/render round trip, unlike TestGoldenAppAttach below.
func TestGoldenAppPeek(t *testing.T) {
	a := newAppForGolden(t, newInternalFakeSup(GoldenRoster()))

	mdl, _ := a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	a = mdl.(App)
	if a.scr != screenPeek {
		t.Fatalf("scr = %v; want screenPeek", a.scr)
	}

	testkit.AssertGolden(t, "app_peek", a.render())
}

// TestGoldenAppAttach reaches the attach screen by pressing → on the
// selected session, resolves the subscribe round trip, feeds the same
// transcript, and types a pending reply into the input line before
// rendering.
func TestGoldenAppAttach(t *testing.T) {
	a := newAppForGolden(t, newInternalFakeSup(GoldenRoster()))

	mdl, cmd := a.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	a = mdl.(App)
	if a.scr != screenAttach {
		t.Fatalf("scr = %v; want screenAttach", a.scr)
	}
	if cmd == nil {
		t.Fatal("expected a subscribe cmd after entering attach")
	}
	mdl, _ = a.Update(cmd())
	a = mdl.(App)

	for _, ev := range appTranscriptEvents(a.sessID) {
		mdl, _ = a.Update(sessEventMsg{id: a.sessID, ev: ev})
		a = mdl.(App)
	}

	for _, r := range "ship it" {
		mdl, _ = a.Update(tea.KeyPressMsg{Text: string(r)})
		a = mdl.(App)
	}

	testkit.AssertGolden(t, "app_attach", a.render())
}

// appTranscriptEvents is a small, fixed turn shared by the peek and attach
// goldens so both show the same populated transcript. It leads with the
// user's own prompt — event.NewMessageStarted/Finished(MessageUser), the
// shape runner.Prompt publishes (see event.MessageUser's doc) — so both
// goldens also cover the App root rendering the user turn above the agent's
// reply.
func appTranscriptEvents(sid string) []event.Event {
	return []event.Event{
		event.NewMessageStarted(sid, event.MessageUser),
		event.NewMessageFinished(sid, event.MessageUser, "Wire the app root."),
		event.NewTurnStarted(sid),
		event.NewMessageStarted(sid, event.MessageText),
		event.NewMessageFinished(sid, event.MessageText, "Wired the app root; nav contract is in."),
		event.NewTurnFinished(sid, "end_turn", provider.Usage{InputTokens: 20, OutputTokens: 11}),
	}
}

// TestAttachOpenPreselectsAndIngests verifies OverviewMeta.AttachSessionID
// opens on the attach screen, pre-selects the session in the roster (so ← lands
// on it), and that events for that session ingest into the transcript.
func TestAttachOpenPreselectsAndIngests(t *testing.T) {
	a := NewApp(theme.Test(), &internalFakeSup{}, OverviewMeta{AttachSessionID: "sess-x"}, CommandEnv{})
	if a.scr != screenAttach {
		t.Fatalf("scr = %v; want screenAttach", a.scr)
	}
	if a.over.selectedID != "sess-x" {
		t.Errorf("pre-selected id = %q; want sess-x", a.over.selectedID)
	}

	mdl, _ := a.Update(sessEventMsg{id: "sess-x", ev: event.NewSessionError("sess-x", "boom", true)})
	if got := len(mdl.(App).sess.items); got != 1 {
		t.Errorf("attached-session event not ingested: %d transcript items, want 1", got)
	}
}

// TestRenderBeforeWindowSize guards the very first frame: bubbletea calls View
// before the initial WindowSizeMsg, so a.width/height are 0 and the content
// budget h goes negative after the padding/footer slices. render must not
// panic. Regression for the command-menu block (#86), which sliced
// menuLines[:h] with a negative bound before this frame ever had room.
func TestRenderBeforeWindowSize(t *testing.T) {
	cases := []struct {
		name string
		meta OverviewMeta
	}{
		{"overview", GoldenMeta()},
		{"attach", OverviewMeta{AttachSessionID: "sess-x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := NewApp(theme.Test(), &internalFakeSup{}, tc.meta, GoldenCommandEnv())
			// No WindowSizeMsg sent: a.width == a.height == 0, the first frame.
			_ = a.View() // must not panic
		})
	}
}

// TestRenderNoPanicAtTinyHeights extends TestRenderBeforeWindowSize's
// no-panic guarantee to the small terminal sizes most likely to underflow
// the bottom-anchoring filler math: [Model.view]'s avail clamp and
// [Overview.render]'s bodyAvail clamp both floor at 0 before any
// strings.Repeat/slice op, but a height of 0, 1, or 2 (and a small-but-real
// terminal) is exactly where a regression in that flooring would surface as
// a negative-count strings.Repeat or an out-of-range slice bound — the #87
// class of bug this PR's bottom-anchoring math must not reintroduce.
//
// The scroll dimension extends the same guarantee to this PR's own new
// underflow surface: [scrollTail] (shared by the attach screen's
// header+transcript scroll and the roster's own mouse-wheel/PgUp-PgDn
// scroll) computes start/end slice bounds from avail and an arbitrary
// offset — a huge offset (larger than the content will ever be) at a tiny
// or zero avail is exactly where a clamping regression would surface as an
// out-of-range slice, so it's exercised at every size/screen combination
// below alongside the pre-existing scroll-0 (tail) case.
func TestRenderNoPanicAtTinyHeights(t *testing.T) {
	screens := []struct {
		name string
		meta OverviewMeta
	}{
		{"overview", GoldenMeta()},
		{"attach", OverviewMeta{AttachSessionID: "sess-x"}},
	}
	sizes := []struct {
		name          string
		width, height int
	}{
		{"height0", 80, 0},
		{"height1", 80, 1},
		{"height2", 80, 2},
		{"tiny", 10, 5},
	}
	scrolls := []int{0, 1, scrollPageLines, 1_000_000}
	for _, scr := range screens {
		for _, sz := range sizes {
			for _, sc := range scrolls {
				t.Run(fmt.Sprintf("%s/%s/scroll=%d", scr.name, sz.name, sc), func(t *testing.T) {
					a := NewApp(theme.Test(), &internalFakeSup{}, scr.meta, GoldenCommandEnv())
					mdl, _ := a.Update(tea.WindowSizeMsg{Width: sz.width, Height: sz.height})
					a = mdl.(App)
					a.scroll = sc
					_ = a.render() // must not panic
				})
			}
		}
	}
}

// TestRenderNoPanicAtTinyHeightsWithContent extends the tiny-height guard
// further: a populated transcript (attach) and roster (overview) — the
// header+transcript combined doc ([Model.view]) and the roster's own scroll
// path ([Overview.body]) both have real content to slice at these sizes, not
// just the empty/zero-item case the fixtures above cover.
func TestRenderNoPanicAtTinyHeightsWithContent(t *testing.T) {
	sizes := []struct{ width, height int }{
		{80, 0}, {80, 1}, {80, 2}, {10, 5},
	}
	scrolls := []int{0, scrollPageLines, 1_000_000}

	t.Run("overview", func(t *testing.T) {
		for _, sz := range sizes {
			for _, sc := range scrolls {
				t.Run(fmt.Sprintf("%dx%d/scroll=%d", sz.width, sz.height, sc), func(t *testing.T) {
					a := newAppForGolden(t, newInternalFakeSup(GoldenRoster()))
					mdl, _ := a.Update(tea.WindowSizeMsg{Width: sz.width, Height: sz.height})
					a = mdl.(App)
					a.scroll = sc
					_ = a.render() // must not panic
				})
			}
		}
	})

	t.Run("attach", func(t *testing.T) {
		a := newAppForGolden(t, newInternalFakeSup(GoldenRoster()))
		mdl, cmd := a.Update(tea.KeyPressMsg{Code: tea.KeyRight})
		a = mdl.(App)
		mdl, _ = a.Update(cmd())
		a = mdl.(App)
		for _, ev := range appTranscriptEvents(a.sessID) {
			mdl, _ = a.Update(sessEventMsg{id: a.sessID, ev: ev})
			a = mdl.(App)
		}
		for _, sz := range sizes {
			for _, sc := range scrolls {
				t.Run(fmt.Sprintf("%dx%d/scroll=%d", sz.width, sz.height, sc), func(t *testing.T) {
					mdl, _ := a.Update(tea.WindowSizeMsg{Width: sz.width, Height: sz.height})
					b := mdl.(App)
					b.scroll = sc
					_ = b.render() // must not panic
				})
			}
		}
	})
}

// TestBottomAnchoredOverviewInput verifies the overview dispatch bar — the
// rule/input/hint block [Overview.dispatch] renders — lands on the render's
// last rows at a normal terminal size, with blank filler directly above it
// when the roster is short, instead of trailing the roster rows the way a
// top-anchored frame would.
func TestBottomAnchoredOverviewInput(t *testing.T) {
	a := newAppForGolden(t, newInternalFakeSup(GoldenRoster()))

	rows := strings.Split(a.render(), "\n")
	if len(rows) != testkit.Height {
		t.Fatalf("render() produced %d rows; want exactly testkit.Height (%d) — the bottom-anchored frame must still total a.height", len(rows), testkit.Height)
	}

	rule := strings.Repeat("─", testkit.Width)
	last := len(rows) - 1
	if rows[last-2] != rule {
		t.Errorf("row %d (dispatch rule) = %q; want the full-width rule", last-2, rows[last-2])
	}
	if !strings.HasPrefix(rows[last-1], "❯") {
		t.Errorf("row %d (dispatch input) = %q; want it to start with the ❯ prompt", last-1, rows[last-1])
	}
	if rows[last-3] != "" {
		t.Errorf("row %d = %q; want blank filler directly above the pinned dispatch bar", last-3, rows[last-3])
	}
}

// TestBottomAnchoredAttachInput is TestBottomAnchoredOverviewInput's attach
// counterpart: the input's framing rules + input line [Model.view] renders
// land on the render's last rows, with blank filler above them when the
// transcript is short (here, empty — a freshly opened attach with no events
// ingested yet).
func TestBottomAnchoredAttachInput(t *testing.T) {
	a := NewApp(theme.Test(), &internalFakeSup{}, OverviewMeta{AttachSessionID: "sess-x"}, GoldenCommandEnv())
	mdl, _ := a.Update(tea.WindowSizeMsg{Width: testkit.Width, Height: testkit.Height})
	a = mdl.(App)

	rows := strings.Split(a.render(), "\n")
	if len(rows) != testkit.Height {
		t.Fatalf("render() produced %d rows; want exactly testkit.Height (%d) — the bottom-anchored frame must still total a.height", len(rows), testkit.Height)
	}

	rule := strings.Repeat("─", testkit.Width)
	last := len(rows) - 1
	if rows[last] != rule {
		t.Errorf("row %d (closing input rule) = %q; want the full-width rule", last, rows[last])
	}
	if !strings.HasPrefix(rows[last-1], "> ") {
		t.Errorf("row %d (input line) = %q; want it to start with the > prompt", last-1, rows[last-1])
	}
	if rows[last-2] != rule {
		t.Errorf("row %d (opening input rule) = %q; want the full-width rule", last-2, rows[last-2])
	}
	if rows[last-3] != "" {
		t.Errorf("row %d = %q; want blank filler directly above the pinned input block", last-3, rows[last-3])
	}
}

// TestAppStaleEventGuard verifies a sessEventMsg tagged for a session other
// than the one currently attached/peeked is dropped rather than ingested —
// the guard against a previous subscription's in-flight waitForEvent read
// landing after the user has already moved on.
func TestAppStaleEventGuard(t *testing.T) {
	th := theme.Test()
	a := App{theme: th, sess: New(th), sessID: "session-b"}

	mdl, _ := a.Update(sessEventMsg{id: "session-a", ev: event.NewSessionError("session-a", "boom", true)})
	got := mdl.(App)

	if len(got.sess.items) != 0 {
		t.Fatalf("stale event from session-a was ingested into session-b's transcript: %+v", got.sess.items)
	}
}

// attachForDialogTest attaches a into the roster's selected session (mirroring
// TestGoldenAppAttach's opening moves) and returns the resulting App,
// subscribed and ready to receive sessEventMsg directly — the shared setup
// for the approval-prompt tests below.
func attachForDialogTest(t *testing.T, sup *internalFakeSup) App {
	t.Helper()
	a := newAppForGolden(t, sup)
	mdl, cmd := a.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	a = mdl.(App)
	if cmd == nil {
		t.Fatal("expected a subscribe cmd after entering attach")
	}
	mdl, _ = a.Update(cmd())
	return mdl.(App)
}

// requestApproval feeds a into a's currently attached session as a
// PermissionRequested sessEventMsg and returns the resulting App.
func requestApproval(t *testing.T, a App, id string) App {
	t.Helper()
	mdl, _ := a.Update(sessEventMsg{
		id: a.sessID,
		ev: event.NewPermissionRequested(a.sessID, id, "bash", map[string]any{"cmd": "rm -rf /tmp/x"}, []string{"no rule"}),
	})
	return mdl.(App)
}

// pendingRemember reads a's peeked/attached session's pending-approval
// remember toggle, failing the test if nothing is pending — the a.dialog
// stand-in now that the state lives on a.sess.
func pendingRemember(t *testing.T, a App) bool {
	t.Helper()
	_, remember, ok := a.sess.PendingApproval()
	if !ok {
		t.Fatal("expected a pending approval")
	}
	return remember
}

// TestGoldenAppApprovalDialog covers the inline approval prompt rendered
// in-flow above the attach screen's status/input lines for a pending
// event.PermissionRequested.
func TestGoldenAppApprovalDialog(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDialogTest(t, sup)

	a = requestApproval(t, a, "perm-1")
	if !a.sess.HasPendingApproval() {
		t.Fatal("expected a pending approval after PermissionRequested")
	}

	testkit.AssertGolden(t, "app_approval_dialog", a.render())
}

// TestGoldenAppApprovalDialogRememberToggled covers the same inline prompt
// with the remember toggle flipped on via the 'r' key.
func TestGoldenAppApprovalDialogRememberToggled(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDialogTest(t, sup)
	a = requestApproval(t, a, "perm-1")

	mdl, _ := a.Update(tea.KeyPressMsg{Text: "r"})
	a = mdl.(App)
	if !pendingRemember(t, a) {
		t.Fatal("expected remember toggled on after 'r'")
	}

	testkit.AssertGolden(t, "app_approval_dialog_remember", a.render())
}

// TestAppApprovalDialogDismissedOnResolved verifies a matching
// PermissionResolved — another attached client answered first — clears the
// pending approval without this client ever sending a reply of its own.
func TestAppApprovalDialogDismissedOnResolved(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDialogTest(t, sup)
	a = requestApproval(t, a, "perm-1")
	if !a.sess.HasPendingApproval() {
		t.Fatal("expected a pending approval after PermissionRequested")
	}

	mdl, _ := a.Update(sessEventMsg{
		id: a.sessID,
		ev: event.NewPermissionResolved(a.sessID, "perm-1", event.VerdictAllow, ""),
	})
	a = mdl.(App)
	if a.sess.HasPendingApproval() {
		t.Fatal("expected the pending approval cleared after a matching PermissionResolved")
	}
	if len(sup.replies) != 0 {
		t.Errorf("sup.replies = %+v, want none — this client never answered", sup.replies)
	}
}

// TestAppApprovalDialogAllowSendsReply verifies 'a' sends an allow reply via
// Supervisor.Reply and dismisses the pending approval immediately.
func TestAppApprovalDialogAllowSendsReply(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDialogTest(t, sup)
	a = requestApproval(t, a, "perm-1")

	mdl, cmd := a.Update(tea.KeyPressMsg{Text: "a"})
	a = mdl.(App)
	if a.sess.HasPendingApproval() {
		t.Fatal("expected the pending approval cleared immediately on allow")
	}
	if cmd == nil {
		t.Fatal("expected a Reply cmd after 'a'")
	}
	cmd() // execute the Reply Cmd synchronously against the fake

	if len(sup.replies) != 1 {
		t.Fatalf("sup.replies = %+v, want one entry", sup.replies)
	}
	want := replyCall{sessionID: a.sessID, id: "perm-1", allow: true, remember: false}
	if sup.replies[0] != want {
		t.Errorf("sup.replies[0] = %+v, want %+v", sup.replies[0], want)
	}
}

// TestAppApprovalDialogDenyWithRememberSendsReply verifies 'r' (toggle
// remember) then 'd' (deny) sends a deny reply with remember=true.
func TestAppApprovalDialogDenyWithRememberSendsReply(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDialogTest(t, sup)
	a = requestApproval(t, a, "perm-1")

	mdl, _ := a.Update(tea.KeyPressMsg{Text: "r"})
	a = mdl.(App)

	mdl, cmd := a.Update(tea.KeyPressMsg{Text: "d"})
	a = mdl.(App)
	if a.sess.HasPendingApproval() {
		t.Fatal("expected the pending approval cleared immediately on deny")
	}
	if cmd == nil {
		t.Fatal("expected a Reply cmd after 'd'")
	}
	cmd()

	if len(sup.replies) != 1 {
		t.Fatalf("sup.replies = %+v, want one entry", sup.replies)
	}
	want := replyCall{sessionID: a.sessID, id: "perm-1", allow: false, remember: true}
	if sup.replies[0] != want {
		t.Errorf("sup.replies[0] = %+v, want %+v", sup.replies[0], want)
	}
}

// TestAppApprovalDialogEscDismissesWithoutReply verifies esc hides the
// prompt without sending any reply — the underlying request stays pending.
func TestAppApprovalDialogEscDismissesWithoutReply(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := attachForDialogTest(t, sup)
	a = requestApproval(t, a, "perm-1")

	mdl, cmd := a.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	a = mdl.(App)
	if a.sess.HasPendingApproval() {
		t.Fatal("expected the pending approval cleared after esc")
	}
	if cmd != nil {
		t.Error("expected esc to issue no Cmd — no reply is sent")
	}
	if len(sup.replies) != 0 {
		t.Errorf("sup.replies = %+v, want none after esc", sup.replies)
	}
}

// TestAppApprovalDialogHiddenOnOverview verifies render()'s screen guard
// directly: a.sess carrying a pending approval while a.scr is
// screenOverview (unreachable through ordinary key navigation today, since
// a pending approval captures every key while active — see
// handleApprovalKey — but a defensive invariant worth pinning regardless)
// renders no approval prompt.
func TestAppApprovalDialogHiddenOnOverview(t *testing.T) {
	th := theme.Test()
	sess := New(th).Ingest(event.NewPermissionRequested("sess-x", "perm-1", "bash", nil, nil))
	a := App{
		theme:  th,
		over:   NewOverview(th, GoldenMeta()),
		sess:   sess,
		scr:    screenOverview,
		width:  testkit.Width,
		height: testkit.Height,
	}
	if strings.Contains(a.render(), "Allow this tool call?") {
		t.Error("overview render contains the approval prompt; want it hidden outside attach/peek")
	}
}

// TestAppHeaderOnEveryScreen verifies the redesign's global two-line
// identity header ("gofer v<version>" / "<model> · <cwd>", see
// identityHeaderLines/attachHeaderLines) tops every screen it's specified
// for (docs/TUI.md's redesign item 1): the overview (already had its own
// copy pre-redesign), the attach transcript, and a pending approval prompt
// (which replaces the attach input but is still part of the same
// header+transcript region). Peek is deliberately excluded — it already
// composes Overview.Rail's own header (see peek.go), not a second copy.
func TestAppHeaderOnEveryScreen(t *testing.T) {
	const wantHeader = "claude-sonnet-5 · ~/orchestration"

	overview := newAppForGolden(t, newInternalFakeSup(GoldenRoster()))
	if got := overview.render(); !strings.Contains(got, wantHeader) {
		t.Errorf("overview render missing identity header %q:\n%s", wantHeader, got)
	}

	attach := attachForDialogTest(t, newInternalFakeSup(GoldenRoster()))
	if got := attach.render(); !strings.Contains(got, wantHeader) {
		t.Errorf("attach render missing identity header %q:\n%s", wantHeader, got)
	}

	withApproval := requestApproval(t, attach, "perm-1")
	if got := withApproval.render(); !strings.Contains(got, wantHeader) {
		t.Errorf("approval-prompt render missing identity header %q:\n%s", wantHeader, got)
	}
}

// TestHeaderScrollsAwayOnLongTranscript exercises the redesign's scroll-away
// behavior end to end (not just the always-short golden fixtures, none of
// which overflow a normal terminal): the header and a long transcript form
// one scrollable region ([Model.view]'s headerLines+transcriptLines join),
// so tailing to the latest message on a transcript long enough to overflow
// the viewport scrolls the header off the top — while scrolling back
// (a.scroll set high) brings it back into view, same as the oldest
// messages.
func TestHeaderScrollsAwayOnLongTranscript(t *testing.T) {
	meta := GoldenMeta()
	meta.AttachSessionID = "sess-x"
	a := NewApp(theme.Test(), &internalFakeSup{}, meta, GoldenCommandEnv())
	mdl, _ := a.Update(tea.WindowSizeMsg{Width: testkit.Width, Height: testkit.Height})
	a = mdl.(App)

	const wantHeader = "gofer v0.3.0"

	// Enough short user/assistant turns to overflow the transcript's avail
	// rows several times over — testkit.Height (24) leaves far fewer than 40
	// rows for content once the footer is carved out.
	for i := 0; i < 40; i++ {
		mdl, _ = a.Update(sessEventMsg{
			id: "sess-x",
			ev: event.NewMessageFinished("sess-x", event.MessageUser, fmt.Sprintf("turn %d", i)),
		})
		a = mdl.(App)
	}

	if got := a.render(); strings.Contains(got, wantHeader) {
		t.Errorf("tailed render (scroll=0) still shows the header on an overflowing transcript; want it scrolled away:\n%s", got)
	}
	if got := a.render(); !strings.Contains(got, "turn 39") {
		t.Errorf("tailed render missing the latest message:\n%s", got)
	}

	a.scroll = 1_000_000 // clamped internally by scrollTail to the content's start
	if got := a.render(); !strings.Contains(got, wantHeader) {
		t.Errorf("fully scrolled-back render is missing the header; want it back in view at the top of the content:\n%s", got)
	}
}

// TestHandleWheelScrollsAndClampsAtTail verifies mouse-wheel notches move
// a.scroll (wheel-up back into history, wheel-down toward the tail) and that
// wheel-down never drives it negative — [scrollTail] only clamps the upper
// bound (content length), so the lower bound has to hold here.
func TestHandleWheelScrollsAndClampsAtTail(t *testing.T) {
	a := App{scr: screenOverview}

	up := a.handleWheel(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if up.scroll != scrollWheelLines {
		t.Errorf("scroll after one wheel-up = %d; want %d", up.scroll, scrollWheelLines)
	}

	down := up.handleWheel(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if down.scroll != 0 {
		t.Errorf("scroll after wheel-up then wheel-down = %d; want back to 0", down.scroll)
	}

	atTail := a.handleWheel(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if atTail.scroll != 0 {
		t.Errorf("scroll after wheel-down at the tail = %d; want clamped to 0, not negative", atTail.scroll)
	}
}

// TestHandleWheelScrollsOverflowingTranscript is the render-level companion
// to TestHandleWheelScrollsAndClampsAtTail: that test only asserts a.scroll's
// numeric field moves, which would pass even if the wheel-driven offset were
// never actually consumed by render (e.g. scrolling a region other than the
// one that overflows, per this PR's BUG 2 investigation). This test builds a
// transcript long enough to genuinely overflow a real terminal height (same
// setup as TestHeaderScrollsAwayOnLongTranscript) and proves a single wheel
// notch changes the VISIBLE WINDOW of rendered content: the tailed frame
// shows the latest turn, and after one handleWheel(MouseWheelUp) it no
// longer does — an earlier turn is visible instead. This is the actual
// user-observable behavior a working mouse wheel produces; content that
// fits the viewport (no overflow) legitimately has nothing to scroll, so
// that case is deliberately not asserted here.
func TestHandleWheelScrollsOverflowingTranscript(t *testing.T) {
	meta := GoldenMeta()
	meta.AttachSessionID = "sess-x"
	a := NewApp(theme.Test(), &internalFakeSup{}, meta, GoldenCommandEnv())
	mdl, _ := a.Update(tea.WindowSizeMsg{Width: testkit.Width, Height: testkit.Height})
	a = mdl.(App)

	// testkit.Height (24) leaves far fewer rows than 40 turns once the
	// header/footer are carved out — see TestHeaderScrollsAwayOnLongTranscript's
	// doc for the same math.
	for i := 0; i < 40; i++ {
		mdl, _ = a.Update(sessEventMsg{
			id: "sess-x",
			ev: event.NewMessageFinished("sess-x", event.MessageUser, fmt.Sprintf("turn %d", i)),
		})
		a = mdl.(App)
	}

	tailed := a.render()
	if !strings.Contains(tailed, "turn 39") {
		t.Fatalf("precondition failed: tailed render missing the latest turn:\n%s", tailed)
	}

	a = a.handleWheel(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if a.scroll == 0 {
		t.Fatal("precondition failed: handleWheel(up) left a.scroll at 0")
	}

	scrolled := a.render()
	if strings.Contains(scrolled, "turn 39") {
		t.Errorf("one wheel-up notch on overflowing content still shows the latest turn — the visible window did not move:\ntailed:\n%s\nscrolled:\n%s", tailed, scrolled)
	}
	if scrolled == tailed {
		t.Error("render() unchanged after handleWheel(up) on overflowing content — the wheel notch had no visible effect")
	}
}

// TestHandleWheelIgnoredOnPeek verifies the wheel is a no-op on the peek
// screen — item 7 scopes mouse-wheel scrolling to "overview + attach" only;
// peek carries no scrollable transcript of its own (see peek.go).
func TestHandleWheelIgnoredOnPeek(t *testing.T) {
	a := App{scr: screenPeek}
	got := a.handleWheel(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if got.scroll != 0 {
		t.Errorf("scroll after wheel-up on peek = %d; want unchanged (0) — peek has no scrollable content", got.scroll)
	}
}

// TestPgUpPgDownScrollOverviewAndAttach verifies the keyboard pairing for
// mouse-wheel scroll (item 7's "nice pairing"): PgUp/PgDn move a.scroll by
// scrollPageLines on both the overview dispatch bar and the attach input,
// and PgDn floors at 0 the same way wheel-down does.
func TestPgUpPgDownScrollOverviewAndAttach(t *testing.T) {
	overview := newAppForGolden(t, newInternalFakeSup(GoldenRoster()))
	mdl, _ := overview.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	overview = mdl.(App)
	if overview.scroll != scrollPageLines {
		t.Errorf("overview scroll after PgUp = %d; want %d", overview.scroll, scrollPageLines)
	}
	mdl, _ = overview.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	overview = mdl.(App)
	if overview.scroll != 0 {
		t.Errorf("overview scroll after PgUp then PgDn = %d; want back to 0", overview.scroll)
	}
	mdl, _ = overview.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	overview = mdl.(App)
	if overview.scroll != 0 {
		t.Errorf("overview scroll after PgDn at the tail = %d; want clamped to 0", overview.scroll)
	}

	attach := attachForDialogTest(t, newInternalFakeSup(GoldenRoster()))
	mdl, _ = attach.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	attach = mdl.(App)
	if attach.scroll != scrollPageLines {
		t.Errorf("attach scroll after PgUp = %d; want %d", attach.scroll, scrollPageLines)
	}
}

// TestScrollResetsOnScreenAndSessionSwitch verifies a.scroll — a stale
// scroll-back offset would otherwise point at unrelated content — is reset
// to 0 (tail) whenever the screen changes or the attached session switches,
// per App.scroll's doc.
func TestScrollResetsOnScreenAndSessionSwitch(t *testing.T) {
	a := newAppForGolden(t, newInternalFakeSup(GoldenRoster()))
	mdl, _ := a.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	a = mdl.(App)
	if a.scroll == 0 {
		t.Fatal("expected a nonzero scroll after PgUp, precondition for this test")
	}

	mdl, _ = a.Update(tea.KeyPressMsg{Code: tea.KeyRight}) // attach: a different scrollable region
	a = mdl.(App)
	if a.scroll != 0 {
		t.Errorf("scroll not reset entering attach: got %d, want 0", a.scroll)
	}

	mdl, _ = a.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	a = mdl.(App)
	mdl, _ = a.Update(tea.KeyPressMsg{Code: tea.KeyLeft}) // back to overview (input is empty)
	a = mdl.(App)
	if a.scroll != 0 {
		t.Errorf("scroll not reset backing out to overview: got %d, want 0", a.scroll)
	}
}

// TestScrollTailClampsOffset unit-tests scrollTail's bound math directly —
// the shared primitive behind both the attach header+transcript scroll and
// the roster's own scroll, and the thing an underflow regression in this PR
// would actually live in (see TestRenderNoPanicAtTinyHeights's doc).
func TestScrollTailClampsOffset(t *testing.T) {
	lines := []string{"a", "b", "c", "d", "e"}

	if got := scrollTail(lines, 0, 5); got != nil {
		t.Errorf("scrollTail with avail<=0 = %v; want nil", got)
	}
	if got := scrollTail(lines, -3, 5); got != nil {
		t.Errorf("scrollTail with negative avail = %v; want nil", got)
	}
	if got := scrollTail(lines, 10, 0); len(got) != 5 {
		t.Errorf("scrollTail with avail > len(lines) = %v; want all 5 lines unchanged", got)
	}
	if got := scrollTail(lines, 2, 0); fmt.Sprint(got) != "[d e]" {
		t.Errorf("scrollTail tail (offset 0) = %v; want the last 2 lines [d e]", got)
	}
	if got := scrollTail(lines, 2, 1); fmt.Sprint(got) != "[c d]" {
		t.Errorf("scrollTail offset 1 = %v; want [c d]", got)
	}
	if got := scrollTail(lines, 2, 1_000_000); fmt.Sprint(got) != "[a b]" {
		t.Errorf("scrollTail with an oversized offset = %v; want clamped to the start [a b], not an out-of-range slice", got)
	}
	if got := scrollTail(lines, 2, -5); fmt.Sprint(got) != "[d e]" {
		t.Errorf("scrollTail with a negative offset = %v; want clamped to the tail [d e]", got)
	}
}

// TestViewEnablesMouseMode verifies App.View requests cell-motion mouse
// reporting (bubbletea v2 moved this from a tea.NewProgram option onto the
// View — see View's doc) so a terminal that supports it starts sending
// tea.MouseWheelMsg without any extra opt-in from cmd/gofer.
func TestViewEnablesMouseMode(t *testing.T) {
	a := NewApp(theme.Test(), &internalFakeSup{}, GoldenMeta(), GoldenCommandEnv())
	if got := a.View().MouseMode; got != tea.MouseModeCellMotion {
		t.Errorf("View().MouseMode = %v; want tea.MouseModeCellMotion", got)
	}
}
