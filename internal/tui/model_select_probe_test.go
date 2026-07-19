package tui_test

// model_select_probe_test.go covers issue #162's TUI half: after a /model
// write on a DAEMON-BACKED app, the TUI re-reads the daemon's current default
// off gofer/hello (CommandEnv.DaemonDefaultModel) and uses the answer to
// refresh the roster header AND replace the hedged "adopts it unless pinned"
// note with a definitive one — all in the running process, with no restart.
//
// These drive the real App through its exported Update/View surface: the
// select is a real Enter key press, and `press` executes the tea.Cmd
// handleModelSelect returns and feeds the resulting message back into Update
// — the exact sequence the bubbletea runtime performs. The probe is an
// in-process closure, so no network is involved at any point.

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/config"
	"github.com/jedwards1230/gofer/internal/tui"
)

// probingDaemonEnv is daemonModelSelectEnv plus a stub gofer/hello probe
// answering daemonDefault — what cmd/gofer wires to the real
// daemon.Client.Hello on the daemon backend. calls counts the probes so a test
// can prove one actually happened rather than inferring it from a value that
// might have arrived some other way.
func probingDaemonEnv(saved *[]config.Config, daemonDefault string, calls *int) tui.CommandEnv {
	env := daemonModelSelectEnv(saved)
	env.DaemonDefaultModel = func(context.Context) (string, error) {
		*calls++
		return daemonDefault, nil
	}
	return env
}

// TestModelSelectDaemonAdoptedRefreshesHeaderInProcess is the headline
// regression gate for issue #162. Before the probe, OverviewMeta.Model was a
// startup snapshot the daemon path explicitly refused to update, so a user who
// changed the default watched the header keep showing the OLD model
// indefinitely and reasonably concluded the change had failed. The acceptance
// bar is literally "it updates in memory without requiring a restart": one
// process, one uninterrupted message sequence, new model on screen.
func TestModelSelectDaemonAdoptedRefreshesHeaderInProcess(t *testing.T) {
	var saved []config.Config
	var probes int
	sup := newFakeSup(modelSelectRoster())
	// An UNPINNED daemon: it re-reads its default per session/new, so by the
	// time we ask it back it reports exactly what was just written.
	m := newModelSelectApp(t, sup, probingDaemonEnv(&saved, "claude-haiku-4-5", &probes))

	if got := content(m); !strings.Contains(got, "claude-sonnet-5 ·") {
		t.Fatalf("test premise broken: expected the header to start on the daemon's claude-sonnet-5, got:\n%s", got)
	}

	m = dispatchSlash(t, m, "/model")
	m = pressDown(t, m, pressesToHaiku)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if probes != 1 {
		t.Fatalf("gofer/hello probes = %d, want exactly 1 after a committed /model write", probes)
	}
	got := content(m)
	if !strings.Contains(got, "claude-haiku-4-5 ·") {
		t.Fatalf("expected the header to show the daemon's NEW default with no restart, got:\n%s", got)
	}
	if strings.Contains(got, "claude-sonnet-5 ·") {
		t.Fatalf("expected the startup-cached default to be gone from the header, got:\n%s", got)
	}
	const want = "Default model saved; the attached daemon adopted it."
	if !strings.Contains(got, want) {
		t.Fatalf("expected a definitive adopted note, got:\n%s", got)
	}
	assertStatusFitsWidth(t, got, want)
}

// TestModelSelectDaemonPinnedReportsPinnedAndShowsTheDaemonsModel covers the
// other answer the probe can give: a daemon started with an explicit --model
// stays on it for its lifetime, so the write reaches only future daemons.
//
// The header still MOVES — to the daemon's pinned model. That is the truth
// about what its sessions run, and it is the whole reason the header is worth
// refreshing at all: leaving the selected-but-unused id there would be the
// same overclaim the old wording was, just in a different place.
func TestModelSelectDaemonPinnedReportsPinnedAndShowsTheDaemonsModel(t *testing.T) {
	var saved []config.Config
	var probes int
	sup := newFakeSup(modelSelectRoster())
	// Pinned to a model that is neither the header's startup value nor the one
	// being selected, so the assertion cannot pass by coincidence.
	m := newModelSelectApp(t, sup, probingDaemonEnv(&saved, "claude-opus-4-8", &probes))

	m = dispatchSlash(t, m, "/model")
	m = pressDown(t, m, pressesToHaiku) // selects claude-haiku-4-5
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if probes != 1 {
		t.Fatalf("gofer/hello probes = %d, want exactly 1", probes)
	}
	// The write still happens — it is what a FUTURE daemon reads.
	if len(saved) != 1 || saved[0].Session.Model != "claude-haiku-4-5" {
		t.Fatalf("SaveConfig calls = %v; want the default still persisted for future daemons", saved)
	}
	got := content(m)
	if !strings.Contains(got, "claude-opus-4-8 ·") {
		t.Fatalf("expected the header to show the DAEMON's pinned model, got:\n%s", got)
	}
	if strings.Contains(got, "claude-haiku-4-5 ·") {
		t.Fatalf("expected the header NOT to show a model the pinned daemon will never run, got:\n%s", got)
	}
	const want = "Default saved; the attached daemon is pinned to another model."
	if !strings.Contains(got, want) {
		t.Fatalf("expected a definitive pinned note, got:\n%s", got)
	}
	assertStatusFitsWidth(t, got, want)
}

