package tui_test

// app_test.go drives App entirely through its exported tea.Model surface
// (Init/Update/View) — the fake Supervisor plus the navigation-contract
// behavioral tests live here. Anything that needs App's unexported
// messages or fields (golden renders of peek/attach, the stale-event
// guard) lives in app_internal_test.go (package tui) instead.

import (
	"context"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/agent-sdk-go/acp"
	"github.com/jedwards1230/agent-sdk-go/event"

	"github.com/jedwards1230/gofer/internal/decision"
	"github.com/jedwards1230/gofer/internal/tui"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// fakeSup is a Supervisor backed by real event.Brokers, so App's
// subscribe/waitForEvent plumbing exercises a genuine channel instead of a
// canned response. Create/Send/Interrupt/Kill/Archive record what they were
// called with, for the behavioral tests to assert against.
type fakeSup struct {
	mu      sync.Mutex
	roster  []tui.SessionInfo
	brokers map[string]*event.Broker

	created    []string
	createdCwd []string
	sent       []string
	ops        []string

	// setModelErr, when non-nil, is what SetModel returns — the failed-op path
	// a test needs to prove opDoneMsg's error still wins over anything the
	// /model select would otherwise report (see model_select_probe_test.go).
	// The call is still recorded in ops either way.
	setModelErr error

	// listed/listErr are what ListSessions answers with, and resumeErr what
	// Resume returns — the /resume picker's list, its load-failure path, and
	// the unknown-session path respectively. The Resume call is recorded in ops
	// either way.
	listed    []tui.SessionRef
	listErr   error
	resumeErr error
	listN     int

	// setEffortErr is setModelErr's effort-axis twin: what SetEffort returns,
	// for the failed-op path. The call is still recorded in ops either way.
	setEffortErr error
}

// createdPrompts, sentPrompts, recordedOps, and listCalls read the recorded
// call log under the mutex — the assertion surface for tests that drive
// Supervisor ops asynchronously (through a tea.Cmd resolved on another
// goroutine's behalf) rather than touching the fields directly.
func (f *fakeSup) createdPrompts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.created...)
}

func (f *fakeSup) sentPrompts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

func (f *fakeSup) recordedOps() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ops...)
}

func (f *fakeSup) listCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listN
}

func newFakeSup(roster []tui.SessionInfo) *fakeSup {
	return &fakeSup{roster: roster, brokers: map[string]*event.Broker{}}
}

func (f *fakeSup) broker(id string) *event.Broker {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.brokers[id]
	if !ok {
		b = event.NewBroker()
		f.brokers[id] = b
	}
	return b
}

func (f *fakeSup) Roster(context.Context) ([]tui.SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tui.SessionInfo(nil), f.roster...), nil
}

func (f *fakeSup) Subscribe(_ context.Context, id string) (*event.Subscription, error) {
	return f.broker(id).Subscribe(event.FilterAll, 16), nil
}

func (f *fakeSup) Create(_ context.Context, prompt string, opts tui.CreateOptions) (tui.SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, prompt)
	f.createdCwd = append(f.createdCwd, opts.Cwd)
	info := tui.SessionInfo{ID: "created-1", Title: prompt, Status: tui.StatusWorking}
	f.roster = append(f.roster, info)
	return info, nil
}

// ListSessions returns the canned resumable-session list the /resume picker
// renders. It is nil by default, so a test that never touches /resume sees the
// honest "no sessions on disk" state rather than a fabricated one.
func (f *fakeSup) ListSessions(context.Context) ([]tui.SessionRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listN++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]tui.SessionRef(nil), f.listed...), nil
}

// Resume records the call (id and cwd, so a test can assert BOTH reached the
// supervisor) and returns resumeErr — the unknown-session path /resume's error
// reporting is built on.
func (f *fakeSup) Resume(_ context.Context, id, cwd string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "resume:"+id+":"+cwd)
	return f.resumeErr
}

func (f *fakeSup) Send(_ context.Context, id, prompt string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, id+":"+prompt)
	return nil
}

func (f *fakeSup) Interrupt(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "interrupt:"+id)
	return nil
}

func (f *fakeSup) Kill(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "kill:"+id)
	return nil
}

func (f *fakeSup) Archive(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "archive:"+id)
	return nil
}

func (f *fakeSup) SetModel(_ context.Context, id, model string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "set-model:"+id+":"+model)
	return f.setModelErr
}

// SetEffort records the call the same way SetModel does. The effort is
// recorded verbatim, so the CLEAR call ("") is distinguishable from no call at
// all — it shows up as a trailing-colon "set-effort:<id>:" entry rather than
// being invisible.
func (f *fakeSup) SetEffort(_ context.Context, id, effort string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, "set-effort:"+id+":"+effort)
	return f.setEffortErr
}

