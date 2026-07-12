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

func (f *internalFakeSup) Create(_ context.Context, prompt string) (SessionInfo, error) {
	return SessionInfo{ID: "created-1", Title: prompt, Status: StatusWorking}, nil
}

func (f *internalFakeSup) Send(context.Context, string, string) error { return nil }
func (f *internalFakeSup) Interrupt(context.Context, string) error    { return nil }
func (f *internalFakeSup) Kill(context.Context, string) error         { return nil }
func (f *internalFakeSup) Archive(context.Context, string) error      { return nil }

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
// goldens so both show the same populated transcript.
func appTranscriptEvents(sid string) []event.Event {
	return []event.Event{
		event.NewTurnStarted(sid),
		event.NewMessageStarted(sid, event.MessageText),
		event.NewMessageFinished(sid, event.MessageText, "Wired the app root; nav contract is in."),
		event.NewTurnFinished(sid, "end_turn", provider.Usage{InputTokens: 20, OutputTokens: 11}),
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