// TestModelSelectDaemonProbeFailureKeepsTheHedgedNote pins the fallback the
// brief calls for: an older daemon (gofer/hello unsupported — cmd/gofer maps
// that to the same unknown answer) or an unreachable one must degrade to the
// pre-probe wording, never to an error and never to a definitive claim the TUI
// cannot back up. The header stays put for the same reason: an unknown answer
// is not evidence the daemon changed.
func TestModelSelectDaemonProbeFailureKeepsTheHedgedNote(t *testing.T) {
	var saved []config.Config
	env := daemonModelSelectEnv(&saved)
	env.DaemonDefaultModel = func(context.Context) (string, error) {
		return "", errors.New("dial tcp: connection refused")
	}
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, env)

	m = dispatchSlash(t, m, "/model")
	m = pressDown(t, m, pressesToHaiku)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	got := content(m)
	if !strings.Contains(got, "Default saved; attached daemon adopts it unless pinned.") {
		t.Fatalf("expected the hedged no-probe note to stand when the probe cannot answer, got:\n%s", got)
	}
	if strings.Contains(got, "connection refused") {
		t.Fatalf("a failed header probe must never surface as an error the user has to act on, got:\n%s", got)
	}
	if !strings.Contains(got, "claude-sonnet-5 ·") {
		t.Fatalf("expected the header to stay put on an unknown answer, got:\n%s", got)
	}
}

// TestModelSelectDaemonHotSwapProbesAfterTheSwap covers the attached-session
// path, where the select does TWO things over the wire: hot-swap the running
// session, then re-read the default. Both must happen, in that order, and the
// note must end up describing both halves definitively.
func TestModelSelectDaemonHotSwapProbesAfterTheSwap(t *testing.T) {
	var saved []config.Config
	var probes int
	sup := newFakeSup(modelSelectRoster())
	m := newModelSelectApp(t, sup, probingDaemonEnv(&saved, "claude-haiku-4-5", &probes))

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight}) // attach the selected (sonnet) session
	m = dispatchSlash(t, m, "/model")
	m = pressDown(t, m, pressesToHaiku)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	wantOp := "set-model:0192a1b2-app0-7000-8000-000000000001:claude-haiku-4-5"
	if len(sup.ops) != 1 || sup.ops[0] != wantOp {
		t.Fatalf("sup.ops = %v; want one entry %q — the live swap must still cross the wire", sup.ops, wantOp)
	}
	if probes != 1 {
		t.Fatalf("gofer/hello probes = %d, want exactly 1 (sequenced after the swap)", probes)
	}
	got := content(m)
	const want = "Model set for this session; the daemon took the new default."
	if !strings.Contains(got, want) {
		t.Fatalf("expected the note to report BOTH the live swap and the adopted default, got:\n%s", got)
	}
	assertStatusFitsWidth(t, got, want)
}

// TestModelSelectDaemonHotSwapFailureSkipsTheProbe pins the precedence issue
// #161 asks for: opDoneMsg's error path is the ONLY route to danger for an op
// result, so a failed swap must be the note the user reads. Letting the probe
// run and overwrite it with a cheerful "the daemon took the new default" would
// report success for an operation that failed.
func TestModelSelectDaemonHotSwapFailureSkipsTheProbe(t *testing.T) {
	var saved []config.Config
	var probes int
	sup := newFakeSup(modelSelectRoster())
	sup.setModelErr = errors.New("gofer/set_model: session busy")
	m := newModelSelectApp(t, sup, probingDaemonEnv(&saved, "claude-haiku-4-5", &probes))

	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyRight})
	m = dispatchSlash(t, m, "/model")
	m = pressDown(t, m, pressesToHaiku)
	m = press(t, m, tea.KeyPressMsg{Code: tea.KeyEnter})

	if probes != 0 {
		t.Fatalf("gofer/hello probes = %d, want 0 — a failed swap is the whole story", probes)
	}
	if got := content(m); !strings.Contains(got, "session busy") {
		t.Fatalf("expected the swap error to be the visible note, got:\n%s", got)
	}
}
