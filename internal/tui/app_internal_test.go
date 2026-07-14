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
	"strings"
	"sync"
	"testing"
	"time"

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

// replyCall records one Supervisor.Reply invocation for the dialog's
// behavioral tests to assert against.
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

func (f *internalFakeSup) Send(context.Context, string, string) error { return nil }
func (f *internalFakeSup) Interrupt(context.Context, string) error    { return nil }
func (f *internalFakeSup) Kill(context.Context, string) error         { return nil }
func (f *internalFakeSup) Archive(context.Context, string) error      { return nil }

func (f *internalFakeSup) Reply(_ context.Context, sessionID, id string, allow, remember bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = append(f.replies, replyCall{sessionID: sessionID, id: id, allow: allow, remember: remember})
	return nil
}

var appGoldenNow = time.Date(2026, 7, 12, 18, 0, 0, 0, time.UTC)

func appGoldenMeta() OverviewMeta {
	return OverviewMeta{App: "gofer", Version: "0.2.0", Model: "fable-5", Cwd: "~/orchestration", Now: appGoldenNow}
}

func appGoldenRoster() []SessionInfo {
	return []SessionInfo{
		{
			ID:      "0192a1b2-app0-7000-8000-000000000001",
			Title:   "wire the app root",
			Summary: "overview <-> peek <-> attach nav",
			Status:  StatusWorking,
			Cost:    provider.Cost{USD: 0.1120},
			Updated: appGoldenNow.Add(-2 * time.Minute),
		},
		{
			ID:      "0192a1b2-app0-7000-8000-000000000002",
			Title:   "review the supervisor contract",
			Summary: "turn finished — awaiting the next prompt",
			Status:  StatusNeedsInput,
			Cost:    provider.Cost{USD: 0.0450},
			Updated: appGoldenNow.Add(-5 * time.Minute),
		},
	}
}

// newAppForGolden builds an App wired to a fresh internalFakeSup, sized and
// with the roster seeded via a real Update(rosterMsg{...}) round trip.
func newAppForGolden(t *testing.T, sup *internalFakeSup) App {
	t.Helper()
	a := NewApp(theme.Test(), sup, appGoldenMeta())

	mdl, _ := a.Update(tea.WindowSizeMsg{Width: testkit.Width, Height: testkit.Height})
	a = mdl.(App)

	mdl, _ = a.Update(rosterMsg{sessions: appGoldenRoster()})
	return mdl.(App)
}

// TestGoldenAppOverview renders the freshly seeded roster screen — App's
// default screen after the first roster fetch resolves.
func TestGoldenAppOverview(t *testing.T) {
	a := newAppForGolden(t, newInternalFakeSup(appGoldenRoster()))
	testkit.AssertGolden(t, "app_overview", a.render())
}

// TestGoldenAppPeek reaches the peek screen by pressing enter on the
// (recency-first) selected session, resolves the subscribe round trip, then
// feeds a few session events directly to populate the tail before
// rendering.
func TestGoldenAppPeek(t *testing.T) {
	a := newAppForGolden(t, newInternalFakeSup(appGoldenRoster()))

	mdl, cmd := a.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	a = mdl.(App)
	if a.scr != screenPeek {
		t.Fatalf("scr = %v; want screenPeek", a.scr)
	}
	if cmd == nil {
		t.Fatal("expected a subscribe cmd after entering peek")
	}
	mdl, _ = a.Update(cmd())
	a = mdl.(App)
	if a.sub == nil {
		t.Fatal("expected a.sub set after subReadyMsg")
	}

	for _, ev := range appTranscriptEvents(a.sessID) {
		mdl, _ = a.Update(sessEventMsg{id: a.sessID, ev: ev})
		a = mdl.(App)
	}

	testkit.AssertGolden(t, "app_peek", a.render())
}

