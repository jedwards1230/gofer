package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/jedwards1230/gofer/internal/tui/testkit"
	"github.com/jedwards1230/gofer/internal/tui/theme"
)

// skewApp builds a seeded overview App whose meta reports the given CLI/daemon
// versions, so a test can put the stale-daemon banner up (or not) and press R.
func skewApp(t *testing.T, sup *internalFakeSup, cliVer, daemonVer string) App {
	t.Helper()
	meta := GoldenMeta()
	meta.Version = cliVer
	meta.DaemonVersion = daemonVer
	a := NewApp(theme.Test(), sup, meta, GoldenCommandEnv())
	mdl, _ := a.Update(tea.WindowSizeMsg{Width: testkit.Width, Height: testkit.Height})
	a = mdl.(App)
	mdl, _ = a.Update(rosterMsg{sessions: GoldenRoster()})
	return mdl.(App)
}

// TestBannerRestartKeyFiresWhenStale is the banner action's core: with the
// stale-daemon banner up and the dispatch bar empty, pressing R restarts the
// daemon (via Supervisor.RestartDaemon) and then re-fetches the roster — the
// visible proof the restart worked, which Part 1 repopulates from the journals.
func TestBannerRestartKeyFiresWhenStale(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := skewApp(t, sup, "v0.3.1", "v0.2.1") // daemon older than CLI ⇒ banner up
	// A real restart redeploys the CLIENT's own build (see
	// [daemonbridge.Supervisor.RestartDaemon]'s doc), so the replacement
	// answers gofer/hello with the CLI's version, not the stale one it
	// started on.
	sup.daemonVersion = "v0.3.1"

	mdl, cmd := a.Update(tea.KeyPressMsg{Text: "R"})
	a = mdl.(App)
	if cmd == nil {
		t.Fatal("pressing R with the stale banner up returned no command, want a restart command")
	}
	// Run the restart command; it must invoke RestartDaemon and report success.
	msg := cmd()
	rm, ok := msg.(daemonRestartMsg)
	if !ok {
		t.Fatalf("restart command produced %T, want daemonRestartMsg", msg)
	}
	if rm.err != nil {
		t.Fatalf("daemonRestartMsg.err = %v, want nil", rm.err)
	}
	if sup.restarts != 1 {
		t.Errorf("RestartDaemon called %d times, want exactly 1", sup.restarts)
	}
	// The success path re-fetches the roster (so the restored sessions show).
	mdl, follow := a.Update(rm)
	a = mdl.(App)
	if follow == nil {
		t.Error("a successful restart did not schedule a roster refresh")
	}
	// The banner must already be gone from THIS render — meta.DaemonVersion is
	// refreshed synchronously in the daemonRestartMsg branch, not deferred
	// until the roster refresh lands (see [App.doRestartDaemon]/
	// [Overview.WithDaemonVersion)).
	if got := a.render(); strings.Contains(got, "daemon is stale") {
		t.Fatalf("stale-daemon banner still rendered after a successful restart:\n%s", got)
	}

	// Run the scheduled roster refresh to completion too, so the whole
	// restart-through-repopulation path is proven, not just the message
	// branch: the banner must stay gone once the roster is back.
	mdl, _ = a.Update(follow())
	a = mdl.(App)
	if got := a.render(); strings.Contains(got, "daemon is stale") {
		t.Fatalf("stale-daemon banner reappeared after the post-restart roster refresh:\n%s", got)
	}
}

// TestBannerRestartKeyInertWhenNotStale guards the footgun: with NO banner (the
// versions match), R is an ordinary character for the dispatch bar, never a
// daemon restart — so a user typing a prompt that starts with R is not
// hijacked.
func TestBannerRestartKeyInertWhenNotStale(t *testing.T) {
	sup := newInternalFakeSup(GoldenRoster())
	a := skewApp(t, sup, "v0.3.1", "v0.3.1") // versions match ⇒ no banner

	mdl, _ := a.Update(tea.KeyPressMsg{Text: "R"})
	a = mdl.(App)
	if sup.restarts != 0 {
		t.Errorf("RestartDaemon called %d times with no banner, want 0", sup.restarts)
	}
	// R landed in the dispatch bar as text rather than triggering a restart.
	if got := a.over.input.String(); got != "R" {
		t.Errorf("dispatch bar = %q after R with no banner, want %q (R typed as text)", got, "R")
	}
}