// Reply is a no-op here: the approval prompt it answers needs the unexported
// sessEventMsg to trigger (see app_internal_test.go, package tui, for the
// behavioral Reply-emission tests), which this package (tui_test) has no
// access to.
func (f *fakeSup) Reply(_ context.Context, _, _ string, _ tui.PermissionDecision) error { return nil }

// ExplainPermission answers with an empty rationale: this package's black-box
// tests drive navigation, not the approval prompt's ctrl+e (which
// app_internal_test.go covers against a recording fake).
func (f *fakeSup) ExplainPermission(_ context.Context, _, _ string) (acp.PermissionRationale, error) {
	return acp.PermissionRationale{}, nil
}

// Decisions hands back an already-closed subscription: the decision prompt's
// behavioral tests need a real gate and the unexported decision messages, so
// they live in app_internal_test.go (package tui) like the approval ones. A
// closed stream keeps App's decision pump a no-op here.
func (f *fakeSup) Decisions(context.Context, string) (*decision.Subscription, error) {
	sub := decision.NewGate("").Subscribe(0)
	sub.Close()
	return sub, nil
}

// AnswerDecision is a no-op here for the same reason as Reply above.
func (f *fakeSup) AnswerDecision(context.Context, string, string, []acp.DecisionAnswer) error {
	return nil
}

// content renders m the way a real frame would, returning just the string
// content for substring assertions.
func content(m tea.Model) string {
	return m.View().Content
}

// newTestApp builds an App over sup, sizes it, and drives Init's roster
// fetch to completion through the exported Update surface only — the
// fetchRoster Cmd's resulting Msg is opaque to this package (its
// concrete type is unexported), but Update accepts it all the same.
func newTestApp(t *testing.T, sup tui.Supervisor) tea.Model {
	t.Helper()
	var m tea.Model = tui.NewApp(theme.Test(), sup, tui.GoldenMeta(), tui.GoldenCommandEnv())
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	cmd := m.Init()
	if cmd == nil {
		t.Fatal("Init() returned a nil Cmd; expected the roster fetch")
	}
	m, _ = m.Update(cmd())
	return m
}

// press drives one key through Update and, if it returns a Cmd, executes it
// immediately and feeds the resulting Msg back in — the synchronous stand-in
// for bubbletea's own runtime loop, safe here because every Cmd App issues
// off a key press (subscribe, create, send, interrupt, kill, archive) either
// resolves immediately against the fake Supervisor or is a follow-on
// waitForEvent read this helper deliberately does not chase (it would block
// forever with no event pending).
func press(t *testing.T, m tea.Model, key tea.KeyPressMsg) tea.Model {
	t.Helper()
	m, cmd := m.Update(key)
	if cmd == nil {
		return m
	}
	m, _ = m.Update(cmd())
	return m
}

// type_ types s into whichever screen's input is focused, one rune per key
// press.
func type_(t *testing.T, m tea.Model, s string) tea.Model {
	t.Helper()
	for _, r := range s {
		m = press(t, m, tea.KeyPressMsg{Text: string(r)})
	}
	return m
}

const ctrl = tea.ModCtrl

// TestNavEnterPeeksSelected verifies enter, with an empty dispatch input,
// peeks the selected session rather than dispatching a new one.
func TestNavEnterPeeksSelected(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if got := content(m); !strings.Contains(got, "space to close") {
		t.Fatalf("expected the peek screen after enter, got:\n%s", got)
	}
}

// TestNavPeekSpaceClosesToOverview verifies space, with an empty reply
// buffer, closes peek back to the overview.
func TestNavPeekSpaceClosesToOverview(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // enter peek
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeySpace}) // empty reply: close

	if got := content(m); !strings.Contains(got, "enter peek") {
		t.Fatalf("expected space with an empty reply to back out to the overview, got:\n%s", got)
	}
}

// TestNavPeekEnterAttaches verifies enter, with an empty reply buffer,
// attaches the peeked session.
func TestNavPeekEnterAttaches(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // enter peek
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // empty reply: attach

	if got := content(m); !strings.Contains(got, "> ▏") {
		t.Fatalf("expected enter with an empty reply to attach, got:\n%s", got)
	}
}

// TestNavPeekReplySends verifies typing a reply on the peek card and
// pressing enter sends it via Supervisor.Send and stays on peek.
func TestNavPeekReplySends(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := newTestApp(t, sup)

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // enter peek
	m = type_(t, m, "status?")
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // send the reply

	want := "0192a1b2-app0-7000-8000-000000000001:status?"
	if len(sup.sent) != 1 || sup.sent[0] != want {
		t.Fatalf("sup.sent = %v; want one entry %q", sup.sent, want)
	}
	if got := content(m); !strings.Contains(got, "space to close") {
		t.Fatalf("expected to stay on the peek screen after sending a reply, got:\n%s", got)
	}
}