// TestGoldenAppAttach reaches the attach screen by pressing → on the
// selected session, resolves the subscribe round trip, feeds the same
// transcript, and types a pending reply into the input line before
// rendering.
func TestGoldenAppAttach(t *testing.T) {
	a := newAppForGolden(t, newInternalFakeSup(appGoldenRoster()))

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
	a := NewApp(theme.Test(), &internalFakeSup{}, OverviewMeta{AttachSessionID: "sess-x"})
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
// for the approval-dialog tests below.
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

// TestGoldenAppApprovalDialog covers the interactive approval modal
// overlaying the attach screen for a pending event.PermissionRequested.
func TestGoldenAppApprovalDialog(t *testing.T) {
	sup := newInternalFakeSup(appGoldenRoster())
	a := attachForDialogTest(t, sup)

	a = requestApproval(t, a, "perm-1")
	if a.dialog == nil {
		t.Fatal("expected a.dialog set after PermissionRequested")
	}

	testkit.AssertGolden(t, "app_approval_dialog", a.render())
}

// TestGoldenAppApprovalDialogRememberToggled covers the same modal with the
// remember toggle flipped on via the 'r' key.
func TestGoldenAppApprovalDialogRememberToggled(t *testing.T) {
	sup := newInternalFakeSup(appGoldenRoster())
	a := attachForDialogTest(t, sup)
	a = requestApproval(t, a, "perm-1")

	mdl, _ := a.Update(tea.KeyPressMsg{Text: "r"})
	a = mdl.(App)
	if a.dialog == nil || !a.dialog.remember {
		t.Fatal("expected remember toggled on after 'r'")
	}

	testkit.AssertGolden(t, "app_approval_dialog_remember", a.render())
}

// TestAppApprovalDialogDismissedOnResolved verifies a matching
// PermissionResolved — another attached client answered first — clears the
// dialog without this client ever sending a reply of its own.
func TestAppApprovalDialogDismissedOnResolved(t *testing.T) {
	sup := newInternalFakeSup(appGoldenRoster())
	a := attachForDialogTest(t, sup)
	a = requestApproval(t, a, "perm-1")
	if a.dialog == nil {
		t.Fatal("expected a.dialog set after PermissionRequested")
	}

	mdl, _ := a.Update(sessEventMsg{
		id: a.sessID,
		ev: event.NewPermissionResolved(a.sessID, "perm-1", event.VerdictAllow, ""),
	})
	a = mdl.(App)
	if a.dialog != nil {
		t.Fatal("expected a.dialog cleared after a matching PermissionResolved")
	}
	if len(sup.replies) != 0 {
		t.Errorf("sup.replies = %+v, want none — this client never answered", sup.replies)
	}
}

// TestAppApprovalDialogAllowSendsReply verifies 'a' sends an allow reply via
// Supervisor.Reply and dismisses the dialog immediately.
func TestAppApprovalDialogAllowSendsReply(t *testing.T) {
	sup := newInternalFakeSup(appGoldenRoster())
	a := attachForDialogTest(t, sup)
	a = requestApproval(t, a, "perm-1")

	mdl, cmd := a.Update(tea.KeyPressMsg{Text: "a"})
	a = mdl.(App)
	if a.dialog != nil {
		t.Fatal("expected a.dialog cleared immediately on allow")
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
	sup := newInternalFakeSup(appGoldenRoster())
	a := attachForDialogTest(t, sup)
	a = requestApproval(t, a, "perm-1")

	mdl, _ := a.Update(tea.KeyPressMsg{Text: "r"})
	a = mdl.(App)

	mdl, cmd := a.Update(tea.KeyPressMsg{Text: "d"})
	a = mdl.(App)
	if a.dialog != nil {
		t.Fatal("expected a.dialog cleared immediately on deny")
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
// dialog without sending any reply — the underlying request stays pending.
func TestAppApprovalDialogEscDismissesWithoutReply(t *testing.T) {
	sup := newInternalFakeSup(appGoldenRoster())
	a := attachForDialogTest(t, sup)
	a = requestApproval(t, a, "perm-1")

	mdl, cmd := a.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	a = mdl.(App)
	if a.dialog != nil {
		t.Fatal("expected a.dialog cleared after esc")
	}
	if cmd != nil {
		t.Error("expected esc to issue no Cmd — no reply is sent")
	}
	if len(sup.replies) != 0 {
		t.Errorf("sup.replies = %+v, want none after esc", sup.replies)
	}
}

// TestAppApprovalDialogHiddenOnOverview verifies render()'s screen guard
// directly: a.dialog set while a.scr is screenOverview (unreachable through
// ordinary key navigation today, since the dialog captures every key while
// active — see handleDialogKey — but a defensive invariant worth pinning
// regardless) renders no dialog box.
func TestAppApprovalDialogHiddenOnOverview(t *testing.T) {
	th := theme.Test()
	a := App{
		theme:  th,
		over:   NewOverview(th, appGoldenMeta()),
		sess:   New(th),
		scr:    screenOverview,
		width:  testkit.Width,
		height: testkit.Height,
		dialog: &approval{sessionID: "sess-x", id: "perm-1", tool: "bash"},
	}
	if strings.Contains(a.render(), "Permission requested") {
		t.Error("overview render contains the dialog box; want it hidden outside attach/peek")
	}
}