// TestNavPeekKillsSelected verifies ctrl-x on the peek screen kills the
// selected (non-finished) session.
func TestNavPeekKillsSelected(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := newTestApp(t, sup)

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter}) // enter peek
	press(t, m, tea.KeyPressMsg{Code: 'x', Mod: ctrl})

	want := "kill:0192a1b2-app0-7000-8000-000000000001"
	if len(sup.ops) != 1 || sup.ops[0] != want {
		t.Fatalf("sup.ops = %v; want one entry %q", sup.ops, want)
	}
}

// TestNavRightAttachesSelected verifies → attaches the selected session
// directly, skipping peek.
func TestNavRightAttachesSelected(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight})

	if got := content(m); !strings.Contains(got, "> ▏") {
		t.Fatalf("expected the attach screen (empty input line) after →, got:\n%s", got)
	}
}

// TestNavTabTogglesView verifies tab flips the roster from the flat to the
// grouped ordering, surfacing its section headers.
func TestNavTabTogglesView(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyTab})

	if got := content(m); !strings.Contains(got, "Working") || !strings.Contains(got, "Needs input") {
		t.Fatalf("expected grouped-view section headers after tab, got:\n%s", got)
	}
}

// TestNavDispatchCreatesSession verifies typing into the dispatch bar and
// pressing enter dispatches the prompt to Supervisor.Create.
func TestNavDispatchCreatesSession(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := newTestApp(t, sup)

	m = type_(t, m, "fix the flaky peek test")
	press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if len(sup.created) != 1 || sup.created[0] != "fix the flaky peek test" {
		t.Fatalf("sup.created = %v; want one entry %q", sup.created, "fix the flaky peek test")
	}
	// The dispatch bar must pass this client's cwd (the roster header's value,
	// tui.GoldenMeta().Cwd) so the created session carries the client's project
	// dir — not the daemon's launch dir — and stays visible to a cwd-filtered
	// client (e.g. a phone).
	if len(sup.createdCwd) != 1 || sup.createdCwd[0] != tui.GoldenMeta().Cwd {
		t.Fatalf("created cwd = %v; want one entry %q (the App's cwd)", sup.createdCwd, tui.GoldenMeta().Cwd)
	}
}

// TestNavAttachSendsPrompt verifies typing into the attach input and
// pressing enter sends the prompt to Supervisor.Send for the attached
// session.
func TestNavAttachSendsPrompt(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := newTestApp(t, sup)

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach the selected (working) session
	m = type_(t, m, "status?")
	press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	want := "0192a1b2-app0-7000-8000-000000000001:status?"
	if len(sup.sent) != 1 || sup.sent[0] != want {
		t.Fatalf("sup.sent = %v; want one entry %q", sup.sent, want)
	}
}

// TestNavKillWorkingSession verifies ctrl-x on a working (non-finished)
// session kills it rather than archiving it.
func TestNavKillWorkingSession(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	m := newTestApp(t, sup)

	press(t, m, tea.KeyPressMsg{Code: 'x', Mod: ctrl})

	want := "kill:0192a1b2-app0-7000-8000-000000000001"
	if len(sup.ops) != 1 || sup.ops[0] != want {
		t.Fatalf("sup.ops = %v; want one entry %q", sup.ops, want)
	}
}

// TestAttachOpenStartsOnAttach verifies OverviewMeta.AttachSessionID opens the
// app directly on the attach screen (the `gofer attach <id>` entry point), and
// that ← still backs out to the overview from there.
func TestAttachOpenStartsOnAttach(t *testing.T) {
	sup := newFakeSup(tui.GoldenRoster())
	meta := tui.GoldenMeta()
	meta.AttachSessionID = "0192a1b2-app0-7000-8000-000000000002"

	var m tea.Model = tui.NewApp(theme.Test(), sup, meta, tui.GoldenCommandEnv())
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	if got := content(m); !strings.Contains(got, "> ▏") || strings.Contains(got, "enter peek") {
		t.Fatalf("expected the attach screen on open, got:\n%s", got)
	}
	if cmd := tui.NewApp(theme.Test(), sup, meta, tui.GoldenCommandEnv()).Init(); cmd == nil {
		t.Error("Init with AttachSessionID returned nil cmd; expected the roster+subscribe batch")
	}

	m, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	if got := content(m); !strings.Contains(got, "enter peek") {
		t.Fatalf("expected ← to back out to the overview, got:\n%s", got)
	}
}

// TestNavAttachLeftBacksOutWhenEmpty verifies ← in the attach screen backs
// out to the overview only when the input buffer is empty.
func TestNavAttachLeftBacksOutWhenEmpty(t *testing.T) {
	m := newTestApp(t, newFakeSup(tui.GoldenRoster()))
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach

	if got := content(m); !strings.Contains(got, "> ▏") {
		t.Fatalf("expected the attach screen before ←, got:\n%s", got)
	}

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyLeft})

	if got := content(m); !strings.Contains(got, "enter peek") {
		t.Fatalf("expected ← with an empty input to back out to the overview, got:\n%s", got)
	}
}
